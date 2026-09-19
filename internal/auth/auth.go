// Package auth genera y verifica los tokens de agentes y nodos.
//
// Formato: <prefijo>_<id>_<secreto>. El id permite buscar el registro sin
// recorrer la tabla; del secreto solo se guarda SHA-256. Los secretos son 256
// bits aleatorios, así que un hash lento (Argon2) no agrega seguridad: eso
// sirve para contraseñas elegidas por humanos, no para tokens de alta entropía.
package auth

import (
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"strings"
)

const (
	PrefixAgent = "wgr"
	PrefixNode  = "wgn"
)

var (
	enc          = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)
	ErrMalformed = errors.New("token con formato inválido")
)

type Token struct {
	Prefix string
	ID     string
	Secret string
}

func (t Token) String() string { return t.Prefix + "_" + t.ID + "_" + t.Secret }

// New genera un token nuevo con el prefijo dado.
func New(prefix string) Token {
	return Token{Prefix: prefix, ID: Random(8), Secret: Random(32)}
}

// Random devuelve n bytes aleatorios codificados en base32 minúscula.
func Random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return enc.EncodeToString(b)
}

func Parse(s, wantPrefix string) (Token, error) {
	parts := strings.Split(strings.TrimSpace(s), "_")
	if len(parts) != 3 || parts[0] != wantPrefix || parts[1] == "" || parts[2] == "" {
		return Token{}, ErrMalformed
	}
	return Token{Prefix: parts[0], ID: parts[1], Secret: parts[2]}, nil
}

func Hash(t Token) []byte {
	h := sha256.Sum256([]byte(t.String()))
	return h[:]
}

func Verify(t Token, stored []byte) bool {
	return subtle.ConstantTimeCompare(Hash(t), stored) == 1
}

// DeriveKey deriva 32 bytes determinísticos del token para un propósito dado.
// El servidor no puede calcularlos: solo guarda el hash del token.
func DeriveKey(t Token, purpose string) [32]byte {
	k, err := hkdf.Key(sha256.New, []byte(t.String()), nil, "wg-relay "+purpose, 32)
	if err != nil {
		panic(err)
	}
	var out [32]byte
	copy(out[:], k)
	return out
}
