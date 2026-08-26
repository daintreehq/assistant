package backend

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// The point of the type: a client that cannot produce a credential must fail rather than
// send the request bare. NoTokenSource sends it bare, which against an open door SUCCEEDS
// — this machine's local fault silently billed to whoever the open door resolves to, with
// nothing anywhere reporting a problem.
func TestUnavailableTokenSourceSendsNothing(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	fault := errors.New("the account layer could not be built")
	c := NewClient(ClientConfig{
		BaseURL:     srv.URL,
		TokenSource: UnavailableTokenSource{Err: fault},
		Retry:       RetryPolicy{MaxAttempts: 1},
	})

	_, err := c.Account(context.Background())
	if err == nil {
		t.Fatal("Account succeeded with an unavailable account layer")
	}
	if reached {
		t.Error("the request was sent anyway, without an Authorization header")
	}
	// Nothing was sent, so this is not the backend asking for a sign-in. Conflating the
	// two sends someone with a broken state root through a browser flow that fails at
	// the same write.
	if !strings.Contains(err.Error(), CodeCredentialUnavailable) {
		t.Errorf("error = %v, want it to carry %s", err, CodeCredentialUnavailable)
	}
	if strings.Contains(err.Error(), CodeAuthRequired) {
		t.Errorf("error = %v must not claim the backend demanded a sign-in", err)
	}
	// The local diagnosis survives the wrap, so every surface can report the same one
	// rather than each inventing its own wording for the same broken directory.
	if !errors.Is(err, fault) {
		t.Errorf("error = %v does not unwrap to the account-layer fault", err)
	}
}

// The zero value must still fail closed. A caller that forgets to supply a cause has a
// less useful message; it must not get an anonymous request.
func TestUnavailableTokenSourceZeroValueStillFails(t *testing.T) {
	tok, err := UnavailableTokenSource{}.AccessToken(context.Background())
	if err == nil {
		t.Fatal("the zero value produced a credential")
	}
	if tok != "" {
		t.Fatalf("token = %q, want empty", tok)
	}
}

// countingUnavailable is UnavailableTokenSource with a call counter, so a public-path
// test can prove the source was never CONSULTED rather than merely that the request
// succeeded — the weaker claim passes even if the source is asked and its failure
// swallowed.
type countingUnavailable struct {
	calls *int32
	inner UnavailableTokenSource
}

func (c countingUnavailable) AccessToken(ctx context.Context) (string, error) {
	atomic.AddInt32(c.calls, 1)
	return c.inner.AccessToken(ctx)
}

func (c countingUnavailable) Invalidate(t string) { c.inner.Invalidate(t) }

// Public paths never consult a token source, so doctor, `/backend` and discovery keep
// working and can EXPLAIN the fault. A fail-closed credential that also broke those would
// leave the user with a dead binary and no way to find out why.
func TestUnavailableTokenSourceLeavesPublicPathsReachable(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.URL.Path)
		if r.Header.Get("Authorization") != "" {
			t.Errorf("a public path was sent an Authorization header")
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer srv.Close()

	var calls int32
	c := NewClient(ClientConfig{
		BaseURL:     srv.URL,
		TokenSource: countingUnavailable{calls: &calls, inner: UnavailableTokenSource{Err: errors.New("broken state root")}},
		Retry:       RetryPolicy{MaxAttempts: 1},
	})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v — a fail-closed credential must not disable the public probes", err)
	}
	if len(seen) == 0 {
		t.Fatal("the health probe never reached the server")
	}
	if got := atomic.LoadInt32(&calls); got != 0 {
		t.Fatalf("the token source was consulted %d times on a public path, want 0", got)
	}
}

// Invalidate must not panic or pretend to do something. There is nothing behind the
// source to re-derive a credential from; a repaired root is picked up by rebuilding the
// layer, not by asking this value again.
func TestUnavailableTokenSourceInvalidateIsInert(t *testing.T) {
	UnavailableTokenSource{}.Invalidate("anything")
}
