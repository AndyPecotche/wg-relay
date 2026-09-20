// wgrelay-api es el control plane. Sin argumentos (o con "serve") levanta la
// API; el resto de los subcomandos son tareas de administración:
//
//	wgrelay-api tunnel create --email ana@ejemplo.com [--plan free|persistent]
//	wgrelay-api tunnel list
//	wgrelay-api tunnel rotate-token --id 3
//	wgrelay-api node create --name node1 --endpoint node1.wg-relay.andy.net.ar:51820
//	wgrelay-api node list
//	wgrelay-api dns sync
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"

	"github.com/AndyPecotche/wg-relay/internal/api"
	"github.com/AndyPecotche/wg-relay/internal/buildinfo"
	"github.com/AndyPecotche/wg-relay/internal/cloudflare"
	"github.com/AndyPecotche/wg-relay/internal/dnsprovider"
	"github.com/AndyPecotche/wg-relay/internal/store"
)

type config struct {
	DatabaseURL string
	BaseDomain  string // dominio bajo el que viven los subdominios de clientes
	EdgeHost    string // nombre con registros A a todos los nodos (destino de los CNAME)
	Listen      string // HTTP plano, para nodos en la misma red interna
	APIDomain   string // si está definido, sirve HTTPS con certificado propio
	TLSListen   string
	ACMEEmail   string
	ACMECA      string
	DataDir     string

	// Proveedor de DNS que usa el control plane para su propia zona: el
	// CNAME por tunnel y el TXT del DNS-01 delegado (DESIGN.md §4.3, §6.2.1).
	// No confundir con el DNS de un dominio propio del cliente (§6.5, F2).
	DNSProviderName string // "cloudflare" | "webhook" | "" (ninguno: alta manual)
	CFToken         string
	CFZoneID        string
	CFBaseURL       string // solo para tests: apunta a un servidor que imite la API de Cloudflare
	DNSWebhookURL   string
	DNSWebhookToken string
}

func loadConfig() config {
	return config{
		DatabaseURL: env("WGRELAY_DATABASE_URL", ""),
		BaseDomain:  strings.TrimSuffix(env("WGRELAY_BASE_DOMAIN", ""), "."),
		EdgeHost:    strings.TrimSuffix(env("WGRELAY_EDGE_HOST", ""), "."),
		Listen:      env("WGRELAY_API_LISTEN", ":8080"),
		APIDomain:   env("WGRELAY_API_DOMAIN", ""),
		TLSListen:   env("WGRELAY_API_TLS_LISTEN", ":8443"),
		ACMEEmail:   env("WGRELAY_ACME_EMAIL", ""),
		ACMECA:      env("WGRELAY_ACME_CA", ""),
		DataDir:     env("WGRELAY_DATA_DIR", "/data"),

		DNSProviderName: env("WGRELAY_DNS_PROVIDER", ""),
		CFToken:         env("CLOUDFLARE_API_TOKEN", ""),
		CFZoneID:        env("CLOUDFLARE_ZONE_ID", ""),
		CFBaseURL:       env("CLOUDFLARE_API_BASE_URL", ""),
		DNSWebhookURL:   env("WGRELAY_DNS_WEBHOOK_URL", ""),
		DNSWebhookToken: env("WGRELAY_DNS_WEBHOOK_TOKEN", ""),
	}
}

