// Package dnsserver implementa el servidor DNS autoritativo propio que cada
// nodo puede correr para la zona clients.* (WGRELAY_BASE_DOMAIN), como
// alternativa a depender de un proveedor externo (internal/dnsprovider). A
// diferencia del ruteo SNI del nodo (que sí necesita precisión por tunnel),
// acá no hace falta: todo subdominio de cliente resuelve al mismo conjunto
// de IPs públicas de nodos activos, la decisión de a qué agente va cada
// conexión ya la toma el nodo por SNI. Ver DESIGN.md.
package dnsserver

import (
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// defaultTTL es corto a propósito: el set de nodos activos puede cambiar.
const defaultTTL = 30

// Zone es la foto de datos que Answer necesita para contestar. Se construye
// y normaliza vía Server.SetZone; Answer no vuelve a normalizar nada.
type Zone struct {
	Name       string              // ápice, ej. "clients.wg-relay.andy.net.ar."
	NSNames    []string            // FQDNs de nameserver, estático, admin
	SOAEmail   string              // "usuario@dominio", o vacío para un default
	Edge       []string            // IPs públicas (IPv4) de nodos activos
	Challenges map[string][]string // FQDN (con punto final) -> valores TXT vigentes
}

// Answer construye la respuesta autoritativa para r según z. Sin red: se
// testea directo, sin sockets.
//
//   - Fuera de la zona (o zona no configurada): REFUSED, nunca forwarding —
//     un nameserver autoritativo real no es un resolver abierto.
//   - SOA/NS del ápice: estático, de z.NSNames/z.SOAEmail.
//   - TXT que coincide con un desafío vigente: sus valores (soporta varios
//     valores concurrentes bajo el mismo nombre).
//   - Cualquier otro nombre bajo la zona, para A: el conjunto z.Edge.
//   - Cualquier otro caso (SOA/NS fuera del ápice, TXT sin match, tipos no
//     soportados, o A con Edge vacío): NODATA (NOERROR + SOA en autoridad),
//     nunca NXDOMAIN — el nombre conceptualmente existe, solo no hay ese
//     dato ahora mismo.
func Answer(z *Zone, r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	if len(r.Question) != 1 {
		m.SetRcode(r, dns.RcodeFormatError)
		return m
	}
	q := r.Question[0]
	qname := strings.ToLower(q.Name)
	if z == nil || z.Name == "" || !dns.IsSubDomain(z.Name, qname) {
		m.SetRcode(r, dns.RcodeRefused)
		return m
	}
	m.Authoritative = true

	switch q.Qtype {
	case dns.TypeSOA:
		if qname == z.Name {
			m.Answer = append(m.Answer, soaRecord(z))
		}
	case dns.TypeNS:
		if qname == z.Name {
			for _, ns := range z.NSNames {
				m.Answer = append(m.Answer, &dns.NS{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: defaultTTL},
					Ns:  ns,
				})
			}
		}
	case dns.TypeTXT:
		for _, v := range z.Challenges[qname] {
			m.Answer = append(m.Answer, &dns.TXT{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: defaultTTL},
				Txt: []string{v},
			})
		}
	case dns.TypeA:
		for _, ip := range z.Edge {
			addr, err := netip.ParseAddr(ip)
			if err != nil || !addr.Is4() {
				continue
			}
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: defaultTTL},
				A:   addr.AsSlice(),
			})
		}
	}
	if len(m.Answer) == 0 {
		m.Ns = append(m.Ns, soaRecord(z))
	}
	return m
}

// soaRecord no depende de una relación primaria/secundaria real (no hay
// AXFR/IXFR: cada nodo computa la misma respuesta de forma independiente
// desde el mismo snapshot empujado), así que el serial no viaja por la red:
// se calcula localmente, siempre creciente, sin coordinación entre nodos.
func soaRecord(z *Zone) *dns.SOA {
	ns := z.Name
	if len(z.NSNames) > 0 {
		ns = z.NSNames[0]
	}
	return &dns.SOA{
		Hdr:     dns.RR_Header{Name: z.Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: defaultTTL},
		Ns:      ns,
		Mbox:    rname(z.SOAEmail, z.Name),
		Serial:  uint32(time.Now().Unix()),
		Refresh: 3600,
		Retry:   600,
		Expire:  604800,
		Minttl:  defaultTTL,
	}
}

// rname convierte "usuario@dominio" a la forma RNAME de RFC 1035 §8: el '@'
// se reemplaza por '.', y cualquier '.' literal de la parte local se escapa
// como '\.'. Vacío o sin '@' (ya viene en forma RNAME) se toma tal cual, con
// un default razonable si está vacío.
func rname(email, zone string) string {
	if email == "" {
		return dns.Fqdn("hostmaster." + strings.TrimSuffix(zone, "."))
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok {
		return dns.Fqdn(email)
	}
	return dns.Fqdn(strings.ReplaceAll(local, ".", `\.`) + "." + domain)
}
