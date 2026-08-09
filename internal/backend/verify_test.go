package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// verifyServer answers capabilities plus a scripted verify response. verifyStatus 0
// means "route not registered" — the older-backend case.
func verifyServer(t *testing.T, verifyStatus int, verifyBody any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/daintree/capabilities":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Capabilities{Protocol: ProtocolRange{Min: 2, Max: 2}})
		case "/v1/daintree/auth/verify":
			if verifyStatus == 0 {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(verifyStatus)
			_ = json.NewEncoder(w).Encode(verifyBody)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func verifyClient(url string) *Client {
	return NewClient(ClientConfig{BaseURL: url, APIKey: "sk-test-0123456789", Retry: RetryPolicy{MaxAttempts: 1}})
}

func TestCheckSignInAcceptsAValidKey(t *testing.T) {
	limit := 7.5
	srv := verifyServer(t, http.StatusOK, KeyVerification{
		Valid: true, Detail: "ok", Label: "my-key", LimitRemaining: &limit,
	})

	v, warning, err := CheckSignIn(context.Background(), verifyClient(srv.URL))
	if err != nil {
		t.Fatalf("CheckSignIn: %v", err)
	}
	if warning != "" {
		t.Fatalf("a fully verified sign-in must carry no warning, got %q", warning)
	}
	if !v.Valid || v.Label != "my-key" {
		t.Fatalf("verification = %+v", v)
	}
	if v.LimitRemaining == nil || *v.LimitRemaining != 7.5 {
		t.Fatalf("LimitRemaining not carried through: %+v", v.LimitRemaining)
	}
}

// The whole point of the endpoint: a key that is structurally fine but that the
// provider does not accept must FAIL sign-in, not sail through to fail on turn one.
func TestCheckSignInRejectsAnInvalidKey(t *testing.T) {
	srv := verifyServer(t, http.StatusOK, KeyVerification{Valid: false, Detail: "The provider rejected this API key."})

	_, _, err := CheckSignIn(context.Background(), verifyClient(srv.URL))
	if err == nil {
		t.Fatal("an invalid key must fail sign-in")
	}
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("error must be identifiable as a key rejection, got %v", err)
	}
}

// A provider-supplied reason (expired, over quota) must survive to the user rather than
// be flattened into the generic sentence.
func TestCheckSignInKeepsAProviderSuppliedReason(t *testing.T) {
	srv := verifyServer(t, http.StatusOK, KeyVerification{Valid: false, Detail: "Key has expired."})

	_, _, err := CheckSignIn(context.Background(), verifyClient(srv.URL))
	if err == nil || !strings.Contains(err.Error(), "Key has expired.") {
		t.Fatalf("the provider's reason must be preserved, got %v", err)
	}
}

// A backend without the endpoint (every deployment predating it) must still allow
// sign-in — refusing would be a self-inflicted outage — but must say what was skipped.
func TestCheckSignInDowngradesWhenTheBackendCannotVerify(t *testing.T) {
	srv := verifyServer(t, 0, nil) // no verify route

	_, warning, err := CheckSignIn(context.Background(), verifyClient(srv.URL))
	if err != nil {
		t.Fatalf("an unsupported verify endpoint must not fail sign-in: %v", err)
	}
	if warning == "" {
		t.Fatal("an unverified sign-in must warn that the key was not checked")
	}
	if !strings.Contains(warning, "first message") {
		t.Fatalf("the warning should say when a bad key would surface, got %q", warning)
	}
}

// "Could not check" must never be reported as "invalid" — that would send the user
// hunting for a credential problem they do not have.
func TestCheckSignInTreatsAnUnreachableProviderAsAWarning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/daintree/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Capabilities{Protocol: ProtocolRange{Min: 2, Max: 2}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":{"code":"upstream_error","message":"provider unreachable"}}`)
	}))
	defer srv.Close()

	_, warning, err := CheckSignIn(context.Background(), verifyClient(srv.URL))
	if err != nil {
		t.Fatalf("an unreachable provider must not be reported as an invalid key: %v", err)
	}
	if warning == "" {
		t.Fatal("an unconfirmed key must warn")
	}
}

// A wrong URL or a mangled header stops at the capabilities gate — before any question
// about the key's validity is even meaningful.
func TestCheckSignInFailsWhenTheEndpointIsNotABackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	if _, _, err := CheckSignIn(context.Background(), verifyClient(srv.URL)); err == nil {
		t.Fatal("a non-backend endpoint must fail sign-in")
	}
}
