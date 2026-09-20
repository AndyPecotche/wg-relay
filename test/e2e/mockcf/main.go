// mockcf imita el subconjunto de la API de Cloudflare que usa
// internal/cloudflare, y sirve un DNS autoritativo de juguete para poder
// probar DNS-01 de punta a punta sin tocar Cloudflare ni el DNS público.
//
// El servidor DNS es deliberadamente "autoritativo para cualquier cosa": ante
// una consulta SOA responde con el propio nombre consultado como dueño de la
// zona. Es degenerado (una zona real no funciona así), pero certmagic solo
// necesita ALGUNA respuesta SOA para completar el recorrido de
// FindZoneByFQDN; a qué nivel exacto se corte da igual, porque el agente
// reconstruye el FQDN completo de todos modos (AbsoluteName("@", zona)).
//
// Solo para test/e2e; no se publica en ninguna imagen del producto.
package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"
)

// ------------------------------------------------- almacén de TXT

type txtStore struct {
	mu     sync.Mutex
	byName map[string][]string // fqdn en minúsculas, con punto final -> valores
}

func newTXTStore() *txtStore { return &txtStore{byName: map[string][]string{}} }

func (s *txtStore) add(name, value string) {
	name = strings.ToLower(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName[name] = append(s.byName[name], value)
}

// removeValue borra un valor puntual: dos desafíos concurrentes con el mismo
// nombre (ej. *.example.com y example.com juntos) no deben pisarse.
func (s *txtStore) removeValue(name, value string) {
	name = strings.ToLower(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	vals := s.byName[name]
	for i, v := range vals {
		if v == value {
			s.byName[name] = append(vals[:i], vals[i+1:]...)
			return
		}
	}
}

func (s *txtStore) get(name string) []string {
	name = strings.ToLower(name)
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.byName[name]...)
}

// ------------------------------------------------- servidor DNS

// upstreamDNS es el resolver embebido de Docker: Pebble usa este servidor
// para TODAS sus resoluciones (se lo pasamos como -dnsserver global, no solo
// para DNS-01), así que lo que no sea SOA/TXT de nuestra zona de prueba hay
// que reenviarlo, o Pebble no podría resolver el nombre real del nodo para
// conectarse durante TLS-ALPN-01.
const upstreamDNS = "127.0.0.11:53"

func serveDNS(store *txtStore, addr string) error {
	client := &dns.Client{Net: "udp", Timeout: 5 * time.Second}
	dns.HandleFunc(".", func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) == 1 {
			switch r.Question[0].Qtype {
			case dns.TypeSOA, dns.TypeTXT:
				w.WriteMsg(answerLocally(store, r))
				return
			}
		}
		resp, _, err := client.Exchange(r, upstreamDNS)
		if err != nil {
			log.Printf("dns: reenviando a %s: %v", upstreamDNS, err)
			m := new(dns.Msg)
			m.SetRcode(r, dns.RcodeServerFailure)
			w.WriteMsg(m)
			return
		}
		w.WriteMsg(resp)
	})
	// Pebble fuerza TCP para su resolver DNS personalizado (va.dnsClient.Net =
	// "tcp" en su código), así que hace falta escuchar en los dos.
	errc := make(chan error, 2)
	go func() { errc <- (&dns.Server{Addr: addr, Net: "udp"}).ListenAndServe() }()
	go func() { errc <- (&dns.Server{Addr: addr, Net: "tcp"}).ListenAndServe() }()
	return <-errc
}

// answerLocally resuelve SOA y TXT con nuestro propio almacén: es la única
// parte de la zona de prueba que no existe en el DNS real.
func answerLocally(store *txtStore, r *dns.Msg) *dns.Msg {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	for _, q := range r.Question {
		switch q.Qtype {
		case dns.TypeSOA:
			// Degenerado a propósito: cada nombre consultado ES su propia
			// zona. certmagic solo necesita ALGUNA respuesta SOA para
			// completar el recorrido de FindZoneByFQDN; a qué nivel se
			// corte da igual, porque el agente reconstruye el FQDN
			// completo de todos modos (AbsoluteName("@", zona)).
			m.Answer = append(m.Answer, &dns.SOA{
				Hdr:    dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 60},
				Ns:     "ns." + q.Name,
				Mbox:   "hostmaster." + q.Name,
				Serial: 1, Refresh: 60, Retry: 60, Expire: 60, Minttl: 60,
			})
		case dns.TypeTXT:
			for _, v := range store.get(q.Name) {
				m.Answer = append(m.Answer, &dns.TXT{
					Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 60},
					Txt: []string{v},
				})
			}
		}
	}
	return m
}

// ------------------------------------------------- mock de la API de Cloudflare

type record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

var (
	store = newTXTStore()

	mu      sync.Mutex
	records = map[string]record{}
	seq     int
)

func main() {
	go func() {
		log.Fatal(serveDNS(store, ":8053"))
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /client/v4/zones/{zone}/dns_records", list)
	mux.HandleFunc("PATCH /client/v4/zones/{zone}/dns_records/{id}", patch)
	mux.HandleFunc("POST /client/v4/zones/{zone}/dns_records", create)
	mux.HandleFunc("DELETE /client/v4/zones/{zone}/dns_records/{id}", del)
	log.Fatal(http.ListenAndServe(":8080", mux))
}

// list responde por tipo+nombre, que es lo único que usa EnsureCNAME antes
// de decidir si crear o actualizar.
func list(w http.ResponseWriter, r *http.Request) {
	typ, name := r.URL.Query().Get("type"), r.URL.Query().Get("name")
	mu.Lock()
	defer mu.Unlock()
	var out []record
	for _, rec := range records {
		if rec.Type == typ && rec.Name == name {
			out = append(out, rec)
		}
	}
	writeEnvelope(w, out)
}

func patch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var in record
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mu.Lock()
	in.ID = id
	records[id] = in
	mu.Unlock()
	writeEnvelope(w, in)
}

func create(w http.ResponseWriter, r *http.Request) {
	var rec record
	if err := json.NewDecoder(r.Body).Decode(&rec); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	mu.Lock()
	seq++
	rec.ID = fmt.Sprintf("rec%d", seq)
	records[rec.ID] = rec
	mu.Unlock()

	if rec.Type == "TXT" {
		store.add(rec.Name+".", rec.Content)
	}
	log.Printf("CREATE %s %s = %q -> %s", rec.Type, rec.Name, rec.Content, rec.ID)
	writeEnvelope(w, rec)
}

func del(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	mu.Lock()
	rec, ok := records[id]
	delete(records, id)
	mu.Unlock()
	if ok && rec.Type == "TXT" {
		store.removeValue(rec.Name+".", rec.Content)
	}
	log.Printf("DELETE %s (%s %s)", id, rec.Type, rec.Name)
	writeEnvelope(w, nil)
}

func writeEnvelope(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result})
}
