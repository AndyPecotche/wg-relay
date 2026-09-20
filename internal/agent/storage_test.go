package agent

import (
	"reflect"
	"testing"

	"github.com/AndyPecotche/wg-relay/internal/auth"
)

func TestChildKeys(t *testing.T) {
	keys := []string{
		"certificates",
		"certificatesX/otro", // no es hijo de "certificates"
		"certificates/le/a.com/a.crt",
		"certificates/le/a.com/a.key",
		"certificates/le/b.com/b.crt",
		"acme/le/users/me/me.json",
	}
	cases := []struct {
		prefix    string
		recursive bool
		want      []string
	}{
		{"certificates", false, []string{"certificates/le"}},
		{"certificates/le", false, []string{"certificates/le/a.com", "certificates/le/b.com"}},
		{"certificates/le/a.com", true, []string{"certificates/le/a.com/a.crt", "certificates/le/a.com/a.key"}},
		{"certificates/le/a.com/", false, []string{"certificates/le/a.com/a.crt", "certificates/le/a.com/a.key"}},
		{"", false, []string{"certificates", "certificatesX", "acme"}},
		{"nada", true, nil},
		{"certificates/le/a.com/a.crt", true, nil}, // una hoja no tiene hijos
	}
	for _, c := range cases {
		got := childKeys(keys, c.prefix, c.recursive)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("childKeys(%q, recursive=%v) = %v; want %v", c.prefix, c.recursive, got, c.want)
		}
	}
}

func TestSealOpen(t *testing.T) {
	st := testStorage(t)
	blob := st.seal("certificates/a.crt", []byte("clave privada"))
	if string(blob) == "clave privada" {
		t.Fatal("el valor no quedó cifrado")
	}
	got, err := st.open("certificates/a.crt", blob)
	if err != nil || string(got) != "clave privada" {
		t.Fatalf("open = %q, %v", got, err)
	}
	// El nombre de la clave está autenticado: no se puede mover un blob.
	if _, err := st.open("certificates/otra.crt", blob); err == nil {
		t.Error("descifró con otra clave de almacén")
	}
}

func TestDistintoTokenNoDescifra(t *testing.T) {
	a, b := testStorage(t), testStorage(t)
	if _, err := b.open("k", a.seal("k", []byte("secreto"))); err == nil {
		t.Error("un token distinto pudo descifrar")
	}
}

func testStorage(t *testing.T) *storage {
	t.Helper()
	st, err := newStorage(nil, auth.New(auth.PrefixAgent))
	if err != nil {
		t.Fatal(err)
	}
	return st
}
