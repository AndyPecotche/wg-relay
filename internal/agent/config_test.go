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
		"token":     "token: wgr_x_y\n",
		"terminate": "routes:\n  - host: web\n    to: web:80\n",
		"to":        "routes:\n  - host: x\n    mode: passthrough\n    to: sinpuerto\n",
		"desconoc":  "routes:\n  - host: x\n    mode: raro\n    to: a:1\n",
	}
	for want, body := range cases {
		_, err := LoadFile(write(t, body))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v", want, err)
		}
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
