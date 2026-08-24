package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// A backend on loopback that answers with a 307 to a REMOTE host must not move the
// request. Go's default policy replays a 307 POST body at the new location, and that
// body is the whole conversation — prose, file paths, tool arguments. Daintree pins this
// engine to loopback precisely because its panel is unauthenticated and carries all of
// that in the request, so a followed redirect walks straight through the pin.
//
// The remote leg is a real (loopback) server here rather than a made-up hostname, so the
// test fails loudly if the refusal ever stops working: the counter it increments is the
// evidence the body arrived somewhere it should never have reached.
func TestRespondStream_RefusesRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: meta\ndata: {}\n\n"))
	}))
	t.Cleanup(remote.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	c := NewClient(ClientConfig{BaseURL: origin.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})

	if err == nil {
		t.Fatal("a redirected respond must fail, not follow")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the conversation was replayed at the redirect target %d time(s)", n)
	}
	// The message has to name what happened; "could not reach the backend" would send
	// someone debugging a network they can reach perfectly well.
	if !strings.Contains(err.Error(), "does not redirect") {
		t.Fatalf("error should explain the refusal, got: %v", err)
	}
}

// The JSON client shares the policy — utility tasks carry transcripts too.
func TestDoJSON_RefusesRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(remote.Close)

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL+r.URL.Path, http.StatusPermanentRedirect)
	}))
	t.Cleanup(origin.Close)

	c := NewClient(ClientConfig{BaseURL: origin.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	err := c.doJSON(context.Background(), http.MethodPost, "/v1/daintree/tasks", map[string]any{"task": "x"}, &struct{}{})

	if err == nil {
		t.Fatal("a redirected JSON call must fail, not follow")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("the payload was replayed at the redirect target %d time(s)", n)
	}
}

// A loopback→loopback redirect is refused too. Allowing it would mean the safety of the
// hop depends on re-reading the loopback predicate, and a redirect to a DIFFERENT
// loopback port is still a different server from the one the session was pinned to.
func TestRefusesEvenLoopbackRedirect(t *testing.T) {
	var elsewhere atomic.Int64
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(remote.Close)
	if !IsLoopbackURL(remote.URL) {
		t.Fatalf("precondition: httptest should serve on loopback, got %s", remote.URL)
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, remote.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(origin.Close)

	c := NewClient(ClientConfig{BaseURL: origin.URL, Retry: RetryPolicy{MaxAttempts: 1}})
	if err := c.doJSON(context.Background(), http.MethodPost, "/v1/daintree/tasks", map[string]any{}, &struct{}{}); err == nil {
		t.Fatal("a loopback-to-loopback redirect must be refused too")
	}
	if n := elsewhere.Load(); n != 0 {
		t.Fatalf("followed a loopback redirect %d time(s)", n)
	}
}

// A refused redirect is FINAL. Classified as a plain "connect" it would be replayed for
// the whole retry budget — ~9 attempts over more than a minute — to re-derive the
// identical refusal.
func TestRedirectRefusalIsNotRetried(t *testing.T) {
	refused := transportError(noRedirects(&http.Request{URL: mustParseURL(t, "https://evil.test/x")}, nil))
	if refused.Code != "redirect_refused" {
		t.Fatalf("code = %q, want redirect_refused", refused.Code)
	}
	if isRetriable(refused) {
		t.Fatal("a refused redirect must be final")
	}
	// An ordinary transport failure keeps its retriable classification.
	if got := transportError(errors.New("connection refused")); got.Code != "connect" || !isRetriable(got) {
		t.Fatalf("an ordinary dial failure must stay retriable: %+v", got)
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}
