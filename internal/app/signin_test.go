package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
)

// capsServer answers the authenticated capabilities probe, recording the bearer it saw.
func capsServer(t *testing.T, ok bool) (*httptest.Server, *string) {
	t.Helper()
	var seen string
	// Serves BOTH sign-in probes. Serving only capabilities would silently route every
	// test through the "backend can't verify" warning path instead of the real one —
	// exactly the vacuous-coverage trap this fixture exists to avoid.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"code":"invalid_api_key","message":"nope"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/daintree/capabilities":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"server_version": "test", "protocol": map[string]int{"min": 2, "max": 2},
			})
		case "/v1/daintree/auth/verify":
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": true, "detail": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// signInApp builds the minimum App that SignIn needs: a swappable backend and a config
// pointing at an isolated credentials file. Deliberately NOT a full app.Create — SignIn
// must not depend on the store, MCP, or the session.
func signInApp(t *testing.T, startURL string) *App {
	t.Helper()
	a := &App{Config: config.AppConfig{
		BackendURL:      startURL,
		APIKey:          "sk-old-0123456789",
		CredentialsPath: credentials.Path(t.TempDir()),
	}}
	a.backendSwap = backend.NewSwappable(backend.NewClient(backendClientConfig(a.Config)))
	a.Backend = a.backendSwap
	return a
}

// The contract in one test: verify, persist, then swap — so the NEXT call goes to the
// new endpoint with no restart and no re-wiring of anything downstream.
func TestSignInVerifiesPersistsAndSwaps(t *testing.T) {
	srv, seenAuth := capsServer(t, true)
	a := signInApp(t, "http://127.0.0.1:1")

	next := credentials.Credentials{BaseURL: srv.URL, APIKey: "sk-or-v1-new0123456789"}
	if err := a.SignIn(context.Background(), next); err != nil {
		t.Fatalf("SignIn: %v", err)
	}

	if want := "Bearer sk-or-v1-new0123456789"; *seenAuth != want {
		t.Fatalf("verification bearer = %q, want %q", *seenAuth, want)
	}
	// The live client — the same object Session/watchers/asyncwork are holding — must
	// now point at the new endpoint.
	if got := a.Backend.BaseURL(); got != srv.URL {
		t.Fatalf("live backend = %q, want %q", got, srv.URL)
	}
	if a.Config.APIKey != next.APIKey || a.Config.BackendURL != next.BaseURL {
		t.Fatalf("config not updated: %+v", a.Config)
	}
	stored, ok, err := credentials.Load(a.Config.CredentialsPath)
	if err != nil || !ok {
		t.Fatalf("not persisted: ok=%v err=%v", ok, err)
	}
	if stored != next {
		t.Fatalf("stored %+v, want %+v", stored, next)
	}
	// The key was VERIFIED, not merely accepted structurally — no caveat.
	if w := a.LastSignInWarning(); w != "" {
		t.Fatalf("a fully verified sign-in must carry no warning, got %q", w)
	}
}

// A backend that rejects the key upstream must fail the sign-in even though its
// capabilities endpoint answered 200 — the whole reason the verify probe exists.
func TestSignInRejectsAKeyTheProviderRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/daintree/capabilities":
			_ = json.NewEncoder(w).Encode(map[string]any{"protocol": map[string]int{"min": 2, "max": 2}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"valid": false, "detail": "Key has expired."})
		}
	}))
	defer srv.Close()

	a := signInApp(t, "http://original.test")
	err := a.SignIn(context.Background(), credentials.Credentials{BaseURL: srv.URL, APIKey: "sk-or-v1-expired0123456789"})
	if err == nil {
		t.Fatal("a provider-rejected key must fail sign-in")
	}
	if !strings.Contains(err.Error(), "Key has expired.") {
		t.Fatalf("the provider's reason must reach the user, got %v", err)
	}
	if _, ok, _ := credentials.Load(a.Config.CredentialsPath); ok {
		t.Fatal("a rejected key must not be persisted")
	}
}

