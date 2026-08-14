package backend

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The request id is what turns "this is a Daintree bug, please report it" into a report
// someone can act on: the useful detail lives in the SERVER's log, and this is the only
// handle the user has on it. Capture it from the HTTP error response.
func TestErrorCarriesTheRequestIDFromAnHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_http_1")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"type":"api_error","code":"upstream_protocol_error","message":"malformed"}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})

	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("want a *backend.Error, got %v", err)
	}
	if be.RequestID != "req_http_1" {
		t.Errorf("RequestID = %q, want %q", be.RequestID, "req_http_1")
	}
}

// A mid-stream failure is the COMMON case for an upstream problem — the backend emits
// `meta` before it opens the upstream stream, so most of the taxonomy arrives after a
// 200 has already been committed. The id rides that 200's headers, which the SSE parser
// never sees, so without an explicit stamp the failures most in need of a correlation
// id would be the only ones without one.
func TestErrorCarriesTheRequestIDFromAMidStreamFailure(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_stream_1","model":"daintree-assistant","state":"dst1.test"}`,
		``,
		`event: error`,
		`data: {"error":{"type":"api_error","code":"upstream_protocol_error","message":"malformed chunk"}}`,
		``,
	}, "\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req_stream_1")
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})

	var be *Error
	if !errors.As(err, &be) {
		t.Fatalf("want a *backend.Error, got %v", err)
	}
	if !be.Stream {
		t.Error("the failure should be marked as mid-stream")
	}
	if be.HTTPStatus != 0 {
		t.Errorf("HTTPStatus = %d, want 0 — the 200 was already committed", be.HTTPStatus)
	}
	if be.RequestID != "req_stream_1" {
		t.Errorf("RequestID = %q, want %q", be.RequestID, "req_stream_1")
	}
}

// A deterministic upstream verdict delivered mid-stream must NOT be replayed. This is
// the end-to-end version of the classification unit test: with no status to go on, the
// old status-based rule saw nothing to exclude and the retry loop sat through the whole
// backoff budget to re-derive an answer that was final the first time.
func TestDeterministicMidStreamFailureIsNotReplayed(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_1","model":"daintree-assistant","state":"dst1.test"}`,
		``,
		`event: error`,
		`data: {"error":{"type":"api_error","code":"upstream_no_compliant_provider","message":"no endpoint matched"}}`,
		``,
	}, "\n")

	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, body })
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("want an error")
	}
	if n := hits(); n != 1 {
		t.Errorf("the backend was called %d times; a routing dead end is deterministic and must be asked exactly once", n)
	}
}

// The transient half must still be replayed. Splitting the taxonomy moved a provider
// outage from `upstream_error` (retried) to `upstream_unavailable`, and a classification
// that only learned the new deterministic codes would silently stop retrying real blips.
func TestTransientMidStreamFailureIsStillReplayed(t *testing.T) {
	fail := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_1","model":"daintree-assistant","state":"dst1.test"}`,
		``,
		`event: error`,
		`data: {"error":{"type":"api_error","code":"upstream_unavailable","message":"provider down"}}`,
		``,
	}, "\n")
	succeed := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_2","model":"daintree-assistant","state":"dst1.test"}`,
		``,
		`event: delta`,
		`data: {"content":"recovered"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{}}`,
		``,
	}, "\n")

	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, fail
		}
		return http.StatusOK, succeed
	})
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3)})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("a transient outage should have been retried to success: %v", err)
	}
	if n := hits(); n != 2 {
		t.Errorf("the backend was called %d times, want 2 (one failure, one retry)", n)
	}
	if res.Message.Content != "recovered" {
		t.Errorf("content = %q, want %q", res.Message.Content, "recovered")
	}
}