// newDNSProvider elige la implementación según WGRELAY_DNS_PROVIDER. Vacío
// con CLOUDFLARE_API_TOKEN/ZONE_ID configurados se sigue leyendo como
// "cloudflare", por compatibilidad con despliegues que ya usaban esas
// variables antes de que existiera esta. nil significa "sin proveedor": el
// endpoint de DNS-01 delegado queda deshabilitado y `dns sync` imprime los
// registros para cargarlos a mano.
func newDNSProvider(cfg config) dnsprovider.Provider {
	name := cfg.DNSProviderName
	if name == "" && cfg.CFToken != "" && cfg.CFZoneID != "" {
		name = "cloudflare"
	}
	switch name {
	case "cloudflare":
		if cfg.CFToken == "" || cfg.CFZoneID == "" {
			return nil
		}
		return cloudflare.NewWithBaseURL(cfg.CFToken, cfg.CFZoneID, cfg.CFBaseURL)
	case "webhook":
		if cfg.DNSWebhookURL == "" {
			return nil
		}
		return dnsprovider.NewWebhook(cfg.DNSWebhookURL, cfg.DNSWebhookToken)
	default:
		return nil
	}
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg := loadConfig()
	if cfg.DatabaseURL == "" || cfg.BaseDomain == "" {
		fatal("WGRELAY_DATABASE_URL y WGRELAY_BASE_DOMAIN son obligatorias")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		fatal(err.Error())
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		fatal(err.Error())
	}

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"serve"}
	}
	switch strings.Join(args[:min(2, len(args))], " ") {
	case "serve":
		err = serve(ctx, cfg, st, log)
	case "tunnel create":
		err = tunnelCreate(ctx, cfg, st, args[2:])
	case "tunnel list":
		err = tunnelList(ctx, cfg, st)
	case "tunnel rotate-token":
		err = tunnelRotate(ctx, st, args[2:])
	case "node create":
		err = nodeCreate(ctx, st, args[2:])
	case "node list":
		err = nodeList(ctx, st)
	case "dns sync":
		err = dnsSync(ctx, cfg, st)
	default:
		fatal("comando desconocido: " + strings.Join(args, " ") + "\n\n" + usage)
	}
	if err != nil {
		fatal(err.Error())
	}
}

const usage = `uso:
  wgrelay-api [serve]
  wgrelay-api tunnel create --email EMAIL [--plan free|persistent]
  wgrelay-api tunnel list
  wgrelay-api tunnel rotate-token --id ID
  wgrelay-api node create --name NOMBRE --endpoint HOST:51820
  wgrelay-api node list
  wgrelay-api dns sync`

