// Package agent implementa el cliente que corre en el servidor del usuario.
//
// No guarda nada en disco. En cada arranque genera una clave WireGuard nueva
// (vive solo en memoria), la registra con el token y toma el "lease" del
// tunnel. Todo lo demás (IP de VPN, dominio, nodos) lo provee el control plane.
package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
	"github.com/AndyPecotche/wg-relay/internal/auth"
	"github.com/AndyPecotche/wg-relay/internal/buildinfo"
	"github.com/AndyPecotche/wg-relay/internal/pipe"
	"github.com/AndyPecotche/wg-relay/internal/proto"
	"github.com/AndyPecotche/wg-relay/internal/proxyproto"
	"github.com/AndyPecotche/wg-relay/internal/sni"
	"github.com/AndyPecotche/wg-relay/internal/wgnet"
)

const listenPort = 443 // puerto del agente dentro del túnel (ver node.AgentPort)

type Config struct {
	Token string
	File  File
	Log   *slog.Logger
}

type Agent struct {
	cfg      Config
	api      *apiclient.Client
	storage  *storage
	instance string
	log      *slog.Logger
}

// ErrFatal envuelve errores ante los que no tiene sentido reintentar.
type ErrFatal struct{ error }

func Run(ctx context.Context, cfg Config) error {
	tok, err := auth.Parse(cfg.Token, auth.PrefixAgent)
	if err != nil {
		return ErrFatal{fmt.Errorf("WGRELAY_TOKEN: %w", err)}
	}
	api := apiclient.New(cfg.File.Relay, cfg.Token)
	st, err := newStorage(api, tok)
	if err != nil {
		return ErrFatal{err}
	}
	a := &Agent{
		cfg:      cfg,
		api:      api,
		storage:  st,
		instance: auth.Random(10),
		log:      cfg.Log,
	}
	for ctx.Err() == nil {
		if err := a.session(ctx); err != nil {
			if _, fatal := err.(ErrFatal); fatal {
				return err
			}
			a.log.Warn("sesión terminada, reconectando", "err", err)
		}
	}
	return nil
}

// session registra una clave nueva, levanta el túnel y lo mantiene hasta
// perder el lease o hasta que ctx termine.
func (a *Agent) session(ctx context.Context) error {
	key := wgnet.GenerateKey()
	reg, err := a.register(ctx, key)
	if err != nil {
		return err
	}
	routes, err := Resolve(a.cfg.File.Routes, reg.Domain)
	if err != nil {
		return ErrFatal{err}
	}
	vpnIP, err := netip.ParseAddr(reg.VPNIP)
	if err != nil {
		return err
	}
	wg, err := wgnet.New(wgnet.Config{PrivateKey: key, Address: vpnIP, Logger: a.log})
	if err != nil {
		return err
	}
	defer wg.Close()

	t := &tunnel{agent: a, wg: wg, routes: routes, domain: reg.Domain}
	// Las rutas terminate necesitan certificado: se piden en segundo plano,
	// así que un fallo de ACME no impide que el resto del túnel funcione.
	term, err := newTerminator(ctx, routes, a.storage, a.cfg.File.ACME, a.log)
	if err != nil {
		return ErrFatal{err}
	}
	defer term.close()
	t.term = term
	if err := t.setNodes(ctx, reg.Nodes); err != nil {
		a.log.Warn("configurando nodos", "err", err)
	}
	ln, err := wg.Listen(listenPort)
	if err != nil {
		return err
	}
	defer ln.Close()
	go t.accept(ln)

	a.printBanner(reg, routes)
	defer func() {
		// Liberar el lease al salir permite que otra instancia tome el tunnel
		// al instante, sin esperar a que venza.
		rctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.api.Do(rctx, http.MethodPost, "/v1/agent/release", proto.ReleaseRequest{InstanceID: a.instance}, nil)
	}()
	return t.heartbeat(ctx, time.Duration(reg.HeartbeatSeconds)*time.Second)
}

