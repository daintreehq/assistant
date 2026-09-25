package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamRidesEveryBackendCall(t *testing.T) {
	const key = "sk-caller-own-0123456789"
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KeyVerification{Valid: false, Detail: "rejected " + key})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{
		BaseURL:  srv.URL,
		Upstream: Upstream{Provider: "openai", Model: "gpt-6-luna", APIKey: key},
		Retry:    RetryPolicy{MaxAttempts: 1},
	})
	v, err := c.VerifyKey(context.Background())
	if err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if got.Get(upstreamProviderHeader) != "openai" || got.Get(upstreamModelHeader) != "gpt-6-luna" ||
		got.Get(upstreamKeyHeader) != key {
		t.Fatalf("upstream headers not sent: %v", got)
	}
	if got.Get("Authorization") != "" {
		t.Errorf("the upstream key must never ride as the bearer: %q", got.Get("Authorization"))
	}
	if strings.Contains(v.Detail, key) {
		t.Errorf("an echoed upstream key was not scrubbed: %q", v.Detail)
	}
}

func TestNoUpstreamSendsNoUpstreamHeaders(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KeyVerification{Valid: true})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	for _, h := range []string{upstreamProviderHeader, upstreamModelHeader, upstreamKeyHeader} {
		if got.Get(h) != "" {
			t.Errorf("%s sent without an upstream: %q", h, got.Get(h))
		}
	}
}

func TestUpstreamValidate(t *testing.T) {
	cases := []struct {
		name string
		u    Upstream
		ok   bool
	}{
		{"unset", Upstream{}, true},
		{"complete", Upstream{Provider: "baseten", APIKey: "abc123"}, true},
		{"unknown host", Upstream{Provider: "anthropic", APIKey: "abc123"}, false},
		{"no key", Upstream{Provider: "openrouter"}, false},
		{"key without host", Upstream{APIKey: "abc123"}, false},
		{"spaced model", Upstream{Provider: "openai", Model: "gpt 6", APIKey: "abc123"}, false},
		{"openrouter routing", Upstream{Provider: "openrouter", APIKey: "abc123", Sort: "price", DataCollection: "deny", ZDR: "true"}, true},
		{"routing off openrouter", Upstream{Provider: "openai", APIKey: "abc123", Sort: "price"}, false},
		{"unknown sort", Upstream{Provider: "openrouter", APIKey: "abc123", Sort: "cheapest"}, false},
		{"bad zdr", Upstream{Provider: "openrouter", APIKey: "abc123", ZDR: "yes"}, false},
		{"routing without a provider", Upstream{ZDR: "true"}, false},
	}
	for _, tc := range cases {
		err := tc.u.Validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v, want ok=%v", tc.name, err, tc.ok)
		}
		if err != nil && tc.u.APIKey != "" && strings.Contains(err.Error(), tc.u.APIKey) {
			t.Errorf("%s: error quoted the key: %v", tc.name, err)
		}
	}
}

func TestCallerUpstreamKeyErrorsAreRecognisedByParam(t *testing.T) {
	caller := &Error{Code: CodeProviderInvalidAPIKey, Param: upstreamKeyHeader, Message: "OpenAI rejected your API key."}
	server := &Error{Code: CodeProviderInvalidAPIKey}
	if !caller.IsCallerUpstreamKey() {
		t.Error("a caller-key rejection was not recognised")
	}
	if server.IsCallerUpstreamKey() {
		t.Error("a deployment-key rejection was taken for the caller's")
	}
}

func TestUpstreamRidesTheStreamAndTaskPaths(t *testing.T) {
	const key = "sk-caller-own-0123456789"
	seen := map[string]http.Header{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.URL.Path] = r.Header.Clone()
		switch r.URL.Path {
		case "/v1/daintree/respond":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("event: done\ndata: {\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":\"stop\"}\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"t","task":"terminal_summarize","output":{"text":"x"}}`))
		}
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{
		BaseURL:  srv.URL,
		Upstream: Upstream{Provider: "baseten", APIKey: key},
		Retry:    RetryPolicy{MaxAttempts: 1},
	})
	_, _ = c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	_, _ = c.RunTask(context.Background(), TaskRequest{Task: "terminal_summarize"})
	for _, path := range []string{"/v1/daintree/respond", "/v1/daintree/tasks"} {
		h, ok := seen[path]
		if !ok {
			t.Fatalf("%s was never called", path)
		}
		if h.Get(upstreamKeyHeader) != key || h.Get(upstreamProviderHeader) != "baseten" {
			t.Errorf("%s did not carry the upstream: %v", path, h)
		}
	}
}

func TestAReadinessEchoOfTheUpstreamKeyIsScrubbed(t *testing.T) {
	const key = "sk-caller-own-0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"starting","error":"bad header ` + key + `"}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Upstream: Upstream{Provider: "openai", APIKey: key}, Retry: RetryPolicy{MaxAttempts: 1}})
	err := c.Ready(context.Background())
	if err == nil || strings.Contains(err.Error(), key) {
		t.Fatalf("readiness error leaked or missing: %v", err)
	}
}

func TestCallerModelFailuresAreNotRetried(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"OpenAI does not serve the model 'x'.","type":"api_error","code":"upstream_unavailable","param":"X-Daintree-Upstream-Model"}}`))
	}))
	defer srv.Close()
	c := NewClient(ClientConfig{BaseURL: srv.URL, Upstream: Upstream{Provider: "openai", APIKey: "sk-x"}, Retry: RetryPolicy{MaxAttempts: 4}})
	_, err := c.RunTask(context.Background(), TaskRequest{Task: "terminal_summarize"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != 1 {
		t.Errorf("a caller-model failure was retried: %d calls", calls)
	}
}

func TestOpenRouterRoutingRidesItsOwnHeaders(t *testing.T) {
	h := http.Header{}
	Upstream{Provider: "openrouter", APIKey: "k", Sort: "price", DataCollection: "deny", ZDR: "true"}.apply(h)
	if h.Get(upstreamSortHeader) != "price" || h.Get(upstreamDataHeader) != "deny" || h.Get(upstreamZDRHeader) != "true" {
		t.Fatalf("routing headers not sent: %v", h)
	}
	h = http.Header{}
	Upstream{Provider: "openrouter", APIKey: "k"}.apply(h)
	if h.Get(upstreamSortHeader) != "" || h.Get(upstreamZDRHeader) != "" {
		t.Fatalf("an unset preference was sent: %v", h)
	}
}
