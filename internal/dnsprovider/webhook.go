package dnsprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Webhook implementa Provider llamando a un servicio HTTP propio del
// operador, en vez de a un proveedor de DNS específico. Es el escape hatch
// para cualquier proveedor que no sea Cloudflare: el operador escribe el
// puente (unas pocas líneas, en el lenguaje que quiera) contra el DNS real
// que use, y wg-relay nunca necesita saber cuál es.
//
// Contrato, todo bajo baseURL:
//
//	POST   /cname      {"name","target"}          -> 2xx
//	POST   /txt        {"name","value"}            -> 2xx {"id":"..."}
//	DELETE /txt/{id}                                -> 2xx (404 también vale)
type Webhook struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewWebhook crea un Provider contra baseURL. token, si no está vacío, viaja
// como "Authorization: Bearer <token>" en cada pedido.
func NewWebhook(baseURL, token string) *Webhook {
	return &Webhook{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		token:   token,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

func (w *Webhook) EnsureCNAME(ctx context.Context, name, target string) error {
	return w.do(ctx, http.MethodPost, "/cname", map[string]string{"name": name, "target": target}, nil)
}

func (w *Webhook) CreateTXT(ctx context.Context, name, value string) (string, error) {
	var out struct {
		ID string `json:"id"`
	}
	err := w.do(ctx, http.MethodPost, "/txt", map[string]string{"name": name, "value": value}, &out)
	return out.ID, err
}

func (w *Webhook) DeleteRecord(ctx context.Context, id string) error {
	err := w.do(ctx, http.MethodDelete, "/txt/"+id, nil, nil)
	if herr, ok := err.(*httpStatusError); ok && herr.status == http.StatusNotFound {
		return nil
	}
	return err
}

type httpStatusError struct {
	status int
	body   string
}

func (e *httpStatusError) Error() string {
	return fmt.Sprintf("dns webhook: HTTP %d: %s", e.status, e.body)
}

func (w *Webhook) do(ctx context.Context, method, path string, body, out any) error {
	var r *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	} else {
		r = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, w.baseURL+path, r)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if w.token != "" {
		req.Header.Set("Authorization", "Bearer "+w.token)
	}
	resp, err := w.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var msg bytes.Buffer
		msg.ReadFrom(resp.Body)
		return &httpStatusError{status: resp.StatusCode, body: strings.TrimSpace(msg.String())}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
