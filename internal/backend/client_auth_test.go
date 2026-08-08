package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The bearer header contract: a configured key rides EVERY request as
// "Authorization: Bearer <key>"; no key sends no header at all (the local dev
// backend runs unauthenticated and must not receive an empty Bearer).
func TestClientSendsBearerOnlyWhenConfigured(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	}))
	defer srv.Close()

	withKey := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: " sk-test-123 "})
	if err := withKey.Health(context.Background()); err != nil {
		t.Fatalf("Health with key: %v", err)
	}
	without := NewClient(ClientConfig{BaseURL: srv.URL})
	if err := without.Health(context.Background()); err != nil {
		t.Fatalf("Health without key: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("server saw %d requests, want 2", len(got))
	}
	if got[0] != "Bearer sk-test-123" {
		t.Fatalf("with key: Authorization = %q, want trimmed bearer", got[0])
	}
	if got[1] != "" {
		t.Fatalf("without key: Authorization = %q, want absent", got[1])
	}
}
