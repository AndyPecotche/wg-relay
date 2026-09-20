package agent

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/caddyserver/certmagic"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
	"github.com/AndyPecotche/wg-relay/internal/auth"
	"github.com/AndyPecotche/wg-relay/internal/proto"
)

// storage implementa certmagic.Storage contra el control plane, cifrando todo
// del lado del agente.
//
// El agente no tiene disco, pero tampoco puede reemitir certificados en cada
// arranque: Let's Encrypt permite 5 idénticos por semana. Guardarlos en el
// servidor resuelve eso sin volúmenes, y el cifrado evita que el servicio
// pueda leer las claves privadas de sus clientes: la clave se deriva del
// token, del que el control plane solo conoce el hash SHA-256.
type storage struct {
	api  *apiclient.Client
	aead cipher.AEAD

	mu    sync.Mutex
	locks map[string]chan struct{}
}

func newStorage(api *apiclient.Client, tok auth.Token) (*storage, error) {
	key := auth.DeriveKey(tok, "cert storage")
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &storage{api: api, aead: aead, locks: map[string]chan struct{}{}}, nil
}

func (s *storage) path(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/v1/agent/storage/" + strings.Join(parts, "/")
}

// El nombre de la clave va como datos autenticados: un blob no puede moverse
// de una clave a otra sin que el descifrado falle.
func (s *storage) seal(key string, plain []byte) []byte {
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, plain, []byte(key))
}

func (s *storage) open(key string, blob []byte) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(blob) < n {
		return nil, fmt.Errorf("blob de %q demasiado corto", key)
	}
	plain, err := s.aead.Open(nil, blob[:n], blob[n:], []byte(key))
	if err != nil {
		return nil, fmt.Errorf("no se pudo descifrar %q (¿el token cambió?): %w", key, err)
	}
	return plain, nil
}

// notExist traduce el 404 de la API a lo que espera certmagic.
func notExist(err error) error {
	if apiclient.Code(err) == proto.ErrNotFound {
		return fs.ErrNotExist
	}
	return err
}

func (s *storage) Store(ctx context.Context, key string, value []byte) error {
	_, _, err := s.api.Raw(ctx, http.MethodPut, s.path(key), s.seal(key, value))
	return err
}

func (s *storage) Load(ctx context.Context, key string) ([]byte, error) {
	blob, _, err := s.api.Raw(ctx, http.MethodGet, s.path(key), nil)
	if err != nil {
		return nil, notExist(err)
	}
	return s.open(key, blob)
}

func (s *storage) Delete(ctx context.Context, key string) error {
	_, _, err := s.api.Raw(ctx, http.MethodDelete, s.path(key), nil)
	return notExist(err)
}

func (s *storage) Exists(ctx context.Context, key string) bool {
	_, _, err := s.api.Raw(ctx, http.MethodHead, s.path(key), nil)
	return err == nil
}

func (s *storage) Stat(ctx context.Context, key string) (certmagic.KeyInfo, error) {
	_, hdr, err := s.api.Raw(ctx, http.MethodHead, s.path(key), nil)
	if err != nil {
		return certmagic.KeyInfo{}, notExist(err)
	}
	modified, _ := http.ParseTime(hdr.Get("Last-Modified"))
	var size int64
	fmt.Sscan(hdr.Get("Content-Length"), &size)
	return certmagic.KeyInfo{Key: key, Modified: modified, Size: size, IsTerminal: true}, nil
}

// List devuelve las claves bajo prefix. Con recursive=false solo los hijos
// inmediatos, como hace el almacén en disco de certmagic.
func (s *storage) List(ctx context.Context, prefix string, recursive bool) ([]string, error) {
	body, _, err := s.api.Raw(ctx, http.MethodGet, "/v1/agent/storage?prefix="+url.QueryEscape(prefix), nil)
	if err != nil {
		return nil, notExist(err)
	}
	var list proto.StorageList
	if err := json.Unmarshal(body, &list); err != nil {
		return nil, err
	}
	keys := make([]string, len(list.Items))
	for i, it := range list.Items {
		keys[i] = it.Key
	}
	out := childKeys(keys, prefix, recursive)
	if len(out) == 0 {
		return nil, fs.ErrNotExist
	}
	return out, nil
}

// childKeys filtra las claves que cuelgan de prefix. El servidor hace un
// starts_with textual, así que acá se exige además que el corte caiga en un
// separador: "certificatesX" no es hijo de "certificates".
func childKeys(keys []string, prefix string, recursive bool) []string {
	base := strings.TrimSuffix(prefix, "/")
	seen := map[string]bool{}
	var out []string
	for _, k := range keys {
		var rest string
		switch {
		case base == "":
			rest = k
		case strings.HasPrefix(k, base+"/"):
			rest = k[len(base)+1:]
		default:
			continue // incluye k == base: el directorio no se lista a sí mismo
		}
		if rest == "" {
			continue
		}
		key := k
		if !recursive {
			if i := strings.IndexByte(rest, '/'); i >= 0 {
				rest = rest[:i]
			}
			if base == "" {
				key = rest
			} else {
				key = base + "/" + rest
			}
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// Lock es local al proceso: el lease del control plane garantiza que solo hay
// una instancia del agente operando este tunnel a la vez.
func (s *storage) Lock(ctx context.Context, name string) error {
	for {
		s.mu.Lock()
		ch, held := s.locks[name]
		if !held {
			s.locks[name] = make(chan struct{})
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(30 * time.Second):
			return errors.New("timeout esperando lock de " + name)
		}
	}
}

func (s *storage) Unlock(_ context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.locks[name]; ok {
		close(ch)
		delete(s.locks, name)
	}
	return nil
}
