package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, s string) string {
	p := filepath.Join(t.TempDir(), "wgrelay.yml")
	os.WriteFile(p, []byte(s), 0o600)
	return p
}

func TestLoadFile(t *testing.T) {
	t.Setenv("BROKER", "emqx")
	f, err := LoadFile(write(t, `
relay: https://api.example.com
routes:
  - host: mqtt
    mode: passthrough
    to: ${BROKER}:8883
`))
	if err != nil {
		t.Fatal(err)
	}
	if f.Routes[0].To != "emqx:8883" {
		t.Fatalf("to = %q", f.Routes[0].To)
	}
}

func TestLoadFileErrors(t *testing.T) {
	cases := map[string]string{
		"token":        "token: wgr_x_y\n",
		"no soportado": "routes:\n  - host: web\n    to: ftp://web\n",
		"to":           "routes:\n  - host: x\n    mode: passthrough\n    to: sinpuerto\n",
		"desconoc":     "routes:\n  - host: x\n    mode: raro\n    to: a:1\n",
	}
	for want, body := range cases {
		_, err := LoadFile(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
	}
}

// terminate es el modo por defecto y su destino se normaliza a una URL.
func TestLoadFileTerminate(t *testing.T) {
	f, err := LoadFile(write(t, "routes:\n  - host: influx\n    to: influxdb:8086\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Routes[0].Mode != ModeTerminate || f.Routes[0].To != "http://influxdb:8086" {
		t.Fatalf("modo=%q to=%q", f.Routes[0].Mode, f.Routes[0].To)
	}
}

// tcp:// en modo terminate: el agente termina TLS y entrega bytes crudos.
func TestLoadFileTerminateTCP(t *testing.T) {
	f, err := LoadFile(write(t, "routes:\n  - host: mqtt\n    to: tcp://emqx:1883\n"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Routes[0].Mode != ModeTerminate || f.Routes[0].To != "tcp://emqx:1883" {
		t.Fatalf("modo=%q to=%q", f.Routes[0].Mode, f.Routes[0].To)
	}
}

func TestLoadFileTerminateSchemeInvalido(t *testing.T) {
	_, err := LoadFile(write(t, "routes:\n  - host: x\n    to: ftp://a:1\n"))
	if err == nil || !strings.Contains(err.Error(), "no soportado") {
		t.Fatalf("err = %v", err)
	}
}

func TestResolve(t *testing.T) {
	d := "abc.clients.example.com"
	got, err := Resolve([]RouteSpec{{Host: "mqtt"}, {Host: "@"}, {Host: "a.b." + d}}, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"mqtt." + d, d, "a.b." + d} {
		if _, ok := got[h]; !ok {
			t.Errorf("falta %s", h)
		}
	}
	if _, err := Resolve([]RouteSpec{{Host: "evil.com"}}, d); err == nil {
		t.Error("debería rechazar dominios ajenos")
	}
	if _, err := Resolve([]RouteSpec{{Host: "x"}, {Host: "X"}}, d); err == nil {
		t.Error("debería rechazar duplicados")
	}
}

func TestResolveWildcard(t *testing.T) {
	d := "abc.clients.example.com"
	got, err := Resolve([]RouteSpec{{Host: "*"}, {Host: "mqtt"}, {Host: "*.dev." + d}, {Host: "*.stage"}}, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"*." + d, "mqtt." + d, "*.dev." + d, "*.stage." + d} {
		if _, ok := got[h]; !ok {
			t.Errorf("falta %s en %v", h, got)
		}
	}
	if _, err := Resolve([]RouteSpec{{Host: "*.ajeno.com"}}, d); err == nil {
		t.Error("debería rechazar un comodín fuera del dominio")
	}
}

func TestResolveWildcardErrors(t *testing.T) {
	d := "abc.clients.example.com"
	for _, h := range []string{"*.@", "*.ajeno.com"} {
		if _, err := Resolve([]RouteSpec{{Host: h}}, d); err == nil {
			t.Errorf("Resolve(%q) debería fallar", h)
		}
	}
}
