// Package store es la única capa que habla con PostgreSQL.
package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AndyPecotche/wg-relay/internal/auth"
	"github.com/AndyPecotche/wg-relay/internal/proto"
)

var (
	ErrNotFound     = errors.New("no encontrado")
	ErrUnauthorized = errors.New("token inválido o revocado")
	ErrLeaseHeld    = errors.New("otra instancia del agente tiene el tunnel")
	ErrSuperseded   = errors.New("la instancia perdió el lease")
	ErrPubkeyInUse  = errors.New("clave pública WireGuard ya registrada")
)

var (
	clientPool = netip.MustParsePrefix("10.64.0.0/10")
	//go:embed migrations/*.sql
	migrations embed.FS
)

type Store struct{ db *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	// Postgres puede tardar en aceptar conexiones al levantar el compose.
	for i := 0; ; i++ {
		if err = db.Ping(ctx); err == nil {
			return &Store{db: db}, nil
		}
		if i == 30 {
			db.Close()
			return nil, fmt.Errorf("conectando a postgres: %w", err)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func (s *Store) Close() { s.db.Close() }

// Migrate aplica en orden las migraciones pendientes. Usa un advisory lock
// para que varias réplicas del control plane puedan arrancar a la vez.
func (s *Store) Migrate(ctx context.Context) error {
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(727274)`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, _ := migrations.ReadFile(name)
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migración %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ---------------------------------------------------------------- tunnels

type Tunnel struct {
	ID        int64
	Email     string
	Plan      string
	Subdomain string
	VPNIP     string
	Online    bool
}

// CreateTunnel da de alta un tunnel (creando la cuenta si no existe) y
// devuelve su token. El token solo se conoce en este momento.
func (s *Store) CreateTunnel(ctx context.Context, email, plan string) (Tunnel, auth.Token, error) {
	tok := auth.New(auth.PrefixAgent)
	t := Tunnel{Email: email, Plan: plan}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var accountID int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO account(email, plan) VALUES ($1, $2)
			ON CONFLICT (email) DO UPDATE SET email = EXCLUDED.email
			RETURNING id, plan`, email, plan).Scan(&accountID, &t.Plan); err != nil {
			return err
		}
		var off int64
		if err := tx.QueryRow(ctx, `SELECT nextval('tunnel_ip_seq')`).Scan(&off); err != nil {
			return err
		}
		ip, err := offsetAddr(clientPool, off)
		if err != nil {
			return err
		}
		t.VPNIP = ip.String()
		// Reintenta ante la (improbable) colisión de subdominio.
		for range 5 {
			t.Subdomain = auth.Random(5)[:7]
			err = tx.QueryRow(ctx, `
				INSERT INTO tunnel(account_id, subdomain, vpn_ip) VALUES ($1, $2, $3::inet)
				ON CONFLICT (subdomain) DO NOTHING RETURNING id`, accountID, t.Subdomain, t.VPNIP).Scan(&t.ID)
			if !errors.Is(err, pgx.ErrNoRows) {
				break
			}
		}
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO token(id, tunnel_id, secret_hash) VALUES ($1, $2, $3)`,
			tok.ID, t.ID, auth.Hash(tok))
		return err
	})
	return t, tok, err
}

// RotateToken invalida el token vigente del tunnel y emite uno nuevo.
func (s *Store) RotateToken(ctx context.Context, tunnelID int64) (auth.Token, error) {
	tok := auth.New(auth.PrefixAgent)
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM token WHERE tunnel_id = $1`, tunnelID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `INSERT INTO token(id, tunnel_id, secret_hash) VALUES ($1, $2, $3)`,
			tok.ID, tunnelID, auth.Hash(tok)); err != nil {
			return err
		}
		// El agente que usaba el token viejo pierde el túnel de inmediato.
		_, err = tx.Exec(ctx, `DELETE FROM agent_lease WHERE tunnel_id = $1`, tunnelID)
		return err
	})
	return tok, err
}

func (s *Store) ListTunnels(ctx context.Context) ([]Tunnel, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.id, a.email, a.plan, t.subdomain, host(t.vpn_ip),
		       COALESCE(l.expires_at > now(), false)
		FROM tunnel t JOIN account a ON a.id = t.account_id
		LEFT JOIN agent_lease l ON l.tunnel_id = t.id
		ORDER BY t.id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tunnel, error) {
		var t Tunnel
		return t, r.Scan(&t.ID, &t.Email, &t.Plan, &t.Subdomain, &t.VPNIP, &t.Online)
	})
}

// AuthAgent valida un token de agente y devuelve su tunnel.
func (s *Store) AuthAgent(ctx context.Context, tok auth.Token) (Tunnel, error) {
	var t Tunnel
	var hash []byte
	err := s.db.QueryRow(ctx, `
		SELECT t.id, a.email, a.plan, t.subdomain, host(t.vpn_ip), k.secret_hash
		FROM token k JOIN tunnel t ON t.id = k.tunnel_id JOIN account a ON a.id = t.account_id
		WHERE k.id = $1`, tok.ID).Scan(&t.ID, &t.Email, &t.Plan, &t.Subdomain, &t.VPNIP, &hash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.Verify(tok, hash)) {
		return Tunnel{}, ErrUnauthorized
	}
	return t, err
}

