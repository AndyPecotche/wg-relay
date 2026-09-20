package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/caddyserver/certmagic"
	"go.uber.org/zap"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
)

// exporter obtiene certificados para las rutas passthrough con export_cert y
// deja fullchain.pem/privkey.pem en el directorio que pidió cada una.
//
// Usa un certmagic.Config aparte del terminador, con HTTP-01 y TLS-ALPN-01
// deshabilitados: en passthrough el TLS viaja intacto hasta el backend del
// usuario, así que un desafío de esos dos tipos nunca llegaría al agente.
// DNS-01 es el único que puede resolverse acá, sea el host wildcard o no.
type exporter struct {
	cache *certmagic.Cache
}

func newExporter(ctx context.Context, routes map[string]RouteSpec, st certmagic.Storage, api *apiclient.Client, acme ACME, log *slog.Logger) (*exporter, error) {
	dirsByHost := map[string][]string{}
	for host, r := range routes {
		if r.Mode == ModePassthrough && r.ExportCert != "" {
			dirsByHost[host] = append(dirsByHost[host], r.ExportCert)
		}
	}
	if len(dirsByHost) == 0 {
		return nil, nil
	}
	hosts := make([]string, 0, len(dirsByHost))
	for h := range dirsByHost {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)

	roots, err := loadTrustedRoots(acme.TrustedRoots)
	if err != nil {
		return nil, err
	}
	zl := zap.New(&slogCore{log: log.With("component", "acme-export")})
	cache := certmagic.NewCache(certmagic.CacheOptions{
		GetConfigForCert: func(certmagic.Certificate) (*certmagic.Config, error) {
			return certmagic.New(nil, certmagic.Config{Storage: st, Logger: zl}), nil
		},
		Logger: zl,
	})
	cfg := certmagic.New(cache, certmagic.Config{
		Storage: st,
		Logger:  zl,
		OnEvent: func(ctx context.Context, event string, data map[string]any) error {
			if event != "cert_obtained" {
				return nil
			}
			return exportOnEvent(ctx, st, dirsByHost, data, log)
		},
	})
	cfg.Issuers = []certmagic.Issuer{certmagic.NewACMEIssuer(cfg, certmagic.ACMEIssuer{
		CA:                      acme.CAOrDefault(),
		Email:                   acme.Email,
		Agreed:                  true,
		DisableHTTPChallenge:    true,
		DisableTLSALPNChallenge: true,
		TrustedRoots:            roots,
		Logger:                  zl,
		DNS01Solver: &certmagic.DNS01Solver{DNSManager: certmagic.DNSManager{
			DNSProvider: newDNS01Provider(api),
			Resolvers:   acme.DNSResolvers,
			Logger:      zl,
		}},
	})}
	if err := cfg.ManageAsync(ctx, hosts); err != nil {
		return nil, fmt.Errorf("export_cert: %w", err)
	}
	log.Info("exportando certificados (DNS-01)", "hosts", strings.Join(hosts, ", "))
	return &exporter{cache: cache}, nil
}

// exportOnEvent lee de storage el certificado recién obtenido o renovado y lo
// escribe en cada directorio pedido para ese host.
func exportOnEvent(ctx context.Context, st certmagic.Storage, dirsByHost map[string][]string, data map[string]any, log *slog.Logger) error {
	host, _ := data["identifier"].(string)
	dirs := dirsByHost[host]
	if len(dirs) == 0 {
		return nil // certificado de otro host manejado por el mismo cache (ver GetConfigForCert)
	}
	certPath, _ := data["certificate_path"].(string)
	keyPath, _ := data["private_key_path"].(string)
	certPEM, err := st.Load(ctx, certPath)
	if err != nil {
		return fmt.Errorf("export_cert: leyendo certificado de %s: %w", host, err)
	}
	keyPEM, err := st.Load(ctx, keyPath)
	if err != nil {
		return fmt.Errorf("export_cert: leyendo clave de %s: %w", host, err)
	}
	for _, dir := range dirs {
		if err := writeCertFiles(dir, certPEM, keyPEM); err != nil {
			return fmt.Errorf("export_cert: escribiendo en %s: %w", dir, err)
		}
		log.Info("certificado exportado", "host", host, "dir", dir)
	}
	return nil
}

func writeCertFiles(dir string, certPEM, keyPEM []byte) error {
	// El agente corre sin privilegios (UID 65532 en la imagen distroless): si
	// el volumen es nuevo, alguien tiene que darle permiso de escritura antes
	// de montarlo (ver docs/DEPLOY.md).
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("%w (¿el volumen le da permiso de escritura al UID 65532?)", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fullchain.pem"), certPEM, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "privkey.pem"), keyPEM, 0o600)
}

func (e *exporter) close() {
	if e == nil {
		return
	}
	e.cache.Stop()
}
