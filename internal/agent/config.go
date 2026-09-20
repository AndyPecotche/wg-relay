package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// File es el contenido de wgrelay.yml. Está pensado para versionarse junto al
// proyecto del usuario: no contiene secretos (el token va por WGRELAY_TOKEN).
type File struct {
	Relay  string      `yaml:"relay"`
	ACME   ACME        `yaml:"acme"`
	Routes []RouteSpec `yaml:"routes"`
}

// ACME configura la obtención de certificados para las rutas en modo terminate.
type ACME struct {
	// Email de contacto para la CA. Opcional, pero recomendado: es por donde
	// Let's Encrypt avisa si un certificado está por vencer sin renovarse.
	Email string `yaml:"email"`
	// CA alternativa. Para probar sin gastar cuota:
	// https://acme-staging-v02.api.letsencrypt.org/directory
	CA string `yaml:"ca"`
	// Ruta a un PEM con las raíces que se confían al hablar con la CA. Solo
	// hace falta con una CA privada (step-ca, Pebble, PKI interna).
	TrustedRoots string `yaml:"trusted_roots"`
	// Resolvers DNS a usar para propagación y verificación de zona en
	// DNS-01 (host:puerto). Vacío usa los del sistema. Solo hace falta en
	// redes con DNS split-horizon, o para pruebas con un servidor propio.
	DNSResolvers []string `yaml:"dns_resolvers"`
}

const LetsEncryptProduction = "https://acme-v02.api.letsencrypt.org/directory"

func (a ACME) CAOrDefault() string {
	if a.CA != "" {
		return a.CA
	}
	return LetsEncryptProduction
}

type RouteSpec struct {
	// Host relativo al dominio asignado ("mqtt" → mqtt.<dominio>), "@" para el
	// dominio mismo, "*" para todo lo demás, o un FQDN (o comodín "*.x") que
	// caiga dentro del dominio asignado.
	Host          string `yaml:"host"`
	Mode          string `yaml:"mode"` // passthrough | terminate
	To            string `yaml:"to"`   // host:puerto alcanzable desde el agente
	ProxyProtocol bool   `yaml:"proxy_protocol"`
	// ExportCert, solo válido en passthrough: el agente obtiene el
	// certificado (por DNS-01, el único desafío que puede resolver una ruta
	// que no termina) y deja fullchain.pem/privkey.pem en este directorio,
	// reescribiéndolos en cada renovación.
	ExportCert string `yaml:"export_cert"`
}

const (
	ModePassthrough = "passthrough"
	ModeTerminate   = "terminate"
)

func LoadFile(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}
	var f File
	dec := yaml.NewDecoder(bytes.NewReader([]byte(os.ExpandEnv(string(raw)))))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		if strings.Contains(err.Error(), "field token") {
			return File{}, errors.New("el token no va en el archivo (se versiona): usá la variable WGRELAY_TOKEN")
		}
		return File{}, fmt.Errorf("%s: %w", path, err)
	}
	for i := range f.Routes {
		r := &f.Routes[i]
		if r.Mode == "" {
			r.Mode = ModeTerminate
		}
		switch {
		case r.Host == "":
			return File{}, fmt.Errorf("ruta %d: falta host", i+1)
		case r.Mode == ModeTerminate:
			// En terminate el destino es una URL. Sin esquema se asume HTTP,
			// que es el caso más común (paneles web, APIs). "tcp://" es para
			// protocolos que no son HTTP: el agente termina el TLS igual,
			// pero entrega bytes crudos al backend en vez de proxear HTTP.
			if !strings.Contains(r.To, "://") {
				r.To = "http://" + r.To
			}
			u, err := url.Parse(r.To)
			if err != nil || u.Host == "" {
				return File{}, fmt.Errorf("ruta %q: to debe ser una URL http(s) o tcp, por ejemplo http://influxdb:8086 o tcp://emqx:1883", r.Host)
			}
			switch u.Scheme {
			case "http", "https", "tcp":
			default:
				return File{}, fmt.Errorf("ruta %q: esquema %q no soportado en modo terminate (usá http://, https:// o tcp://)", r.Host, u.Scheme)
			}
		case r.Mode == ModePassthrough:
			if _, _, err := net.SplitHostPort(r.To); err != nil {
				return File{}, fmt.Errorf("ruta %q: to debe ser host:puerto (%v)", r.Host, err)
			}
		default:
			return File{}, fmt.Errorf("ruta %q: modo %q desconocido", r.Host, r.Mode)
		}
		if r.ExportCert != "" && r.Mode != ModePassthrough {
			return File{}, fmt.Errorf("ruta %q: export_cert solo es válido en mode: passthrough (en terminate el agente ya usa el certificado, no hace falta exportarlo)", r.Host)
		}
	}
	return f, nil
}

// Resolve convierte los hosts de las rutas en FQDN dentro de domain.
func Resolve(specs []RouteSpec, domain string) (map[string]RouteSpec, error) {
	out := make(map[string]RouteSpec, len(specs))
	for _, r := range specs {
		h := strings.ToLower(strings.TrimSuffix(r.Host, "."))
		// "@" es el dominio mismo; un nombre sin puntos es relativo a él; con
		// puntos se toma como FQDN y tiene que caer dentro del dominio. La
		// misma regla vale para comodines: "*" y "*.dev" son relativos.
		star := ""
		switch {
		case h == "*":
			star, h = "*.", domain
		case strings.HasPrefix(h, "*."):
			if rest := h[2:]; rest == "" || rest == "@" {
				return nil, fmt.Errorf("ruta %q: comodín inválido", r.Host)
			} else {
				star, h = "*.", rest
			}
		}
		switch {
		case h == "@":
			h = domain
		case !strings.Contains(h, "."):
			h = h + "." + domain
		case h != domain && !strings.HasSuffix(h, "."+domain):
			return nil, fmt.Errorf("ruta %q: no pertenece al dominio asignado %s (dominios propios: próximamente)", r.Host, domain)
		}
		h = star + h
		if _, dup := out[h]; dup {
			return nil, fmt.Errorf("ruta %q duplicada", h)
		}
		out[h] = r
	}
	return out, nil
}
