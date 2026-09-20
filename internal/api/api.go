// Package api implementa el control plane HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/AndyPecotche/wg-relay/internal/auth"
	"github.com/AndyPecotche/wg-relay/internal/cloudflare"
	"github.com/AndyPecotche/wg-relay/internal/proto"
	"github.com/AndyPecotche/wg-relay/internal/store"
	"github.com/AndyPecotche/wg-relay/internal/wgnet"
)

const (
	LeaseTTL        = 45 * time.Second
	HeartbeatEvery  = 15 * time.Second
	nodeFreshness   = 90 * time.Second
	longPollTimeout = 25 * time.Second
	longPollTick    = time.Second
)

type Server struct {
	Store      *store.Store
	BaseDomain string
	Log        *slog.Logger
	// Cloudflare habilita el endpoint de DNS-01 delegado (F1b). Si es nil,
	// ese endpoint responde 501: el resto del servicio funciona igual.
	Cloudflare *cloudflare.Client

	dnsLimitersMu sync.Mutex
	dnsLimiters   map[int64]*rate.Limiter // por tunnel_id
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /v1/agent/register", s.agent(s.register))
	mux.HandleFunc("POST /v1/agent/heartbeat", s.agent(s.heartbeat))
	mux.HandleFunc("POST /v1/agent/release", s.agent(s.release))
	mux.HandleFunc("GET /v1/agent/storage", s.agent(s.storageList))
	mux.HandleFunc("GET /v1/agent/storage/{key...}", s.agent(s.storageGet))
	mux.HandleFunc("HEAD /v1/agent/storage/{key...}", s.agent(s.storageGet))
	mux.HandleFunc("PUT /v1/agent/storage/{key...}", s.agent(s.storagePut))
	mux.HandleFunc("DELETE /v1/agent/storage/{key...}", s.agent(s.storageDelete))
	mux.HandleFunc("POST /v1/agent/acme-dns", s.agent(s.acmeDNSCreate))
	mux.HandleFunc("DELETE /v1/agent/acme-dns/{id}", s.agent(s.acmeDNSDelete))
	mux.HandleFunc("POST /v1/node/hello", s.node(s.nodeHello))
	mux.HandleFunc("GET /v1/node/config", s.node(s.nodeConfig))
	return mux
}

// ---------------------------------------------------------------- agentes

type agentHandler func(w http.ResponseWriter, r *http.Request, t store.Tunnel)

func (s *Server) agent(h agentHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.Parse(bearer(r), auth.PrefixAgent)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, proto.ErrUnauthorized, "token ausente o con formato inválido")
			return
		}
		t, err := s.Store.AuthAgent(r.Context(), tok)
		if err != nil {
			s.fail(w, err)
			return
		}
		h(w, r, t)
	}
}

func (s *Server) register(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	var req proto.RegisterRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := wgnet.ParseKey(req.WGPublicKey); err != nil || !validInstance(req.InstanceID) {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "wg_public_key o instance_id inválidos")
		return
	}
	if err := s.Store.AcquireLease(r.Context(), t.ID, req.InstanceID, req.WGPublicKey, req.Version, LeaseTTL); err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Store.ActiveNodes(r.Context(), nodeFreshness)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("agente registrado", "tunnel", t.ID, "subdomain", t.Subdomain, "instance", req.InstanceID, "version", req.Version)
	writeJSON(w, proto.RegisterResponse{
		TunnelID:         t.ID,
		VPNIP:            t.VPNIP,
		Domain:           t.Subdomain + "." + s.BaseDomain,
		Nodes:            nodes,
		LeaseTTLSeconds:  int(LeaseTTL.Seconds()),
		HeartbeatSeconds: int(HeartbeatEvery.Seconds()),
	})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	var req proto.HeartbeatRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.Store.RenewLease(r.Context(), t.ID, req.InstanceID, LeaseTTL); err != nil {
		s.fail(w, err)
		return
	}
	nodes, err := s.Store.ActiveNodes(r.Context(), nodeFreshness)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, proto.HeartbeatResponse{Nodes: nodes})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	var req proto.ReleaseRequest
	if !decode(w, r, &req) {
		return
	}
	if err := s.Store.ReleaseLease(r.Context(), t.ID, req.InstanceID); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("agente liberó el tunnel", "tunnel", t.ID, "instance", req.InstanceID)
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------- almacén del agente
//
// Es un key-value opaco por tunnel: el agente guarda acá su material ACME
// cifrado con una clave derivada de su token, de la que el servidor solo tiene
// el hash. Sin esto el agente pediría certificados nuevos en cada arranque.

const maxStorageValue = 1 << 20 // 1 MiB por clave

func storageKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if key == "" || len(key) > 512 || strings.Contains(key, "..") {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "clave inválida")
		return "", false
	}
	return key, true
}

func (s *Server) storageGet(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	key, ok := storageKey(w, r)
	if !ok {
		return
	}
	value, updated, err := s.Store.StorageGet(r.Context(), t.ID, key)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "clave inexistente")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(value)))
	w.Header().Set("Last-Modified", updated.UTC().Format(http.TimeFormat))
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	w.Write(value)
}

func (s *Server) storagePut(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	key, ok := storageKey(w, r)
	if !ok {
		return
	}
	value, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxStorageValue))
	if err != nil {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "cuerpo demasiado grande")
		return
	}
	if err := s.Store.StoragePut(r.Context(), t.ID, key, value); err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storageDelete(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	key, ok := storageKey(w, r)
	if !ok {
		return
	}
	err := s.Store.StorageDelete(r.Context(), t.ID, key)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, "clave inexistente")
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) storageList(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	items, err := s.Store.StorageList(r.Context(), t.ID, r.URL.Query().Get("prefix"))
	if err != nil {
		s.fail(w, err)
		return
	}
	out := proto.StorageList{Items: make([]proto.StorageItem, len(items))}
	for i, it := range items {
		out.Items[i] = proto.StorageItem{Key: it.Key, Size: it.Size, UpdatedAt: it.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	}
	writeJSON(w, out)
}

// ------------------------------------------------- ACME DNS-01 delegado (F1b)
//
// Habilita certificados wildcard sobre el dominio asignado: el agente pide
// que se escriba el TXT del desafío, nosotros lo hacemos en Cloudflare
// (nunca el agente, que no tiene ni debe tener ese token) y solo dentro de
// la zona del tunnel autenticado. Ver DESIGN.md §6.2.1.

func (s *Server) acmeDNSCreate(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	if s.Cloudflare == nil {
		writeErr(w, http.StatusNotImplemented, proto.ErrInternal, "el servidor no tiene un proveedor DNS configurado")
		return
	}
	if !s.dnsLimiter(t.ID).Allow() {
		writeErr(w, http.StatusTooManyRequests, proto.ErrRateLimited, "demasiadas solicitudes de DNS-01; esperá unos segundos")
		return
	}
	var req proto.ACMEDNSCreateRequest
	if !decode(w, r, &req) {
		return
	}
	fqdn := strings.ToLower(strings.TrimSuffix(req.FQDN, "."))
	if !validACMEDNSName(fqdn, t.Subdomain+"."+s.BaseDomain) {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "fqdn fuera del dominio asignado a este tunnel")
		return
	}
	if req.Value == "" || len(req.Value) > 512 {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "value inválido")
		return
	}
	cfID, err := s.Cloudflare.CreateTXT(r.Context(), fqdn, req.Value)
	if err != nil {
		s.Log.Error("cloudflare: creando TXT", "fqdn", fqdn, "err", err)
		writeErr(w, http.StatusBadGateway, proto.ErrInternal, "no se pudo crear el registro DNS")
		return
	}
	id, err := s.Store.CreateDNSChallenge(r.Context(), t.ID, fqdn, cfID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("DNS-01: TXT creado", "tunnel", t.ID, "fqdn", fqdn)
	writeJSON(w, proto.ACMEDNSCreateResponse{ID: id})
}

