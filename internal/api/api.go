// Package api implementa el control plane HTTP.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/AndyPecotche/wg-relay/internal/auth"
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
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("POST /v1/agent/register", s.agent(s.register))
	mux.HandleFunc("POST /v1/agent/heartbeat", s.agent(s.heartbeat))
	mux.HandleFunc("POST /v1/agent/release", s.agent(s.release))
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