// ---------------------------------------------------------------- leases

func (s *Store) AcquireLease(ctx context.Context, tunnelID int64, instanceID, pubkey, version string, ttl time.Duration) error {
	tag, err := s.db.Exec(ctx, `
		INSERT INTO agent_lease(tunnel_id, instance_id, wg_pubkey, agent_version, expires_at)
		VALUES ($1, $2, $3, $4, now() + $5::interval)
		ON CONFLICT (tunnel_id) DO UPDATE SET
			instance_id = EXCLUDED.instance_id, wg_pubkey = EXCLUDED.wg_pubkey,
			agent_version = EXCLUDED.agent_version, acquired_at = now(), expires_at = EXCLUDED.expires_at
		WHERE agent_lease.instance_id = EXCLUDED.instance_id OR agent_lease.expires_at < now()`,
		tunnelID, instanceID, pubkey, version, ttl.String())
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrPubkeyInUse
	}
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseHeld
	}
	s.db.Exec(ctx, `UPDATE token SET last_used_at = now() WHERE tunnel_id = $1`, tunnelID)
	return nil
}

func (s *Store) RenewLease(ctx context.Context, tunnelID int64, instanceID string, ttl time.Duration) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE agent_lease SET expires_at = now() + $3::interval
		WHERE tunnel_id = $1 AND instance_id = $2`, tunnelID, instanceID, ttl.String())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrSuperseded
	}
	return nil
}

func (s *Store) ReleaseLease(ctx context.Context, tunnelID int64, instanceID string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM agent_lease WHERE tunnel_id = $1 AND instance_id = $2`, tunnelID, instanceID)
	return err
}

// ---------------------------------------------------------------- nodes

type Node struct {
	ID        int64
	Name      string
	Endpoint  string
	GatewayIP string
	PublicKey string
	LastSeen  *time.Time
}

func (s *Store) CreateNode(ctx context.Context, name, endpoint string) (Node, auth.Token, error) {
	tok := auth.New(auth.PrefixNode)
	n := Node{Name: name, Endpoint: endpoint}
	err := s.db.QueryRow(ctx, `
		INSERT INTO node(name, endpoint, gateway_ip, token_id, token_hash)
		VALUES ($1, $2, ('10.10.' || nextval('node_gw_seq') || '.1')::inet, $3, $4)
		RETURNING id, host(gateway_ip)`, name, endpoint, tok.ID, auth.Hash(tok)).Scan(&n.ID, &n.GatewayIP)
	return n, tok, err
}

func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.Query(ctx, `
		SELECT id, name, endpoint, host(gateway_ip), COALESCE(wg_pubkey, ''), last_seen_at
		FROM node ORDER BY id`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Node, error) {
		var n Node
		return n, r.Scan(&n.ID, &n.Name, &n.Endpoint, &n.GatewayIP, &n.PublicKey, &n.LastSeen)
	})
}

func (s *Store) AuthNode(ctx context.Context, tok auth.Token) (Node, error) {
	var n Node
	var hash []byte
	err := s.db.QueryRow(ctx, `
		SELECT id, name, endpoint, host(gateway_ip), COALESCE(wg_pubkey, ''), token_hash
		FROM node WHERE token_id = $1`, tok.ID).Scan(&n.ID, &n.Name, &n.Endpoint, &n.GatewayIP, &n.PublicKey, &hash)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.Verify(tok, hash)) {
		return Node{}, ErrUnauthorized
	}
	return n, err
}

func (s *Store) NodeHello(ctx context.Context, nodeID int64, pubkey, version string) error {
	_, err := s.db.Exec(ctx, `UPDATE node SET wg_pubkey = $2, node_version = $3, last_seen_at = now() WHERE id = $1`,
		nodeID, pubkey, version)
	return err
}

func (s *Store) TouchNode(ctx context.Context, nodeID int64) error {
	_, err := s.db.Exec(ctx, `UPDATE node SET last_seen_at = now() WHERE id = $1`, nodeID)
	return err
}

// ActiveNodes devuelve los nodos que se reportaron en la ventana dada.
func (s *Store) ActiveNodes(ctx context.Context, window time.Duration) ([]proto.Node, error) {
	rows, err := s.db.Query(ctx, `
		SELECT name, endpoint, wg_pubkey, host(gateway_ip) FROM node
		WHERE wg_pubkey IS NOT NULL AND last_seen_at > now() - $1::interval
		ORDER BY id`, window.String())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (proto.Node, error) {
		var n proto.Node
		return n, r.Scan(&n.Name, &n.Endpoint, &n.PublicKey, &n.GatewayIP)
	})
}

