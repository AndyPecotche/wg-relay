// Package pipe copia bytes en ambos sentidos entre dos conexiones.
package pipe

import (
	"io"
	"net"
	"sync"
)

type closeWriter interface{ CloseWrite() error }

// Join copia a<->b hasta que ambos lados terminan y cierra las dos conexiones.
// Propaga el half-close (FIN) cuando la conexión lo soporta, para protocolos
// que cierran la escritura y siguen leyendo la respuesta.
func Join(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); copyAndClose(a, b) }()
	go func() { defer wg.Done(); copyAndClose(b, a) }()
	wg.Wait()
	a.Close()
	b.Close()
}

func copyAndClose(dst, src net.Conn) {
	if _, err := io.Copy(dst, src); err != nil {
		// Error de un lado: cortamos todo para no dejar la otra mitad colgada.
		dst.Close()
		src.Close()
		return
	}
	if cw, ok := unwrap(dst).(closeWriter); ok {
		cw.CloseWrite()
	} else {
		dst.Close()
	}
}

func unwrap(c net.Conn) net.Conn {
	for {
		u, ok := c.(interface{ Unwrap() net.Conn })
		if !ok {
			return c
		}
		c = u.Unwrap()
	}
}
