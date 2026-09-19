package store

import (
	"net/netip"
	"testing"
)

func TestOffsetAddr(t *testing.T) {
	p := netip.MustParsePrefix("10.64.0.0/10")
	cases := map[int64]string{2: "10.64.0.2", 256: "10.64.1.0", 1<<22 - 1: "10.127.255.255"}
	for off, want := range cases {
		got, err := offsetAddr(p, off)
		if err != nil || got.String() != want {
			t.Errorf("offset %d = %v, %v; want %s", off, got, err, want)
		}
	}
	if _, err := offsetAddr(p, 1<<22); err == nil {
		t.Error("debería detectar pool agotado")
	}
}
