package proxyproto

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	cases := []struct{ src, dst string }{
		{"203.0.113.7:51234", "198.51.100.1:443"},
		{"[2001:db8::1]:40000", "[2001:db8::2]:443"},
		{"[::ffff:203.0.113.7]:1", "198.51.100.1:443"}, // v4 mapeada → v4
	}
	for _, c := range cases {
		src, dst := netip.MustParseAddrPort(c.src), netip.MustParseAddrPort(c.dst)
		h := Header(src, dst)
		payload := []byte("resto de la conexión")
		gs, gd, raw, err := Read(bytes.NewReader(append(h, payload...)))
		if err != nil {
			t.Fatal(err)
		}
		if gs.Addr() != src.Addr().Unmap() || gs.Port() != src.Port() || gd.Addr() != dst.Addr().Unmap() {
			t.Errorf("%s -> %s: got %s -> %s", c.src, c.dst, gs, gd)
		}
		if !bytes.Equal(raw, h) {
			t.Error("raw no coincide con el header emitido")
		}
	}
}

func TestRejectsGarbage(t *testing.T) {
	if _, _, _, err := Read(bytes.NewReader([]byte("\x16\x03\x01 no es un header PROXY"))); err == nil {
		t.Fatal("debería fallar")
	}
}