func (a *Agent) register(ctx context.Context, key wgnet.Key) (proto.RegisterResponse, error) {
	var resp proto.RegisterResponse
	req := proto.RegisterRequest{InstanceID: a.instance, WGPublicKey: key.Public().String(), Version: buildinfo.Version}
	warned := false
	for backoff := time.Second; ; {
		err := a.api.Do(ctx, http.MethodPost, "/v1/agent/register", req, &resp)
		switch apiclient.Code(err) {
		case "":
			if err == nil {
				return resp, nil
			}
			a.log.Warn("no se pudo contactar a la API del relay, reintentando", "err", err, "en", backoff)
			backoff = min(backoff*2, 30*time.Second)
		case proto.ErrUnauthorized:
			return resp, ErrFatal{fmt.Errorf("el relay rechazó el token (¿revocado o rotado?)")}
		case proto.ErrLeaseHeld:
			if !warned {
				a.log.Warn("otra instancia está usando este token; esta queda en espera y tomará el túnel cuando se libere")
				warned = true
			}
			backoff = 15 * time.Second
		default:
			return resp, err
		}
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (a *Agent) printBanner(reg proto.RegisterResponse, routes map[string]RouteSpec) {
	var b strings.Builder
	fmt.Fprintf(&b, "\n  wg-relay %s\n\n", buildinfo.Version)
	fmt.Fprintf(&b, "  Dominio:  %s\n", reg.Domain)
	fmt.Fprintf(&b, "  IP VPN:   %s\n", reg.VPNIP)
	names := make([]string, len(reg.Nodes))
	for i, n := range reg.Nodes {
		names[i] = n.Name
	}
	if len(names) == 0 {
		names = []string{"ninguno activo todavía (el túnel se arma solo cuando aparezca uno)"}
	}
	fmt.Fprintf(&b, "  Nodos:    %s\n\n", strings.Join(names, ", "))
	hosts := make([]string, 0, len(routes))
	for h := range routes {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		r := routes[h]
		pp := ""
		if r.ProxyProtocol {
			pp = ", proxy_protocol"
		}
		fmt.Fprintf(&b, "  %s:443  →  %s  (%s%s)\n", h, r.To, r.Mode, pp)
	}
	if len(routes) == 0 {
		b.WriteString("  (sin rutas: agregá entradas en routes: de wgrelay.yml)\n")
	}
	fmt.Println(b.String())
}

// ---------------------------------------------------------------- túnel

type tunnel struct {
	agent  *Agent
	wg     *wgnet.Net
	routes map[string]RouteSpec
	domain string
	term   *terminator

	mu        sync.Mutex
	nodes     map[wgnet.Key]proto.Node
	gateways  map[netip.Addr]bool
	connected map[wgnet.Key]bool
}

func (t *tunnel) heartbeat(ctx context.Context, every time.Duration) error {
	check := time.NewTicker(2 * time.Second) // detectar el primer handshake rápido
	defer check.Stop()
	next := time.After(t.interval(every))
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-check.C:
			t.reportHandshakes()
		case <-next:
			next = time.After(t.interval(every))
			var resp proto.HeartbeatResponse
			err := t.agent.api.Do(ctx, http.MethodPost, "/v1/agent/heartbeat",
				proto.HeartbeatRequest{InstanceID: t.agent.instance}, &resp)
			switch apiclient.Code(err) {
			case "":
				if err != nil {
					t.agent.log.Warn("heartbeat falló; el túnel sigue activo mientras el lease no venza", "err", err)
					continue
				}
				if err := t.setNodes(ctx, resp.Nodes); err != nil {
					t.agent.log.Warn("actualizando nodos", "err", err)
				}
			case proto.ErrSuperseded:
				return fmt.Errorf("otra instancia tomó este token")
			case proto.ErrUnauthorized:
				return ErrFatal{fmt.Errorf("el relay rechazó el token (¿revocado o rotado?)")}
			default:
				t.agent.log.Warn("heartbeat", "err", err)
			}
		}
	}
}

