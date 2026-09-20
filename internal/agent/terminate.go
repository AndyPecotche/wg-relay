package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
	"github.com/AndyPecotche/wg-relay/internal/pipe"
	"github.com/AndyPecotche/wg-relay/internal/sni"
)

// terminator atiende las rutas en modo terminate: obtiene los certificados,
// termina el TLS y proxea HTTP al servicio del usuario.
//
// Usa TLS-ALPN-01, que funciona a través del túnel sin que el control plane
// participe: el challenge entra por el :443 del nodo con el SNI del cliente y
// se rutea hasta acá como cualquier otra conexión.
type terminator struct {
	cache         *certmagic.Cache
	wildcardCache *certmagic.Cache // nil si no hay comodines en terminate
	cfg           *certmagic.Config
	tlsCfg        *tls.Config
	srv           *http.Server
	ln            *chanListener
	proxies       map[string]*httputil.ReverseProxy // rutas http(s)://
	tcpTargets    map[string]string                 // rutas tcp://: host -> destino
	log           *slog.Logger
}

func newTerminator(ctx context.Context, routes map[string]RouteSpec, st certmagic.Storage, api *apiclient.Client, acme ACME, log *slog.Logger) (*terminator, error) {
	hosts := make([]string, 0, len(routes))
	proxies := make(map[string]*httputil.ReverseProxy, len(routes))
	tcpTargets := make(map[string]string, len(routes))
	for host, r := range routes {
		if r.Mode != ModeTerminate {
			continue
		}
		target, err := url.Parse(r.To)
		if err != nil {
			return nil, fmt.Errorf("ruta %q: destino inválido: %w", host, err)
		}
		if target.Scheme == "tcp" {
			// El agente termina el TLS y entrega bytes crudos: sin HTTP de
			// por medio. Mismo mecanismo de certificados que el resto.
			tcpTargets[host] = target.Host
		} else {
			proxies[host] = &httputil.ReverseProxy{
				Rewrite: func(pr *httputil.ProxyRequest) {
					pr.SetURL(target)
					pr.Out.Host = pr.In.Host // el backend ve el hostname público
					pr.SetXForwarded()       // X-Forwarded-For con la IP real del visitante
				},
				ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn),
			}
		}
		hosts = append(hosts, host)
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	sort.Strings(hosts)

	// Separados en dos emisores ACME, no uno: si el mismo emisor tuviera
	// configurados DNS01Solver y TLS-ALPN a la vez, acmez puede preferir
	// dns-01 incluso para hosts concretos que no lo necesitan, y entonces
	// certificados que hoy funcionan sin ningún proveedor DNS pasarían a
	// depender de uno. Solo los comodines (que ACME exige resolver por
	// DNS-01) usan el emisor con DNS01Solver; el resto sigue en TLS-ALPN-01
	// puro.
	var normalHosts, wildcardHosts []string
	for _, h := range hosts {
		if strings.HasPrefix(h, "*.") {
			wildcardHosts = append(wildcardHosts, h)
		} else {
			normalHosts = append(normalHosts, h)
		}
	}

	roots, err := loadTrustedRoots(acme.TrustedRoots)
	if err != nil {
		return nil, err
	}
	zl := zap.New(&slogCore{log: log.With("component", "acme")})
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(nil, certmagic.Config{Storage: st, Logger: zl}), nil
		},
		Logger: zl,
	})
	cfg := certmagic.New(cache, certmagic.Config{Storage: st, Logger: zl})
	cfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:     acme.CAOrDefault(),
		Email:  acme.Email,
		Agreed: true,
		// El :80 del nodo solo redirige a HTTPS: HTTP-01 no es confiable acá.
		DisableHTTPChallenge: true,
		TrustedRoots:         roots,
		Logger:               zl,
	})}
	tlsCfg := cfg.TLSConfig()
	tlsCfg.NextProtos = append([]string{"h2", "http/1.1"}, tlsCfg.NextProtos...)

	t := &terminator{cache: cache, cfg: cfg, tlsCfg: tlsCfg, proxies: proxies, tcpTargets: tcpTargets, log: log}

	var wildcardCfg *certmagic.Config
	if len(wildcardHosts) > 0 {
		wzl := zap.New(&slogCore{log: log.With("component", "acme-wildcard")})
		t.wildcardCache = certmagic.NewCache(certmagic.CacheOptions{
			GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
				return certmagic.New(nil, certmagic.Config{Storage: st, Logger: wzl}), nil
			},
			Logger: wzl,
		})
		wildcardCfg = certmagic.New(t.wildcardCache, certmagic.Config{Storage: st, Logger: wzl})
		wildcardCfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(wildcardCfg, certmagic.ACMEIssuer{
			CA:     acme.CAOrDefault(),
			Email:  acme.Email,
			Agreed: true,
			// DNS-01 es el único desafío que ACME acepta para un comodín;
			// deshabilitar los otros dos evita cualquier ambigüedad.
			DisableHTTPChallenge:    true,
			DisableTLSALPNChallenge: true,
			TrustedRoots:            roots,
			Logger:                  wzl,
			DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
				DNSProvider: newDNS01Provider(api),
				Resolvers:   acme.DNSResolvers,
				Logger:      wzl,
			}},
		})}
		// El http.Server solo tiene UN tls.Config: se combinan las dos
		// búsquedas de certificado, probando primero la de hosts concretos.
		getCert, wildcardGetCert := tlsCfg.GetCertificate, wildcardCfg.TLSConfig().GetCertificate
		tlsCfg.GetCertificate = func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if cert, err := getCert(hello); err == nil {
				return cert, nil
			}
			return wildcardGetCert(hello)
		}
	}

	t.ln = newChanListener()
	t.srv = &http.Server{
		Handler:   http.HandlerFunc(t.serveHTTP),
		TLSConfig: tlsCfg,
		ErrorLog:  slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	// ManageAsync no bloquea el arranque: si ACME falla, el agente igual queda
	// operativo para el resto de las rutas y reintenta solo.
	if err := cfg.ManageAsync(ctx, normalHosts); err != nil {
		return nil, err
	}
	if wildcardCfg != nil {
		if err := wildcardCfg.ManageAsync(ctx, wildcardHosts); err != nil {
			return nil, err
		}
	}
	go t.srv.ServeTLS(t.ln, "", "")
	if len(proxies) > 0 {
		log.Info("terminando TLS (HTTP)", "hosts", strings.Join(sortedKeys(proxies), ", "))
	}
	if len(tcpTargets) > 0 {
		log.Info("terminando TLS (TCP)", "hosts", strings.Join(sortedKeys(tcpTargets), ", "))
	}
	return t, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func (t *terminator) serveHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// Match exacto primero y después comodín, igual que el resto del agente:
	// así una ruta "*" con certificado wildcard cubre cualquier subdominio.
	proxy, ok := sni.Match(t.proxies, host)
	if !ok {
		http.Error(w, "hostname no configurado en wgrelay.yml", http.StatusMisdirectedRequest)
		return
	}
	proxy.ServeHTTP(w, r)
}

