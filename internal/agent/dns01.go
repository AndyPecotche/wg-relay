package agent

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/libdns/libdns"

	"github.com/AndyPecotche/wg-relay/internal/apiclient"
	"github.com/AndyPecotche/wg-relay/internal/proto"
)

// dns01Provider implementa libdns.RecordAppender/RecordDeleter contra el
// endpoint de DNS-01 delegado del control plane (DESIGN.md §6.2.1). Habilita
// certificados wildcard sobre el dominio asignado: certmagic recurre a esto
// automáticamente cuando el certificado pedido es "*.<dominio>", que es el
// único caso en el que Let's Encrypt exige DNS-01.
//
// El agente nunca ve el token de Cloudflare: el control plane es quien
// escribe el TXT, y solo dentro de la zona del tunnel autenticado.
type dns01Provider struct {
	api *apiclient.Client

	mu  sync.Mutex
	ids map[string]string // fqdn+"\x00"+valor -> id opaco del desafío en el control plane
}

func newDNS01Provider(api *apiclient.Client) *dns01Provider {
	return &dns01Provider{api: api, ids: map[string]string{}}
}

func challengeKey(fqdn, value string) string { return fqdn + "\x00" + value }

// AppendRecords resuelve dnsName/zone (que certmagic determina por una
// consulta DNS real, no por nuestra API) a un FQDN absoluto y le pide al
// control plane que escriba el TXT correspondiente.
func (p *dns01Provider) AppendRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(recs))
	for _, rec := range recs {
		rr := rec.RR()
		fqdn := strings.TrimSuffix(libdns.AbsoluteName(rr.Name, zone), ".")
		var resp proto.ACMEDNSCreateResponse
		err := p.api.Do(ctx, http.MethodPost, "/v1/agent/acme-dns",
			proto.ACMEDNSCreateRequest{FQDN: fqdn, Value: rr.Data}, &resp)
		if err != nil {
			return out, fmt.Errorf("DNS-01: creando TXT para %s: %w", fqdn, err)
		}
		p.mu.Lock()
		p.ids[challengeKey(fqdn, rr.Data)] = resp.ID
		p.mu.Unlock()
		out = append(out, rr)
	}
	return out, nil
}

// DeleteRecords borra los TXT creados por AppendRecords. certmagic no nos
// devuelve ningún identificador propio junto al registro (libdns.RR no tiene
// campo para eso), así que la correspondencia fqdn+valor -> id del desafío la
// llevamos nosotros, en memoria, con el mismo criterio que certmagic usa
// internamente para distinguir desafíos concurrentes con el mismo nombre.
func (p *dns01Provider) DeleteRecords(ctx context.Context, zone string, recs []libdns.Record) ([]libdns.Record, error) {
	out := make([]libdns.Record, 0, len(recs))
	for _, rec := range recs {
		rr := rec.RR()
		fqdn := strings.TrimSuffix(libdns.AbsoluteName(rr.Name, zone), ".")
		key := challengeKey(fqdn, rr.Data)
		p.mu.Lock()
		id, ok := p.ids[key]
		delete(p.ids, key)
		p.mu.Unlock()
		if !ok {
			continue // no lo creamos nosotros (o ya se borró): nada que hacer
		}
		if err := p.api.Do(ctx, http.MethodDelete, "/v1/agent/acme-dns/"+id, nil, nil); err != nil {
			return out, fmt.Errorf("DNS-01: borrando TXT de %s: %w", fqdn, err)
		}
		out = append(out, rr)
	}
	return out, nil
}
