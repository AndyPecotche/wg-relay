package agent

import (
	"testing"

	"github.com/AndyPecotche/wg-relay/internal/sni"
)

// El despacho del agente usa el mismo matcher que el nodo: exacto primero,
// después comodín. Es lo que permite mandar todo a Caddy y desviar una
// excepción a otro puerto.
func TestRouteMatching(t *testing.T) {
	d := "abc.clients.example.com"
	routes, err := Resolve([]RouteSpec{
		{Host: "*", To: "caddy:443"},
		{Host: "mqtt", To: "caddy:8883"},
	}, d)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"mqtt." + d:      "caddy:8883",
		"emqx." + d:      "caddy:443",
		"a.b.c." + d:     "caddy:443",
		d:                "", // el comodín no cubre el dominio raíz (RFC 4592)
		"otrodominio.io": "",
	}
	for host, want := range cases {
		r, ok := sni.Match(routes, host)
		if want == "" {
			if ok {
				t.Errorf("%s: no debería matchear, dio %s", host, r.To)
			}
			continue
		}
		if !ok || r.To != want {
			t.Errorf("%s = %q,%v; want %q", host, r.To, ok, want)
		}
	}
}
