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
	"time"
)

const apiBase = "https://api.cloudflare.com/client/v4"

type Client struct {
	token  string
	zoneID string
	http   *http.Client
}

func New(token, zoneID string) *Client {
	return &Client{token: token, zoneID: zoneID, http: &http.Client{Timeout: 20 * time.Second}}
}

type record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
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

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+"/zones/"+c.zoneID+path, r)
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
