// dnsquery es un cliente DNS mínimo para el e2e: consulta el :5300 del nodo
// directamente (su DNS autoritativo propio, internal/dnsserver) e imprime la
// respuesta, para no depender de dig/kdig (no están en las imágenes que usa
// el harness). Solo para test/e2e; no se publica en ninguna imagen del
// producto.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/miekg/dns"
)

func main() {
	server := flag.String("server", "", "host:puerto del DNS a consultar")
	net_ := flag.String("net", "udp", "udp | tcp")
	typ := flag.String("type", "A", "A | TXT | SOA | NS")
	name := flag.String("name", "", "nombre a consultar")
	expectRefused := flag.Bool("expect-refused", false, "éxito solo si la respuesta es REFUSED")
	flag.Parse()
	if *server == "" || *name == "" {
		fmt.Fprintln(os.Stderr, "uso: dnsquery -server host:puerto -name X [-type A|TXT|SOA|NS] [-net udp|tcp] [-expect-refused]")
		os.Exit(2)
	}
	qtype, ok := map[string]uint16{"A": dns.TypeA, "TXT": dns.TypeTXT, "SOA": dns.TypeSOA, "NS": dns.TypeNS}[*typ]
	if !ok {
		fmt.Fprintln(os.Stderr, "tipo desconocido:", *typ)
		os.Exit(2)
	}

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(*name), qtype)
	client := &dns.Client{Net: *net_, Timeout: 5 * time.Second}
	resp, _, err := client.Exchange(m, *server)
	if err != nil {
		fmt.Fprintln(os.Stderr, "consulta:", err)
		os.Exit(1)
	}

	if *expectRefused {
		if resp.Rcode != dns.RcodeRefused {
			fmt.Fprintf(os.Stderr, "rcode = %s, esperaba REFUSED\n", dns.RcodeToString[resp.Rcode])
			os.Exit(1)
		}
		fmt.Println("REFUSED")
		return
	}
	if resp.Rcode != dns.RcodeSuccess {
		fmt.Fprintf(os.Stderr, "rcode = %s\n", dns.RcodeToString[resp.Rcode])
		os.Exit(1)
	}
	for _, rr := range resp.Answer {
		switch v := rr.(type) {
		case *dns.A:
			fmt.Println(v.A.String())
		case *dns.TXT:
			for _, s := range v.Txt {
				fmt.Println(s)
			}
		case *dns.NS:
			fmt.Println(v.Ns)
		case *dns.SOA:
			fmt.Println(v.Ns, v.Mbox)
		default:
			fmt.Println(rr.String())
		}
	}
}