// interval acorta el heartbeat mientras no haya nodos: así un agente que
// arrancó antes que el nodo se entera en segundos y no en un ciclo completo.
func (t *tunnel) interval(every time.Duration) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.nodes) == 0 {
		return min(every, 3*time.Second)
	}
	return every
}

func (t *tunnel) setNodes(ctx context.Context, nodes []proto.Node) error {
	peers := make([]wgnet.Peer, 0, len(nodes))
	byKey := map[wgnet.Key]proto.Node{}
	gws := map[netip.Addr]bool{}
	var firstErr error
	for _, n := range nodes {
		key, err1 := wgnet.ParseKey(n.PublicKey)
		gw, err2 := netip.ParseAddr(n.GatewayIP)
		ep, err3 := wgnet.ResolveEndpoint(ctx, n.Endpoint)
		if err := firstNonNil(err1, err2, err3); err != nil {
			firstErr = fmt.Errorf("nodo %s: %w", n.Name, err)
			continue
		}
		peers = append(peers, wgnet.Peer{PublicKey: key, AllowedIP: netip.PrefixFrom(gw, 32), Endpoint: ep, Keepalive: 25})
		byKey[key] = n
		gws[gw] = true
	}
	if err := t.wg.SetPeers(peers); err != nil {
		return err
	}
	t.mu.Lock()
	t.nodes, t.gateways = byKey, gws
	if t.connected == nil {
		t.connected = map[wgnet.Key]bool{}
	}
	t.mu.Unlock()
	return firstErr
}

func (t *tunnel) reportHandshakes() {
	hs := t.wg.LastHandshakes()
	t.mu.Lock()
	defer t.mu.Unlock()
	for key, n := range t.nodes {
		ok := time.Since(hs[key]) < 3*time.Minute
		if ok != t.connected[key] {
			if ok {
				t.agent.log.Info("túnel establecido", "nodo", n.Name, "endpoint", n.Endpoint)
			} else {
				t.agent.log.Warn("túnel caído", "nodo", n.Name)
			}
			t.connected[key] = ok
		}
	}
}

func (t *tunnel) isGateway(addr net.Addr) bool {
	ap, err := netip.ParseAddrPort(addr.String())
	if err != nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gateways[ap.Addr()]
}

func (t *tunnel) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go t.handle(c)
	}
}

func (t *tunnel) handle(c net.Conn) {
	log := t.agent.log
	if !t.isGateway(c.RemoteAddr()) {
		c.Close()
		return
	}
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	src, _, rawHeader, err := proxyproto.Read(c)
	c.SetReadDeadline(time.Time{})
	if err != nil {
		log.Debug("conexión sin header PROXY", "err", err)
		c.Close()
		return
	}
	host, conn, err := sni.Peek(c, 5*time.Second)
	if err != nil {
		c.Close()
		return
	}
	// Match exacto primero y después comodines, igual que en el nodo: así una
	// ruta "*" cubre todos los subdominios sin enumerarlos.
	route, ok := sni.Match(t.routes, host)
	if !ok {
		log.Debug("hostname sin ruta en wgrelay.yml", "host", host, "cliente", src)
		c.Close()
		return
	}
	if route.Mode == ModeTerminate {
		// El TLS lo termina el agente: la conexión pasa al servidor HTTPS
		// interno, con la IP real del visitante ya puesta.
		log.Debug("conexión (terminate)", "host", host, "cliente", src, "to", route.To)
		t.term.handle(addrConn{Conn: conn, remote: net.TCPAddrFromAddrPort(src)})
		return
	}
	up, err := net.DialTimeout("tcp", route.To, 5*time.Second)
	if err != nil {
		log.Warn("no se pudo conectar al destino de la ruta", "host", host, "to", route.To, "err", err)
		c.Close()
		return
	}
	if route.ProxyProtocol {
		if _, err := up.Write(rawHeader); err != nil {
			c.Close()
			up.Close()
			return
		}
	}
	log.Debug("conexión", "host", host, "cliente", src, "to", route.To)
	pipe.Join(conn, up)
}

func firstNonNil(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}