// handle despacha una conexión ya aceptada: al servidor HTTPS interno si el
// host está en modo terminate http(s), o directo al backend TCP si es tcp://.
func (t *terminator) handle(conn net.Conn, host string) {
	if target, ok := sni.Match(t.tcpTargets, host); ok {
		t.handleTCP(conn, target)
		return
	}
	if !t.ln.push(conn) {
		conn.Close()
	}
}

// handleTCP hace el handshake TLS a mano (el mismo tls.Config que usa el
// servidor HTTP, así que ACME y TLS-ALPN-01 funcionan igual) y después copia
// bytes crudos hacia el backend: el agente termina TLS, pero no sabe ni le
// importa qué protocolo va adentro.
func (t *terminator) handleTCP(conn net.Conn, target string) {
	tlsConn := tls.Server(conn, t.tlsCfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		t.log.Debug("handshake TLS falló (terminate tcp)", "target", target, "err", err)
		conn.Close()
		return
	}
	up, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		t.log.Warn("no se pudo conectar al destino (terminate tcp)", "target", target, "err", err)
		tlsConn.Close()
		return
	}
	pipe.Join(tlsConn, up)
}

func (t *terminator) close() {
	if t == nil {
		return
	}
	t.ln.Close()
	t.srv.Close()
	t.cache.Stop()
	if t.wildcardCache != nil {
		t.wildcardCache.Stop()
	}
}

// ------------------------------------------------- listener alimentado a mano

// chanListener convierte conexiones que ya aceptamos nosotros en un
// net.Listener, para poder usar el http.Server de la biblioteca estándar.
type chanListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newChanListener() *chanListener {
	return &chanListener{conns: make(chan net.Conn), done: make(chan struct{})}
}

func (l *chanListener) push(c net.Conn) bool {
	select {
	case l.conns <- c:
		return true
	case <-l.done:
		return false
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (l *chanListener) Addr() net.Addr { return &net.TCPAddr{Port: listenPort} }

// addrConn reemplaza RemoteAddr por la IP real del visitante, que llegó en el
// header PROXY. Así el http.Server la pone en r.RemoteAddr y el proxy la
// reenvía en X-Forwarded-For.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (c addrConn) RemoteAddr() net.Addr { return c.remote }
func (c addrConn) Unwrap() net.Conn     { return c.Conn }

// ------------------------------------------------- puente de logs zap → slog

// certmagic loguea con zap; este core reenvía esos mensajes a nuestro slog
// para que el agente tenga una sola salida coherente.
type slogCore struct {
	log    *slog.Logger
	fields []zapcore.Field
}

func (c *slogCore) Enabled(zapcore.Level) bool { return true }

func (c *slogCore) With(fs []zapcore.Field) zapcore.Core {
	return &slogCore{log: c.log, fields: append(slices.Clip(c.fields), fs...)}
}

func (c *slogCore) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	return ce.AddCore(e, c)
}

func (c *slogCore) Write(e zapcore.Entry, fs []zapcore.Field) error {
	enc := zapcore.NewMapObjectEncoder()
	for _, f := range append(slices.Clip(c.fields), fs...) {
		f.AddTo(enc)
	}
	attrs := make([]any, 0, len(enc.Fields)*2)
	for k, v := range enc.Fields {
		attrs = append(attrs, k, v)
	}
	level := slog.LevelInfo
	switch {
	case e.Level <= zapcore.DebugLevel:
		level = slog.LevelDebug
	case e.Level == zapcore.WarnLevel:
		level = slog.LevelWarn
	case e.Level >= zapcore.ErrorLevel:
		level = slog.LevelError
	}
	c.log.Log(context.Background(), level, e.Message, attrs...)
	return nil
}

func (c *slogCore) Sync() error { return nil }
