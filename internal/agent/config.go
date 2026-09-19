package agent

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// File es el contenido de wgrelay.yml. Está pensado para versionarse junto al
// proyecto del usuario: no contiene secretos (el token va por WGRELAY_TOKEN).
type File struct {
	Relay  string      `yaml:"relay"`
	Routes []RouteSpec `yaml:"routes"`
}

type RouteSpec struct {
	// Host relativo al dominio asignado ("mqtt" → mqtt.<dominio>), "@" para el
	// dominio mismo, o un FQDN que caiga dentro del dominio asignado.
	Host          string `yaml:"host"`
	Mode          string `yaml:"mode"` // passthrough | terminate
	To            string `yaml:"to"`   // host:puerto alcanzable desde el agente
	ProxyProtocol bool   `yaml:"proxy_protocol"`
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
			return File{}, fmt.Errorf("ruta %q: el modo terminate todavía no está implementado; usá mode: passthrough", r.Host)
		case r.Mode != ModePassthrough:
			return File{}, fmt.Errorf("ruta %q: modo %q desconocido", r.Host, r.Mode)
		}
		if _, _, err := net.SplitHostPort(r.To); err != nil {
			return File{}, fmt.Errorf("ruta %q: to debe ser host:puerto (%v)", r.Host, err)
		}
	}
	return f, nil
}

// Resolve convierte los hosts de las rutas en FQDN dentro de domain.
func Resolve(specs []RouteSpec, domain string) (map[string]RouteSpec, error) {
	out := make(map[string]RouteSpec, len(specs))
	for _, r := range specs {
		h := strings.ToLower(strings.TrimSuffix(r.Host, "."))
		switch {
		case h == "@":
			h = domain
		case !strings.Contains(h, "."):
			h = h + "." + domain
		case h != domain && !strings.HasSuffix(h, "."+domain):
			return nil, fmt.Errorf("ruta %q: no pertenece al dominio asignado %s (dominios propios: próximamente)", r.Host, domain)
		}
		if _, dup := out[h]; dup {
			return nil, fmt.Errorf("ruta %q duplicada", h)
		}
		out[h] = r
	}
	return out, nil
}
