// Package sni extrae el Server Name Indication de un ClientHello TLS sin
// terminar la conexión, y devuelve una conexión que reproduce los bytes leídos.
package sni

import (
	"bytes"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

var (
	errDone  = errors.New("clienthello leído")
	ErrNoSNI = errors.New("el ClientHello no trae SNI")
)

// Peek lee el ClientHello de conn y devuelve el hostname (en minúsculas) y una
// conexión que entrega primero los bytes ya consumidos.
func Peek(conn net.Conn, timeout time.Duration) (string, net.Conn, error) {
	var buf bytes.Buffer
	var name string
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return "", nil, err
	}
	// crypto/tls hace el parseo real; el callback aborta el handshake apenas
	// tiene el hello, antes de escribir nada en la conexión.
	err := tls.Server(readOnlyConn{io.TeeReader(conn, &buf)}, &tls.Config{
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			name = h.ServerName
			return nil, errDone
		},
	}).Handshake()
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return "", nil, err
	}
	if !errors.Is(err, errDone) {
		return "", nil, err
	}
	if name == "" {
		return "", nil, ErrNoSNI
	}
	return strings.ToLower(name), &PrefixConn{Conn: conn, prefix: buf.Bytes()}, nil
}

// PrefixConn entrega prefix antes de seguir leyendo de Conn.
type PrefixConn struct {
	net.Conn
	prefix []byte
}

func NewPrefixConn(c net.Conn, prefix []byte) *PrefixConn {
	return &PrefixConn{Conn: c, prefix: prefix}
}

func (c *PrefixConn) Read(p []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(p, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(p)
}

// Unwrap expone la conexión original (lo usa pipe para el half-close).
func (c *PrefixConn) Unwrap() net.Conn { return c.Conn }

type readOnlyConn struct{ r io.Reader }

func (c readOnlyConn) Read(p []byte) (int, error)     { return c.r.Read(p) }
func (readOnlyConn) Write([]byte) (int, error)        { return 0, io.ErrClosedPipe }
func (readOnlyConn) Close() error                     { return nil }
func (readOnlyConn) LocalAddr() net.Addr              { return nil }
func (readOnlyConn) RemoteAddr() net.Addr             { return nil }
func (readOnlyConn) SetDeadline(time.Time) error      { return nil }
func (readOnlyConn) SetReadDeadline(time.Time) error  { return nil }
func (readOnlyConn) SetWriteDeadline(time.Time) error { return nil }

// Match busca host en table: primero exacto, después comodines subiendo de a
// una etiqueta ("a.b.c" prueba "*.b.c" y luego "*.c").
func Match[V any](table map[string]V, host string) (V, bool) {
	if v, ok := table[host]; ok {
		return v, true
	}
	for rest := host; ; {
		i := strings.IndexByte(rest, '.')
		if i < 0 {
			var zero V
			return zero, false
		}
		rest = rest[i+1:]
		if v, ok := table["*."+rest]; ok {
			return v, true
		}
	}
}
