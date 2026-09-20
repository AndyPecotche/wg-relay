// Package node implementa el data plane: termina WireGuard y rutea las
// conexiones TLS entrantes por SNI hacia el agente que corresponde, sin
// descifrar nada.
package node

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"time"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
	"github.com/AndyPecotche/wg-relay/internal/auth"
	"github.com/AndyPecotche/wg-relay/internal/buildinfo"
	"github.com/AndyPecotche/wg-relay/internal/dnsserver"
	"github.com/AndyPecotche/wg-relay/internal/pipe"
	"github.com/AndyPecotche/wg-relay/internal/proto"
	"github.com/AndyPecotche/wg-relay/internal/proxyproto"
	"github.com/AndyPecotche/wg-relay/internal/sni"
	"github.com/AndyPecotche/wg-relay/internal/wgnet"
)

// AgentPort es el puerto donde cada agente escucha dentro del túnel.
const AgentPort = 443

type Config struct {
	APIURL      string
	Token       string
	WGPort      int
	HTTPSListen string
	HTTPListen  string
	// DNSListen habilita el DNS autoritativo propio para clients.* (vacío =
	// deshabilitado, opt-in explícito: requiere delegación externa
	// deliberada, no hay que sorprender despliegues existentes).
	DNSListen string
	// LocalRoutes son destinos fijos fuera del túnel, ej. el SNI de la API
	// hacia el contenedor de la API en la misma máquina.
	LocalRoutes map[string]string
	Log         *slog.Logger
}

type Node struct {
	cfg    Config
	api    *apiclient.Client
	wg     *wgnet.Net
	routes atomic.Pointer[map[string]netip.Addr]
	dns    *dnsserver.Server
	log    *slog.Logger
}

func Run(ctx context.Context, cfg Config) error {
	tok, err := auth.Parse(cfg.Token, auth.PrefixNode)
	if err != nil {
		return fmt.Errorf("WGRELAY_NODE_TOKEN: %w", err)
	}
	n := &Node{cfg: cfg, api: apiclient.New(cfg.APIURL, cfg.Token), dns: dnsserver.New(), log: cfg.Log}
	empty := map[string]netip.Addr{}
	n.routes.Store(&empty)

	// La clave del nodo se deriva de su token: estable entre reinicios sin
	// guardar nada en disco. El control plane solo tiene el hash del token,
	// así que no puede calcularla.
	key := wgnet.KeyFromSeed(auth.DeriveKey(tok, "node wireguard key"))
	hello, err := n.hello(ctx, key)
	if err != nil {
		return err
	}
	gw, err := netip.ParseAddr(hello.GatewayIP)
	if err != nil {
		return err
	}
	n.wg, err = wgnet.New(wgnet.Config{PrivateKey: key, Address: gw, ListenPort: cfg.WGPort, Logger: cfg.Log})
	if err != nil {
		return err
	}
	defer n.wg.Close()
	n.log.Info("nodo listo", "name", hello.Name, "gateway", gw, "wg_port", cfg.WGPort, "public_key", key.Public())

	errc := make(chan error, 4)
	go func() { errc <- n.syncLoop(ctx) }()
	go func() { errc <- n.serveTLS(ctx) }()
	go func() { errc <- n.serveHTTP(ctx) }()
	go func() { errc <- n.serveDNS(ctx) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return nil
	}
}

func (n *Node) hello(ctx context.Context, key wgnet.Key) (proto.NodeHelloResponse, error) {
	var resp proto.NodeHelloResponse
	for backoff := time.Second; ; backoff = min(backoff*2, 30*time.Second) {
		err := n.api.Do(ctx, http.MethodPost, "/v1/node/hello",
			proto.NodeHelloRequest{WGPublicKey: key.Public().String(), Version: buildinfo.Version}, &resp)
		if err == nil {
			return resp, nil
		}
		if apiclient.Code(err) == proto.ErrUnauthorized {
			return resp, fmt.Errorf("el control plane rechazó el token del nodo")
		}
		n.log.Warn("no se pudo contactar al control plane, reintentando", "err", err, "en", backoff)
		select {
		case <-ctx.Done():
			return resp, ctx.Err()
		case <-time.After(backoff):
		}
	}
}

// syncLoop mantiene peers y rutas al día con long-poll contra el control plane.
func (n *Node) syncLoop(ctx context.Context) error {
	version := ""
	for ctx.Err() == nil {
		var cfg proto.NodeConfig
		err := n.api.Do(ctx, http.MethodGet, "/v1/node/config?since="+version, nil, &cfg)
		switch {
		case errors.Is(err, apiclient.ErrNotModified):
			continue
		case apiclient.Code(err) == proto.ErrUnauthorized:
			return fmt.Errorf("el control plane rechazó el token del nodo")
		case err != nil:
			if ctx.Err() == nil {
				n.log.Warn("sync de configuración falló; se mantiene la última", "err", err)
				time.Sleep(3 * time.Second)
			}
			continue
		}
		if err := n.apply(cfg); err != nil {
			n.log.Error("configuración inválida del control plane", "err", err)
			time.Sleep(3 * time.Second)
			continue
		}
		version = cfg.Version
	}
	return nil
}

