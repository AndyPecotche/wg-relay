package dnsserver

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"
)

// Server sirve el contenido de la zona actual (atómica, reemplazable en
// caliente vía SetZone) por UDP y TCP.
type Server struct {
	zone atomic.Pointer[Zone]
}

func New() *Server {
	s := &Server{}
	s.zone.Store(&Zone{})
	return s
}

// SetZone normaliza y reemplaza la zona servida.
func (s *Server) SetZone(z Zone) {
	s.zone.Store(normalize(z))
}

func normalize(z Zone) *Zone {
	out := &Zone{
		SOAEmail:   z.SOAEmail,
		Edge:       append([]string(nil), z.Edge...),
		Challenges: make(map[string][]string, len(z.Challenges)),
	}
	if z.Name != "" {
		out.Name = dns.Fqdn(strings.ToLower(z.Name))
	}
	for _, ns := range z.NSNames {
		out.NSNames = append(out.NSNames, dns.Fqdn(strings.ToLower(ns)))
	}
	for fqdn, vals := range z.Challenges {
		out.Challenges[dns.Fqdn(strings.ToLower(fqdn))] = append([]string(nil), vals...)
	}
	return out
}

// Serve arranca listeners UDP y TCP en addr (algunos clientes DNS fuerzan
// TCP, lo vimos depurando Pebble/ACME) y bloquea hasta que ctx se cancela o
// falla el bind de alguno de los dos; en cualquier caso cierra ambos.
func (s *Server) Serve(ctx context.Context, addr string) error {
	handler := dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		w.WriteMsg(Answer(s.zone.Load(), r))
	})
	udp := &dns.Server{Addr: addr, Net: "udp", Handler: handler}
	tcp := &dns.Server{Addr: addr, Net: "tcp", Handler: handler}

	errc := make(chan error, 2)
	go func() { errc <- udp.ListenAndServe() }()
	go func() { errc <- tcp.ListenAndServe() }()

	var err error
	select {
	case err = <-errc:
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	udp.ShutdownContext(shutdown)
	tcp.ShutdownContext(shutdown)
	return err
}
