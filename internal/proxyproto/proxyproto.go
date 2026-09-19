// Package proxyproto implementa el header binario de PROXY protocol v2 (solo
// TCP), con el que el nodo le transmite al agente la IP real del visitante.
// Especificación: https://www.haproxy.org/download/2.9/doc/proxy-protocol.txt
package proxyproto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/netip"
)

var signature = []byte("\r\n\r\n\x00\r\nQUIT\n")

const (
	cmdLocal = 0x20
	cmdProxy = 0x21
	famTCP4  = 0x11
	famTCP6  = 0x21
)

// Header codifica src (visitante) y dst (dirección pública del nodo).
func Header(src, dst netip.AddrPort) []byte {
	s, d := src.Addr().Unmap(), dst.Addr().Unmap()
	var b bytes.Buffer
	b.Write(signature)
	b.WriteByte(cmdProxy)
	if s.Is4() && d.Is4() {
		b.WriteByte(famTCP4)
		binary.Write(&b, binary.BigEndian, uint16(12))
		b.Write(s.AsSlice())
		b.Write(d.AsSlice())
	} else {
		b.WriteByte(famTCP6)
		binary.Write(&b, binary.BigEndian, uint16(36))
		s16, d16 := s.As16(), d.As16()
		b.Write(s16[:])
		b.Write(d16[:])
	}
	binary.Write(&b, binary.BigEndian, src.Port())
	binary.Write(&b, binary.BigEndian, dst.Port())
	return b.Bytes()
}

var ErrNoHeader = errors.New("falta el header PROXY v2")

// Read consume un header v2 de r. Para comandos LOCAL devuelve direcciones
// vacías. El header completo leído se devuelve para poder reenviarlo tal cual.
func Read(r io.Reader) (src, dst netip.AddrPort, raw []byte, err error) {
	hdr := make([]byte, 16)
	if _, err = io.ReadFull(r, hdr); err != nil {
		return
	}
	if !bytes.Equal(hdr[:12], signature) {
		err = ErrNoHeader
		return
	}
	n := int(binary.BigEndian.Uint16(hdr[14:16]))
	body := make([]byte, n)
	if _, err = io.ReadFull(r, body); err != nil {
		return
	}
	raw = append(hdr, body...)
	switch {
	case hdr[12] == cmdLocal:
		return
	case hdr[12] != cmdProxy:
		err = fmt.Errorf("proxy v2: comando %#x no soportado", hdr[12])
	case hdr[13] == famTCP4 && n >= 12:
		src = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[0:4])), binary.BigEndian.Uint16(body[8:10]))
		dst = netip.AddrPortFrom(netip.AddrFrom4([4]byte(body[4:8])), binary.BigEndian.Uint16(body[10:12]))
	case hdr[13] == famTCP6 && n >= 36:
		src = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[0:16])), binary.BigEndian.Uint16(body[32:34]))
		dst = netip.AddrPortFrom(netip.AddrFrom16([16]byte(body[16:32])), binary.BigEndian.Uint16(body[34:36]))
	default:
		err = fmt.Errorf("proxy v2: familia %#x no soportada", hdr[13])
	}
	return
}