func serve(ctx context.Context, cfg config, st *store.Store, log *slog.Logger) error {
	dp := newDNSProvider(cfg)
	if dp == nil {
		log.Warn("ningún proveedor de DNS configurado (WGRELAY_DNS_PROVIDER, o CLOUDFLARE_API_TOKEN/CLOUDFLARE_ZONE_ID): el endpoint de DNS-01 delegado (certificados wildcard) queda deshabilitado")
	}
	h := (&api.Server{Store: st, BaseDomain: cfg.BaseDomain, Log: log, DNSProvider: dp}).Handler()
	servers := []*http.Server{{Addr: cfg.Listen, Handler: h, ReadHeaderTimeout: 10 * time.Second}}

	if cfg.APIDomain != "" {
		// Certificado propio de la API vía TLS-ALPN-01. El nodo reenvía el SNI
		// de la API a este listener sin tocar el TLS, así que el challenge
		// llega por el mismo :443 público.
		m := &autocert.Manager{
			Prompt:     autocert.AcceptTOS,
			HostPolicy: autocert.HostWhitelist(cfg.APIDomain),
			Cache:      autocert.DirCache(filepath.Join(cfg.DataDir, "acme")),
			Email:      cfg.ACMEEmail,
		}
		if cfg.ACMECA != "" {
			m.Client = &acme.Client{DirectoryURL: cfg.ACMECA}
		}
		tlsCfg := m.TLSConfig()
		tlsCfg.MinVersion = tls.VersionTLS12
		servers = append(servers, &http.Server{Addr: cfg.TLSListen, Handler: h, TLSConfig: tlsCfg, ReadHeaderTimeout: 10 * time.Second})
	}

	errc := make(chan error, len(servers))
	for _, srv := range servers {
		go func() {
			log.Info("API escuchando", "addr", srv.Addr, "tls", srv.TLSConfig != nil, "version", buildinfo.Version)
			var err error
			if srv.TLSConfig != nil {
				err = srv.ListenAndServeTLS("", "")
			} else {
				err = srv.ListenAndServe()
			}
			if !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		srv.Shutdown(shutdown)
	}
	return nil
}

func tunnelCreate(ctx context.Context, cfg config, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("tunnel create", flag.ExitOnError)
	email := fs.String("email", "", "email de la cuenta (se crea si no existe)")
	plan := fs.String("plan", "persistent", "plan de la cuenta: free | persistent")
	fs.Parse(args)
	if *email == "" {
		return errors.New("--email es obligatorio")
	}
	t, tok, err := st.CreateTunnel(ctx, *email, *plan)
	if err != nil {
		return err
	}
	domain := t.Subdomain + "." + cfg.BaseDomain
	fmt.Printf("Tunnel creado\n  id:        %d\n  cuenta:    %s (%s)\n  dominio:   %s\n  ip vpn:    %s\n\n", t.ID, t.Email, t.Plan, domain, t.VPNIP)
	fmt.Printf("Token (se muestra UNA sola vez, guardalo ahora):\n\n  %s\n\n", tok)
	return ensureDNS(ctx, cfg, []string{domain})
}

func tunnelList(ctx context.Context, cfg config, st *store.Store) error {
	ts, err := st.ListTunnels(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCUENTA\tPLAN\tDOMINIO\tIP VPN\tONLINE")
	for _, t := range ts {
		fmt.Fprintf(w, "%d\t%s\t%s\t%s.%s\t%s\t%v\n", t.ID, t.Email, t.Plan, t.Subdomain, cfg.BaseDomain, t.VPNIP, t.Online)
	}
	return w.Flush()
}

func tunnelRotate(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("tunnel rotate-token", flag.ExitOnError)
	id := fs.Int64("id", 0, "id del tunnel")
	fs.Parse(args)
	tok, err := st.RotateToken(ctx, *id)
	if err != nil {
		return err
	}
	fmt.Printf("Token nuevo (el anterior ya no funciona):\n\n  %s\n\n", tok)
	return nil
}

func nodeCreate(ctx context.Context, st *store.Store, args []string) error {
	fs := flag.NewFlagSet("node create", flag.ExitOnError)
	name := fs.String("name", "", "nombre del nodo, ej. node1")
	endpoint := fs.String("endpoint", "", "host:puerto UDP público de WireGuard, ej. node1.wg-relay.andy.net.ar:51820")
	fs.Parse(args)
	if *name == "" || *endpoint == "" {
		return errors.New("--name y --endpoint son obligatorios")
	}
	n, tok, err := st.CreateNode(ctx, *name, *endpoint)
	if err != nil {
		return err
	}
	fmt.Printf("Nodo creado\n  nombre:    %s\n  endpoint:  %s\n  gateway:   %s\n\n", n.Name, n.Endpoint, n.GatewayIP)
	fmt.Printf("Token del nodo (se muestra UNA sola vez) → WGRELAY_NODE_TOKEN:\n\n  %s\n\n", tok)
	return nil
}

func nodeList(ctx context.Context, st *store.Store) error {
	ns, err := st.ListNodes(ctx)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tNOMBRE\tENDPOINT\tGATEWAY\tÚLTIMO CONTACTO")
	for _, n := range ns {
		seen := "nunca"
		if n.LastSeen != nil {
			seen = time.Since(*n.LastSeen).Round(time.Second).String() + " atrás"
		}
		fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", n.ID, n.Name, n.Endpoint, n.GatewayIP, seen)
	}
	return w.Flush()
}

func dnsSync(ctx context.Context, cfg config, st *store.Store) error {
	ts, err := st.ListTunnels(ctx)
	if err != nil {
		return err
	}
	domains := make([]string, len(ts))
	for i, t := range ts {
		domains[i] = t.Subdomain + "." + cfg.BaseDomain
	}
	return ensureDNS(ctx, cfg, domains)
}

// ensureDNS crea "<dominio>" y "*.<dominio>" como CNAME al edge. Los registros
// son explícitos por tunnel (no un comodín global) por RFC 4592: el TXT de
// ACME bajo el dominio del cliente anularía un comodín superior.
func ensureDNS(ctx context.Context, cfg config, domains []string) error {
	dp := newDNSProvider(cfg)
	if dp == nil || cfg.EdgeHost == "" {
		fmt.Println("Proveedor de DNS no configurado: creá estos registros a mano (sin proxy, nube gris):")
		for _, d := range domains {
			fmt.Printf("  %-50s CNAME  %s\n  %-50s CNAME  %s\n", d, cfg.EdgeHost, "*."+d, cfg.EdgeHost)
		}
		return nil
	}
	for _, d := range domains {
		for _, name := range []string{d, "*." + d} {
			if err := dp.EnsureCNAME(ctx, name, cfg.EdgeHost); err != nil {
				return fmt.Errorf("DNS %s: %w", name, err)
			}
			fmt.Printf("DNS ok: %s → %s\n", name, cfg.EdgeHost)
		}
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "error:", msg)
	os.Exit(1)
}
