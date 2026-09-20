package dnsserver

import (
	"testing"

	"github.com/miekg/dns"
)

func testZone() *Zone {
	return normalize(Zone{
		Name:     "clients.wg-relay.andy.net.ar",
		NSNames:  []string{"ns1.wg-relay.andy.net.ar", "ns2.wg-relay.andy.net.ar"},
		SOAEmail: "admin@andy.net.ar",
		Edge:     []string{"203.0.113.10", "203.0.113.11"},
		Challenges: map[string][]string{
			"_acme-challenge.foo.clients.wg-relay.andy.net.ar.": {"valorA", "valorB"},
		},
	})
}

func query(name string, qtype uint16) *dns.Msg {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	return m
}

func TestAnswerApexSOA(t *testing.T) {
	m := Answer(testZone(), query("clients.wg-relay.andy.net.ar", dns.TypeSOA))
	if len(m.Answer) != 1 {
		t.Fatalf("esperaba 1 SOA, tengo %d", len(m.Answer))
	}
	soa, ok := m.Answer[0].(*dns.SOA)
	if !ok {
		t.Fatalf("no es SOA: %T", m.Answer[0])
	}
	if soa.Mbox != "admin.andy.net.ar." {
		t.Errorf("Mbox = %q", soa.Mbox)
	}
	if soa.Ns != "ns1.wg-relay.andy.net.ar." {
		t.Errorf("Ns (MNAME) = %q", soa.Ns)
	}
}

func TestAnswerApexNS(t *testing.T) {
	m := Answer(testZone(), query("clients.wg-relay.andy.net.ar", dns.TypeNS))
	if len(m.Answer) != 2 {
		t.Fatalf("esperaba 2 NS, tengo %d", len(m.Answer))
	}
}

func TestAnswerNonApexSOANODATA(t *testing.T) {
	m := Answer(testZone(), query("sub.clients.wg-relay.andy.net.ar", dns.TypeSOA))
	if len(m.Answer) != 0 {
		t.Fatalf("SOA fuera del ápice no debería contestar, tengo %d", len(m.Answer))
	}
	assertNODATA(t, m)
}

func TestAnswerCatchAllA(t *testing.T) {
	cases := []string{
		"clients.wg-relay.andy.net.ar",           // el ápice mismo
		"sub1.clients.wg-relay.andy.net.ar",      // subdominio de un tunnel
		"algo.sub1.clients.wg-relay.andy.net.ar", // comodín no enumerado
		"api.clients.wg-relay.andy.net.ar",
	}
	for _, name := range cases {
		m := Answer(testZone(), query(name, dns.TypeA))
		if len(m.Answer) != 2 {
			t.Errorf("%s: esperaba 2 A (edge), tengo %d", name, len(m.Answer))
			continue
		}
		got := map[string]bool{}
		for _, rr := range m.Answer {
			a, ok := rr.(*dns.A)
			if !ok {
				t.Errorf("%s: no es A: %T", name, rr)
				continue
			}
			got[a.A.String()] = true
		}
		if !got["203.0.113.10"] || !got["203.0.113.11"] {
			t.Errorf("%s: IPs = %v", name, got)
		}
	}
}

func TestAnswerAAAANODATA(t *testing.T) {
	m := Answer(testZone(), query("sub1.clients.wg-relay.andy.net.ar", dns.TypeAAAA))
	if len(m.Answer) != 0 {
		t.Fatalf("AAAA no debería contestar nada, tengo %d", len(m.Answer))
	}
	assertNODATA(t, m)
}

func TestAnswerTXTMultipleValues(t *testing.T) {
	m := Answer(testZone(), query("_acme-challenge.foo.clients.wg-relay.andy.net.ar", dns.TypeTXT))
	if len(m.Answer) != 2 {
		t.Fatalf("esperaba 2 TXT, tengo %d", len(m.Answer))
	}
	vals := map[string]bool{}
	for _, rr := range m.Answer {
		txt, ok := rr.(*dns.TXT)
		if !ok || len(txt.Txt) != 1 {
			t.Fatalf("RR inesperado: %#v", rr)
		}
		vals[txt.Txt[0]] = true
	}
	if !vals["valorA"] || !vals["valorB"] {
		t.Errorf("valores = %v", vals)
	}
}

func TestAnswerTXTNoMatchIsNODATA(t *testing.T) {
	m := Answer(testZone(), query("_acme-challenge.otro.clients.wg-relay.andy.net.ar", dns.TypeTXT))
	if len(m.Answer) != 0 {
		t.Fatalf("no debería contestar nada, tengo %d", len(m.Answer))
	}
	assertNODATA(t, m)
}

func TestAnswerPrefixCollisionIsRefused(t *testing.T) {
	m := Answer(testZone(), query("evil-clients.wg-relay.andy.net.ar", dns.TypeA))
	if m.Rcode != dns.RcodeRefused {
		t.Fatalf("Rcode = %v, quería REFUSED", m.Rcode)
	}
}

func TestAnswerOutsideZoneIsRefused(t *testing.T) {
	m := Answer(testZone(), query("otro-dominio.invalid", dns.TypeA))
	if m.Rcode != dns.RcodeRefused {
		t.Fatalf("Rcode = %v, quería REFUSED", m.Rcode)
	}
}

func TestAnswerNoZoneIsAlwaysRefused(t *testing.T) {
	m := Answer(&Zone{}, query("clients.wg-relay.andy.net.ar", dns.TypeA))
	if m.Rcode != dns.RcodeRefused {
		t.Fatalf("Rcode = %v, quería REFUSED", m.Rcode)
	}
	if Answer(nil, query("x", dns.TypeA)).Rcode != dns.RcodeRefused {
		t.Fatal("zona nil debería REFUSED")
	}
}

func TestAnswerEmptyEdgeIsNODATANotCrash(t *testing.T) {
	z := normalize(Zone{Name: "clients.wg-relay.andy.net.ar"})
	m := Answer(z, query("sub1.clients.wg-relay.andy.net.ar", dns.TypeA))
	if len(m.Answer) != 0 {
		t.Fatalf("sin nodos activos no debería contestar nada, tengo %d", len(m.Answer))
	}
	assertNODATA(t, m)
}

func TestAnswerMultiQuestionIsFormErr(t *testing.T) {
	m := new(dns.Msg)
	m.Question = []dns.Question{
		{Name: "a.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
		{Name: "b.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
	}
	got := Answer(testZone(), m)
	if got.Rcode != dns.RcodeFormatError {
		t.Fatalf("Rcode = %v, quería FORMERR", got.Rcode)
	}
}

func assertNODATA(t *testing.T, m *dns.Msg) {
	t.Helper()
	if m.Rcode != dns.RcodeSuccess {
		t.Fatalf("Rcode = %v, NODATA quiere NOERROR", m.Rcode)
	}
	if len(m.Ns) != 1 {
		t.Fatalf("NODATA debería traer la SOA en autoridad, Ns = %v", m.Ns)
	}
	if _, ok := m.Ns[0].(*dns.SOA); !ok {
		t.Fatalf("autoridad no es SOA: %T", m.Ns[0])
	}
}