// NodeConfig arma la foto de peers y rutas de los agentes con lease vigente.
func (s *Store) NodeConfig(ctx context.Context, baseDomain string) (proto.NodeConfig, error) {
	rows, err := s.db.Query(ctx, `
		SELECT t.subdomain, host(t.vpn_ip), l.wg_pubkey
		FROM agent_lease l JOIN tunnel t ON t.id = l.tunnel_id
		WHERE l.expires_at > now() ORDER BY t.id`)
	if err != nil {
		return proto.NodeConfig{}, err
	}
	cfg := proto.NodeConfig{Peers: []proto.Peer{}, Routes: []proto.Route{}}
	var sub, ip, key string
	_, err = pgx.ForEachRow(rows, []any{&sub, &ip, &key}, func() error {
		domain := sub + "." + baseDomain
		cfg.Peers = append(cfg.Peers, proto.Peer{PublicKey: key, VPNIP: ip})
		cfg.Routes = append(cfg.Routes,
			proto.Route{Hostname: domain, VPNIP: ip},
			proto.Route{Hostname: "*." + domain, VPNIP: ip})
		return nil
	})
	if err != nil {
		return proto.NodeConfig{}, err
	}
	b, _ := json.Marshal(cfg)
	sum := sha256.Sum256(b)
	cfg.Version = hex.EncodeToString(sum[:8])
	return cfg, nil
}

// ------------------------------------------------- almacén del agente

type StorageItem struct {
	Key       string
	Size      int64
	UpdatedAt time.Time
}

func (s *Store) StoragePut(ctx context.Context, tunnelID int64, key string, value []byte) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO agent_storage(tunnel_id, key, value) VALUES ($1, $2, $3)
		ON CONFLICT (tunnel_id, key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		tunnelID, key, value)
	return err
}

func (s *Store) StorageGet(ctx context.Context, tunnelID int64, key string) ([]byte, time.Time, error) {
	var value []byte
	var updated time.Time
	err := s.db.QueryRow(ctx, `SELECT value, updated_at FROM agent_storage WHERE tunnel_id = $1 AND key = $2`,
		tunnelID, key).Scan(&value, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, time.Time{}, ErrNotFound
	}
	return value, updated, err
}

func (s *Store) StorageDelete(ctx context.Context, tunnelID int64, key string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM agent_storage WHERE tunnel_id = $1 AND key = $2`, tunnelID, key)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// StorageList devuelve todas las claves bajo un prefijo. El agente se encarga
// de recortarlas si pidió un listado no recursivo.
func (s *Store) StorageList(ctx context.Context, tunnelID int64, prefix string) ([]StorageItem, error) {
	rows, err := s.db.Query(ctx, `
		SELECT key, length(value), updated_at FROM agent_storage
		WHERE tunnel_id = $1 AND starts_with(key, $2) ORDER BY key`,
		tunnelID, prefix)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (StorageItem, error) {
		var it StorageItem
		return it, r.Scan(&it.Key, &it.Size, &it.UpdatedAt)
	})
}

// ------------------------------------------------- DNS-01 delegado (F1b)

// CreateDNSChallenge registra qué tunnel es dueño de un registro TXT del
// proveedor DNS configurado, para poder autorizar su borrado después sin
// exponerle al agente el ID real de ese proveedor.
func (s *Store) CreateDNSChallenge(ctx context.Context, tunnelID int64, fqdn, providerRecordID string) (string, error) {
	id := auth.Random(10)
	_, err := s.db.Exec(ctx, `INSERT INTO dns_challenge(id, tunnel_id, fqdn, provider_record_id) VALUES ($1, $2, $3, $4)`,
		id, tunnelID, fqdn, providerRecordID)
	return id, err
}

// DNSChallengeRecordID devuelve el ID del proveedor DNS de un desafío, solo
// si pertenece al tunnel dado.
func (s *Store) DNSChallengeRecordID(ctx context.Context, tunnelID int64, id string) (string, error) {
	var providerRecordID string
	err := s.db.QueryRow(ctx, `SELECT provider_record_id FROM dns_challenge WHERE id = $1 AND tunnel_id = $2`,
		id, tunnelID).Scan(&providerRecordID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return providerRecordID, err
}

func (s *Store) DeleteDNSChallenge(ctx context.Context, tunnelID int64, id string) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM dns_challenge WHERE id = $1 AND tunnel_id = $2`, id, tunnelID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func offsetAddr(p netip.Prefix, off int64) (netip.Addr, error) {
	if off <= 0 || off >= 1<<(32-p.Bits()) {
		return netip.Addr{}, fmt.Errorf("pool de IPs %s agotado", p)
	}
	a := p.Addr().As4()
	v := binary.BigEndian.Uint32(a[:]) + uint32(off)
	binary.BigEndian.PutUint32(a[:], v)
	return netip.AddrFrom4(a), nil
}
