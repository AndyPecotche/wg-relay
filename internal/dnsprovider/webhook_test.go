package dnsprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWebhookEnsureCNAME(t *testing.T) {
	var gotAuth, gotMethod, gotPath string
	var gotBody map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotMethod, gotPath = r.Header.Get("Authorization"), r.Method, r.URL.Path
		json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "s3cr3t")
	if err := wh.EnsureCNAME(context.Background(), "sub.example.com", "edge.example.com"); err != nil {
		t.Fatalf("EnsureCNAME: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/cname" {
		t.Fatalf("got %s %s, want POST /cname", gotMethod, gotPath)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody["name"] != "sub.example.com" || gotBody["target"] != "edge.example.com" {
		t.Fatalf("body = %+v", gotBody)
	}
}

func TestWebhookCreateAndDeleteTXT(t *testing.T) {
	deleted := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/txt":
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]string{"id": "rec-123"})
		case r.Method == http.MethodDelete:
			deleted = r.URL.Path
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "")
	id, err := wh.CreateTXT(context.Background(), "_acme-challenge.example.com", "value")
	if err != nil {
		t.Fatalf("CreateTXT: %v", err)
	}
	if id != "rec-123" {
		t.Fatalf("id = %q, want rec-123", id)
	}
	if err := wh.DeleteRecord(context.Background(), id); err != nil {
		t.Fatalf("DeleteRecord: %v", err)
	}
	if deleted != "/txt/rec-123" {
		t.Fatalf("deleted path = %q", deleted)
	}
}

func TestWebhookDeleteRecordNotFoundIsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "")
	if err := wh.DeleteRecord(context.Background(), "gone"); err != nil {
		t.Fatalf("DeleteRecord con 404 debería ser éxito: %v", err)
	}
}

func TestWebhookErrorStatusFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	wh := NewWebhook(srv.URL, "")
	if err := wh.EnsureCNAME(context.Background(), "a", "b"); err == nil {
		t.Fatal("esperaba error con HTTP 500")
	}
}