func (s *Server) acmeDNSDelete(w http.ResponseWriter, r *http.Request, t store.Tunnel) {
	if s.Cloudflare == nil {
		writeErr(w, http.StatusNotImplemented, proto.ErrInternal, "el servidor no tiene un proveedor DNS configurado")
		return
	}
	id := r.PathValue("id")
	cfID, err := s.Store.DNSChallengeRecordID(r.Context(), t.ID, id)
	if errors.Is(err, store.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent) // ya no existe: el estado deseado ya está logrado
		return
	}
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.Cloudflare.DeleteRecord(r.Context(), cfID); err != nil {
		// No abortamos: preferible un TXT huérfano (TTL 60s) a dejar al
		// agente sin poder terminar de limpiar su propio estado.
		s.Log.Warn("cloudflare: borrando TXT", "id", id, "err", err)
	}
	if err := s.Store.DeleteDNSChallenge(r.Context(), t.ID, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// validACMEDNSName exige que fqdn sea "_acme-challenge." seguido del dominio
// del tunnel, o de un subdominio de este (para comodines más profundos, como
// *.dev.<sub>.<base>). Es la única puerta que evita que un tunnel escriba un
// TXT fuera de su propia zona.
func validACMEDNSName(fqdn, tunnelDomain string) bool {
	rest, ok := strings.CutPrefix(fqdn, "_acme-challenge.")
	if !ok {
		return false
	}
	return rest == tunnelDomain || strings.HasSuffix(rest, "."+tunnelDomain)
}

// dnsLimiter da un limitador por tunnel: sin él, un agente en bucle de
// reintentos podría agotar la cuota de la API de Cloudflare para todos.
//
// El mapa vive en memoria del proceso: no se comparte entre réplicas de la
// API ni se poda con el tiempo. Aceptable con la cantidad de tunnels de hoy;
// si el número de tunnels crece mucho o la API se replica, hay que moverlo a
// un almacén compartido (Redis, o una tabla con TTL en Postgres).
func (s *Server) dnsLimiter(tunnelID int64) *rate.Limiter {
	s.dnsLimitersMu.Lock()
	defer s.dnsLimitersMu.Unlock()
	if s.dnsLimiters == nil {
		s.dnsLimiters = make(map[int64]*rate.Limiter)
	}
	l, ok := s.dnsLimiters[tunnelID]
	if !ok {
		l = rate.NewLimiter(rate.Every(2*time.Second), 10) // ráfaga de 10, ~30/min sostenido
		s.dnsLimiters[tunnelID] = l
	}
	return l
}

// ---------------------------------------------------------------- nodos

type nodeHandler func(w http.ResponseWriter, r *http.Request, n store.Node)

func (s *Server) node(h nodeHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, err := auth.Parse(bearer(r), auth.PrefixNode)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, proto.ErrUnauthorized, "token de nodo inválido")
			return
		}
		n, err := s.Store.AuthNode(r.Context(), tok)
		if err != nil {
			s.fail(w, err)
			return
		}
		h(w, r, n)
	}
}

func (s *Server) nodeHello(w http.ResponseWriter, r *http.Request, n store.Node) {
	var req proto.NodeHelloRequest
	if !decode(w, r, &req) {
		return
	}
	if _, err := wgnet.ParseKey(req.WGPublicKey); err != nil {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "wg_public_key inválida")
		return
	}
	if err := s.Store.NodeHello(r.Context(), n.ID, req.WGPublicKey, req.Version); err != nil {
		s.fail(w, err)
		return
	}
	s.Log.Info("nodo conectado", "node", n.Name, "gateway", n.GatewayIP, "version", req.Version)
	writeJSON(w, proto.NodeHelloResponse{Name: n.Name, GatewayIP: n.GatewayIP})
}

// nodeConfig es un long-poll: responde en cuanto la foto difiere de ?since=,
// o al vencer el timeout. Consultar la base cada segundo (en vez de notificar
// en memoria) lo mantiene correcto con varias réplicas del control plane y
// capta también los leases que vencen sin que nadie escriba.
func (s *Server) nodeConfig(w http.ResponseWriter, r *http.Request, n store.Node) {
	since := r.URL.Query().Get("since")
	ctx, cancel := context.WithTimeout(r.Context(), longPollTimeout)
	defer cancel()
	for {
		if err := s.Store.TouchNode(ctx, n.ID); err != nil && ctx.Err() == nil {
			s.fail(w, err)
			return
		}
		cfg, err := s.Store.NodeConfig(ctx, s.BaseDomain)
		if err != nil && ctx.Err() == nil {
			s.fail(w, err)
			return
		}
		if err == nil && cfg.Version != since {
			writeJSON(w, cfg)
			return
		}
		select {
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return // el nodo cortó
			}
			w.WriteHeader(http.StatusNotModified)
			return
		case <-time.After(longPollTick):
		}
	}
}

// ---------------------------------------------------------------- helpers

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUnauthorized):
		writeErr(w, http.StatusUnauthorized, proto.ErrUnauthorized, err.Error())
	case errors.Is(err, store.ErrLeaseHeld):
		writeErr(w, http.StatusConflict, proto.ErrLeaseHeld, err.Error())
	case errors.Is(err, store.ErrSuperseded):
		writeErr(w, http.StatusConflict, proto.ErrSuperseded, err.Error())
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, proto.ErrNotFound, err.Error())
	case errors.Is(err, store.ErrPubkeyInUse):
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, err.Error())
	default:
		s.Log.Error("error interno", "err", err)
		writeErr(w, http.StatusInternalServerError, proto.ErrInternal, "error interno")
	}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if v, ok := strings.CutPrefix(h, "Bearer "); ok {
		return v
	}
	return ""
}

func validInstance(id string) bool { return id != "" && len(id) <= 64 }

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, proto.ErrBadRequest, "JSON inválido")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(proto.Error{Code: code, Message: msg})
}