// SECURITY: a backend we do not control can echo the bearer into an error body, and
// that text lands in a cockpit sheet on the NORMAL screen buffer (i.e. scrollback).
func TestSignInScrubsTheKeyFromBackendText(t *testing.T) {
	const key = "sk-or-v1-leakyvalue0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"code":"boom","message":"upstream said Bearer `+key+` is wrong"}}`)
	}))
	defer srv.Close()

	a := signInApp(t, "http://original.test")
	err := a.SignIn(context.Background(), credentials.Credentials{BaseURL: srv.URL, APIKey: key})
	if err == nil {
		t.Fatal("expected the sign-in to fail")
	}
	if strings.Contains(err.Error(), key) {
		t.Fatalf("the key leaked into the displayed error: %v", err)
	}
}

// A rejected sign-in must change NOTHING observable: not the live client, not the
// config, not the file. Otherwise a typo would break the running session.
func TestSignInFailureLeavesEverythingUntouched(t *testing.T) {
	srv, _ := capsServer(t, false)
	a := signInApp(t, "http://original.test")
	beforeURL, beforeKey := a.Backend.BaseURL(), a.Config.APIKey

	err := a.SignIn(context.Background(), credentials.Credentials{BaseURL: srv.URL, APIKey: "sk-bad-0123456789"})
	if err == nil {
		t.Fatal("SignIn must fail when the endpoint rejects the key")
	}
	if !strings.Contains(err.Error(), "rejected the key") {
		t.Fatalf("error should name the cause, got %v", err)
	}
	if a.Backend.BaseURL() != beforeURL {
		t.Fatalf("live backend changed to %q on a failed sign-in", a.Backend.BaseURL())
	}
	if a.Config.APIKey != beforeKey {
		t.Fatal("config key changed on a failed sign-in")
	}
	if _, ok, _ := credentials.Load(a.Config.CredentialsPath); ok {
		t.Fatal("a failed sign-in must not write credentials")
	}
}

func TestSignInRejectsIncompleteCredentials(t *testing.T) {
	a := signInApp(t, "http://original.test")
	if err := a.SignIn(context.Background(), credentials.Credentials{BaseURL: "https://x.test"}); err == nil {
		t.Fatal("SignIn without a key must fail before any network call")
	}
}

func TestSignInStatusRedactsAndReadsTheLiveEndpoint(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", "")
	t.Setenv("DAINTREE_API_KEY", "")
	a := signInApp(t, "https://endpoint.test")

	st := a.SignInStatus()
	if st.KeyRedacted == a.Config.APIKey {
		t.Fatal("SignInStatus leaked the raw key")
	}
	if !st.SignedIn {
		t.Fatal("a configured key must report as signed in")
	}
	// Reading the endpoint from the CLIENT (not the config) is what keeps /auth honest
	// after a swap.
	if st.Endpoint != a.Backend.BaseURL() {
		t.Fatalf("status endpoint %q != live client %q", st.Endpoint, a.Backend.BaseURL())
	}
	if st.EnvOverride != "" {
		t.Fatalf("no env override is set, got %q", st.EnvOverride)
	}
}

// An exported override silently beats a stored sign-in, so the status must surface it —
// otherwise /login appears to succeed while every turn keeps hitting the old endpoint.
func TestSignInStatusReportsEnvOverride(t *testing.T) {
	t.Setenv("DAINTREE_BACKEND_URL", "http://forced.test")
	a := signInApp(t, "https://endpoint.test")
	if got := a.SignInStatus().EnvOverride; got != "DAINTREE_BACKEND_URL" {
		t.Fatalf("EnvOverride = %q, want DAINTREE_BACKEND_URL", got)
	}
}

// SignIn mutates App.Config from the cockpit's command goroutine while turns copy the
// WHOLE config on agent goroutines (snapshotConfig / buildContext). That is a genuine
// data race unless both go through cfgMu — the same hazard SetTier already solves, and
// one the original cut of this code got wrong. Run with -race.
func TestSignInIsRaceFreeAgainstConfigReaders(t *testing.T) {
	srv, _ := capsServer(t, true)
	a := signInApp(t, srv.URL)

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// The two unlocked reads the first cut shipped.
					_ = a.snapshotConfig()
					_ = a.SignInStatus()
					_ = a.LastSignInWarning()
				}
			}
		}()
	}

	for i := range 20 {
		key := "sk-or-v1-rotating" + string(rune('a'+i%26)) + "0123456789"
		if err := a.SignIn(context.Background(), credentials.Credentials{BaseURL: srv.URL, APIKey: key}); err != nil {
			t.Errorf("SignIn #%d: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()
}