func (n *Node) apply(cfg proto.NodeConfig) error {
	peers := make([]wgnet.Peer, 0, len(cfg.Peers))
	for _, p := range cfg.Peers {
		key, err := wgnet.ParseKey(p.PublicKey)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(p.VPNIP)
		if err != nil {
			return err
		}
		peers = append(peers, wgnet.Peer{PublicKey: key, AllowedIP: netip.PrefixFrom(ip, 32)})
	}
	routes := make(map[string]netip.Addr, len(cfg.Routes))
	for _, r := range cfg.Routes {
		ip, err := netip.ParseAddr(r.VPNIP)
		if err != nil {
			return err
		}
		routes[strings.ToLower(r.Hostname)] = ip
	}
	if err := n.wg.SetPeers(peers); err != nil {
		return err
	}
	n.routes.Store(&routes)
	if cfg.DNS != nil {
		n.dns.SetZone(dnsserver.Zone{
			Name:       cfg.DNS.Zone,
			NSNames:    cfg.DNS.NSNames,
			SOAEmail:   cfg.DNS.SOAEmail,
			Edge:       cfg.DNS.Edge,
			Challenges: dnsChallenges(cfg.DNS.Challenges),
		})
	}
	n.log.Info("configuración aplicada", "version", cfg.Version, "agentes", len(peers))
	return nil
}

// dnsChallenges agrupa la lista plana que viaja por el protocolo en el mapa
// fqdn->valores que dnsserver.Zone espera (varios valores concurrentes bajo
// el mismo nombre son un caso soportado, ver internal/cloudflare).
func dnsChallenges(txt []proto.DNSTXT) map[string][]string {
	m := make(map[string][]string, len(txt))
	for _, c := range txt {
		m[c.FQDN] = append(m[c.FQDN], c.Value)
	}
	return m
}

// serveDNS sirve la zona clients.* como DNS autoritativo propio, si el
// operador lo habilitó con WGRELAY_DNS_LISTEN. Deshabilitado (default) tiene
// que bloquear hasta ctx.Done() como cualquier otro listener, no retornar
// enseguida: el primer valor que llega a errc en Run() termina el proceso.
func (n *Node) serveDNS(ctx context.Context) error {
	if n.cfg.DNSListen == "" {
		<-ctx.Done()
		return nil
	}
	n.log.Info("DNS autoritativo propio escuchando", "addr", n.cfg.DNSListen)
	return n.dns.Serve(ctx, n.cfg.DNSListen)
}

func (n *Node) serveTLS(ctx context.Context) error {
	ln, err := net.Listen("tcp", n.cfg.HTTPSListen)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); ln.Close() }()
	n.log.Info("router SNI escuchando", "addr", n.cfg.HTTPSListen)
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			n.log.Warn("accept", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go n.handleTLS(ctx, c)
	}
}

func (n *Node) handleTLS(ctx context.Context, c net.Conn) {
	host, conn, err := sni.Peek(c, 5*time.Second)
	if err != nil {
		c.Close() // sin SNI o no es TLS: se corta sin responder nada
		return
	}
	if target, ok := sni.Match(n.cfg.LocalRoutes, host); ok {
		up, err := net.DialTimeout("tcp", target, 5*time.Second)
		if err != nil {
			n.log.Warn("destino local inaccesible", "host", host, "target", target, "err", err)
			c.Close()
			return
		}
		pipe.Join(conn, up)
		return
	}
	ip, ok := sni.Match(*n.routes.Load(), host)
	if !ok {
		n.log.Debug("SNI sin ruta", "host", host, "from", c.RemoteAddr())
		c.Close()
		return
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	up, err := n.wg.Dial(dctx, netip.AddrPortFrom(ip, AgentPort))
	cancel()
	if err != nil {
		n.log.Debug("agente inaccesible", "host", host, "vpn_ip", ip, "err", err)
		c.Close()
		return
	}
	src, _ := netip.ParseAddrPort(c.RemoteAddr().String())
	dst, _ := netip.ParseAddrPort(c.LocalAddr().String())
	if _, err := up.Write(proxyproto.Header(src, dst)); err != nil {
		c.Close()
		up.Close()
		return
	}
	pipe.Join(conn, up)
}

// serveHTTP redirige a HTTPS los hostnames conocidos y corta el resto sin
// responder (equivalente al "return 444" de nginx).
func (n *Node) serveHTTP(ctx context.Context) error {
	if n.cfg.HTTPListen == "" {
		return nil
	}
	srv := &http.Server{
		Addr:              n.cfg.HTTPListen,
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := strings.ToLower(r.Host)
			if h, _, err := net.SplitHostPort(host); err == nil {
				host = h
			}
			_, local := sni.Match(n.cfg.LocalRoutes, host)
			_, remote := sni.Match(*n.routes.Load(), host)
			if !local && !remote {
				panic(http.ErrAbortHandler)
			}
			http.Redirect(w, r, "https://"+host+r.URL.RequestURI(), http.StatusPermanentRedirect)
		}),
		ErrorLog: nil,
	}
	go func() { <-ctx.Done(); srv.Close() }()
	n.log.Info("redirector HTTP escuchando", "addr", n.cfg.HTTPListen)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ParseLocalRoutes interpreta "host=destino,host2=destino2".
func ParseLocalRoutes(s string) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(strings.ReplaceAll(s, ",", "\n")))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		host, target, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("ruta local inválida %q (formato host=destino:puerto)", line)
		}
		out[strings.ToLower(strings.TrimSpace(host))] = strings.TrimSpace(target)
	}
	return out, nil
}
