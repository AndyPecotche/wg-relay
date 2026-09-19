// Package apiclient es el cliente HTTP del control plane que usan nodos y agentes.
package apiclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/AndyPecotche/wg-relay/internal/proto"
)

type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func New(baseURL, token string) *Client {
	return &Client{
		BaseURL: strings.TrimSuffix(baseURL, "/"),
		Token:   token,
		// Holgado para el long-poll de configuración de los nodos.
		HTTP: &http.Client{Timeout: 45 * time.Second},
	}
}

// Error es una respuesta de error de la API.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("API %d %s: %s", e.Status, e.Code, e.Message) }

// Code devuelve el código de error de la API, o "" si err no vino de la API.
func Code(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// ErrNotModified se devuelve ante un 304 (long-poll sin cambios).
var ErrNotModified = errors.New("sin cambios")

func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&body).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotModified:
		return ErrNotModified
	case resp.StatusCode >= 400:
		var pe proto.Error
		if json.NewDecoder(resp.Body).Decode(&pe) != nil {
			pe = proto.Error{Code: proto.ErrInternal, Message: resp.Status}
		}
		return &Error{Status: resp.StatusCode, Code: pe.Code, Message: pe.Message}
	case out != nil:
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
