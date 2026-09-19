package auth

import "testing"

func TestRoundTrip(t *testing.T) {
	tok := New(PrefixAgent)
	got, err := Parse(tok.String(), PrefixAgent)
	if err != nil {
		t.Fatal(err)
	}
	if got != tok {
		t.Fatalf("got %v want %v", got, tok)
	}
	if !Verify(got, Hash(tok)) {
		t.Fatal("verify falló con el token correcto")
	}
	other := New(PrefixAgent)
	if Verify(other, Hash(tok)) {
		t.Fatal("verify aceptó otro token")
	}
}

func TestParseRejects(t *testing.T) {
	tok := New(PrefixNode)
	for _, s := range []string{"", "wgr", "wgr__x", "wgr_a_", tok.String()} {
		if _, err := Parse(s, PrefixAgent); err == nil {
			t.Errorf("Parse(%q) debería fallar", s)
		}
	}
}

func TestDeriveKeyStable(t *testing.T) {
	tok := New(PrefixNode)
	if DeriveKey(tok, "a") != DeriveKey(tok, "a") {
		t.Fatal("no determinístico")
	}
	if DeriveKey(tok, "a") == DeriveKey(tok, "b") {
		t.Fatal("propósitos distintos dieron la misma clave")
	}
}
