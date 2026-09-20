// Package cloudflare es un cliente mínimo de la API de DNS de Cloudflare:
// solo lo que el control plane necesita, sin dependencias externas.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AndyPecotche/wg-relay/internal/dnsprovider"
)

const defaultAPIBase = "https://api.cloudflare.com/client/v4"

var _ dnsprovider.Provider = (*Client)(nil)

type Client struct {
	token   string
	zoneID  string
	apiBase string
	http    *http.Client
}

// New crea un cliente contra la API real de Cloudflare.
func New(token, zoneID string) *Client {
	return NewWithBaseURL(token, zoneID, defaultAPIBase)
}

// NewWithBaseURL permite apuntar a otra URL base (para tests: un servidor
// que imite el subconjunto de la API que este cliente usa).
func NewWithBaseURL(token, zoneID, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultAPIBase
	}
	return &Client{token: token, zoneID: zoneID, apiBase: strings.TrimSuffix(baseURL, "/"),
		http: &http.Client{Timeout: 20 * time.Second}}
}

type record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

// txtRecord omite "proxied": Cloudflare no lo acepta en registros que no son
// A/AAAA/CNAME.
type txtRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
}

// EnsureCNAME crea o corrige name -> target. Siempre sin proxy de Cloudflare
// (nube gris): el proxy terminaría TLS en Cloudflare y rompería el passthrough.
func (c *Client) EnsureCNAME(ctx context.Context, name, target string) error {
	var existing []record
	q := url.Values{"type": {"CNAME"}, "name": {name}}
	if err := c.do(ctx, http.MethodGet, "/dns_records?"+q.Encode(), nil, &existing); err != nil {
		return err
	}
	want := record{Type: "CNAME", Name: name, Content: target, TTL: 1, Proxied: false}
	if len(existing) == 0 {
		return c.do(ctx, http.MethodPost, "/dns_records", want, nil)
	}
	if existing[0].Content == target && !existing[0].Proxied {
		return nil
	}
	return c.do(ctx, http.MethodPatch, "/dns_records/"+existing[0].ID, want, nil)
}

// CreateTXT agrega un registro TXT nuevo, sin tocar los que ya existan bajo el
// mismo nombre. Es intencional: un desafío ACME de comodín (ej. *.example.com
// y example.com juntos) puede necesitar dos TXT distintos y simultáneos bajo
// el mismo _acme-challenge, diferenciados solo por su valor. Devuelve el ID
// del registro creado, para poder borrarlo puntualmente después.
func (c *Client) CreateTXT(ctx context.Context, name, value string) (id string, err error) {
	var rec txtRecord
	err = c.do(ctx, http.MethodPost, "/dns_records", txtRecord{Type: "TXT", Name: name, Content: value, TTL: 60}, &rec)
	return rec.ID, err
}

// DeleteRecord borra un registro por ID. Si ya no existe, Cloudflare devuelve
// un error que tratamos como éxito: el estado final deseado ya está logrado.
func (c *Client) DeleteRecord(ctx context.Context, id string) error {
	err := c.do(ctx, http.MethodDelete, "/dns_records/"+id, nil, nil)
	if err != nil && strings.Contains(err.Error(), "81044") { // record does not exist
		return nil
	}
	return err
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase+"/zones/"+c.zoneID+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var env struct {
		Success bool                       `json:"success"`
		Errors  []struct{ Message string } `json:"errors"`
		Result  json.RawMessage            `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("cloudflare %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if !env.Success {
		msg := resp.Status
		if len(env.Errors) > 0 {
			msg = env.Errors[0].Message
		}
		return fmt.Errorf("cloudflare %s: %s", method, msg)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}
