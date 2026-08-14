package backend

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// The one lenient case: a LOOPBACK backend without the endpoint still signs in — the
// local development loop must keep working — but must say what was skipped.
// (verifyClient points at an httptest server, which is always 127.0.0.1.)
func TestCheckSignInDowngradesWhenALocalBackendCannotVerify(t *testing.T) {
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

// remoteClient points a client whose BaseURL is a REMOTE spelling at a local test
// server, by rewriting the request host in a RoundTripper. The strictness branch keys
// off BaseURL, so this is the only way to exercise it without real network calls.
// Request construction, path, headers, and Client.BaseURL() all stay realistic — only
// the routing is substituted.
func remoteClient(t *testing.T, baseURL string, target *httptest.Server) *Client {
	t.Helper()
	u, err := url.Parse(target.URL)
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		r = r.Clone(r.Context())
		r.URL.Scheme, r.URL.Host, r.Host = u.Scheme, u.Host, u.Host
		return target.Client().Transport.RoundTrip(r)
	})
	return NewClient(ClientConfig{
		BaseURL:    baseURL,
		APIKey:     "sk-or-v1-test0123456789",
		HTTPClient: &http.Client{Transport: rt},
		Retry:      RetryPolicy{MaxAttempts: 1},
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A remote backend that does not serve /v1/daintree/auth/verify fails sign-in. Warning
// through would persist an unverified SPENDABLE key — exactly what verification exists
// to prevent.
//
// The table is the alias surface: every one of these spellings reaches a remote host, so
// every one must take the strict path. An "is this the official endpoint?" test fails
// OPEN on each of them; the loopback test fails closed.
func TestCheckSignInFailsWhenARemoteBackendCannotVerify(t *testing.T) {
	for _, baseURL := range []string{
		DefaultBaseURL,
		DefaultBaseURL + "/",
		"HTTPS://Assistant.Daintree.ORG",
		"https://assistant.daintree.org:443",     // default port, spelled out
		"https://assistant.daintree.org.",        // trailing DNS root dot
		"https://staging.assistant.daintree.org", // a different deployment of ours
		"https://someone-elses-backend.example",  // an unrelated custom endpoint
	} {
		t.Run(baseURL, func(t *testing.T) {
			srv := verifyServer(t, 0, nil) // no verify route

			_, warning, err := CheckSignIn(context.Background(), remoteClient(t, baseURL, srv))
			if err == nil {
				t.Fatal("a remote endpoint without /v1/daintree/auth/verify must fail sign-in")
			}
			if !errors.Is(err, ErrBackendIncompatible) {
				t.Fatalf("the failure must be identifiable as a backend-compatibility problem, got %v", err)
			}
			if errors.Is(err, ErrKeyRejected) {
				t.Fatal("an unsupported route must never be reported as a bad key — the fixes are opposite")
			}
			if warning != "" {
				t.Fatalf("a hard failure must not also warn, got %q", warning)
			}
		})
	}
}

// 405/501 are the same evidence as 404: the client issues the contractually correct
// POST, so "method not allowed" / "not implemented" means the route is absent or
// intercepted. Letting them fall through to the soft "could not confirm" branch would
// quietly restore warn-and-persist for a remote endpoint.
func TestCheckSignInTreatsMethodNotAllowedAndNotImplementedAsUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/daintree/capabilities" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(Capabilities{Protocol: ProtocolRange{Min: 2, Max: 2}})
					return
				}
				w.WriteHeader(status)
			}))
			defer srv.Close()

			if _, _, err := CheckSignIn(context.Background(), remoteClient(t, DefaultBaseURL, srv)); !errors.Is(err, ErrBackendIncompatible) {
				t.Fatalf("status %d on a remote endpoint must fail sign-in as incompatible, got %v", status, err)
			}
		})
	}
}

// The strict branch is scoped to the missing-CAPABILITY case only. A provider we cannot
// reach still warns, even on a remote endpoint: "could not check" is not a verdict.
func TestCheckSignInRemoteUnreachableProviderStillWarns(t *testing.T) {
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

	_, warning, err := CheckSignIn(context.Background(), remoteClient(t, DefaultBaseURL, srv))
	if err != nil {
		t.Fatalf("an unreachable provider must not be reported as an incompatible backend: %v", err)
	}
	if warning == "" {
		t.Fatal("an unconfirmed key must warn")
	}
}

// A remote endpoint keeps every OTHER verdict unchanged — strictness applies to the
// missing-capability case alone, not to what the provider says about the key.
func TestCheckSignInRemoteEndpointStillAcceptsAValidKey(t *testing.T) {
	srv := verifyServer(t, http.StatusOK, KeyVerification{Valid: true, Label: "ci-key"})

	v, warning, err := CheckSignIn(context.Background(), remoteClient(t, DefaultBaseURL, srv))
	if err != nil {
		t.Fatalf("CheckSignIn: %v", err)
	}
	if warning != "" || !v.Valid {
		t.Fatalf("verification = %+v, warning = %q", v, warning)
	}
}

// A remote endpoint that rejects the key still reports a KEY problem, not an endpoint
// problem — the two carry opposite next actions.
func TestCheckSignInRemoteEndpointStillReportsAKeyRejection(t *testing.T) {
	srv := verifyServer(t, http.StatusOK, KeyVerification{Valid: false, Detail: "Key has expired."})

	_, _, err := CheckSignIn(context.Background(), remoteClient(t, DefaultBaseURL, srv))
	if !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("want ErrKeyRejected, got %v", err)
	}
	if errors.Is(err, ErrBackendIncompatible) {
		t.Fatal("a provider verdict must not be reported as an incompatible backend")
	}
}

// The bearer token IS a spendable credential, and a backend we do not control can echo
// it back inside a 200 verdict. That body reaches the login confirmation, the /auth
// view, and the cockpit sheet — which renders on the NORMAL screen buffer, so a leak
// persists in host scrollback long after the session. VerifyKey must scrub it at the
// single choke point rather than trusting N display sites to remember.
func TestVerifyKeyScrubsTheKeyEchoedBackInTheVerdict(t *testing.T) {
	const key = "sk-or-v1-supersecret0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KeyVerification{
			Valid:  false,
			Detail: "the provider rejected " + key,
			Label:  "key " + key,
		})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: key, Retry: RetryPolicy{MaxAttempts: 1}})
	v, err := c.VerifyKey(context.Background())
	if err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if strings.Contains(v.Detail, key) {
		t.Errorf("Detail leaked the key: %q", v.Detail)
	}
	if strings.Contains(v.Label, key) {
		t.Errorf("Label leaked the key: %q", v.Label)
	}
	// The surrounding text must survive — scrubbing must not blank a real reason.
	if !strings.Contains(v.Detail, "the provider rejected") {
		t.Errorf("scrubbing destroyed the reason: %q", v.Detail)
	}
}

// The same leak, one layer up: CheckSignIn folds Detail into the ErrKeyRejected message,
// which both sign-in surfaces render with %v before any scrubbing fallback.
func TestCheckSignInErrorDoesNotLeakTheKey(t *testing.T) {
	const key = "sk-or-v1-supersecret0123456789"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/daintree/capabilities" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(Capabilities{Protocol: ProtocolRange{Min: 2, Max: 2}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KeyVerification{Valid: false, Detail: "bad key " + key})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: key, Retry: RetryPolicy{MaxAttempts: 1}})
	v, _, err := CheckSignIn(context.Background(), c)
	if err == nil {
		t.Fatal("an invalid key must fail sign-in")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("the sign-in error leaked the key: %q", err.Error())
	}
	if strings.Contains(v.Detail, key) {
		t.Errorf("the returned verification leaked the key: %q", v.Detail)
	}
}

// VerifyKey must use POST. A GET would 405 against the real backend and — now that 405
// maps to "unsupported" — would fail every remote sign-in for the wrong reason.
func TestVerifyKeyUsesPost(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(KeyVerification{Valid: true})
	}))
	defer srv.Close()

	c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: "sk-test-0123456789", Retry: RetryPolicy{MaxAttempts: 1}})
	if _, err := c.VerifyKey(context.Background()); err != nil {
		t.Fatalf("VerifyKey: %v", err)
	}
	if method != http.MethodPost || path != "/v1/daintree/auth/verify" {
		t.Fatalf("VerifyKey issued %s %s, want POST /v1/daintree/auth/verify", method, path)
	}
}

// The lenient path exists for the local development loop and nothing else. Every
// spelling that reaches THIS machine qualifies; every spelling that reaches a remote
// host does not — including ones designed to look local.
func TestAllowsUnverifiedSignIn(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want bool
	}{
		{LocalBaseURL, true},
		{"http://localhost:8473", true},
		{"http://localhost.:8473", true}, // trailing DNS root dot
		{"http://127.0.0.2:8473", true},  // all of 127.0.0.0/8 is this machine
		{"http://[::1]:8473", true},      // bracketed IPv6 loopback
		{"https://LOCALHOST:8473", true}, // case
		{DefaultBaseURL, false},
		{"https://assistant.daintree.org:443", false},
		{"https://assistant.daintree.org.", false},
		{"https://localhost.evil.example", false}, // looks local, resolves remote
		{"https://evil.example/127.0.0.1", false}, // loopback in the PATH, not the host
		{"http://127.0.0.1.evil.example", false},
		{"", false},     // unparseable/empty is remote, i.e. strict
		{"://x", false}, // malformed is remote, i.e. strict
	} {
		if got := AllowsUnverifiedSignIn(tc.url); got != tc.want {
			t.Errorf("AllowsUnverifiedSignIn(%q) = %v, want %v", tc.url, got, tc.want)
		}
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

// An upstream we do not control can echo the Authorization header into an ERROR body —
// a 502 from the provider, a proxy's error page. Error.Message reaches the turn's error
// rendering (host scrollback, never cleared) and the debug log, so the client must scrub
// on the way out rather than trust every sink to remember.
func TestBackendErrorsDoNotLeakTheKey(t *testing.T) {
	const key = "sk-or-v1-supersecret0123456789"

	t.Run("structured envelope", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, `{"error":{"code":"upstream_error","message":"provider rejected `+key+`","param":"`+key+`"}}`)
		}))
		defer srv.Close()

		c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: key, Retry: RetryPolicy{MaxAttempts: 1}})
		_, err := c.Capabilities(context.Background())
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("error text leaked the key: %q", err.Error())
		}
		var be *Error
		if errors.As(err, &be) {
			if strings.Contains(be.Message, key) || strings.Contains(be.Param, key) {
				t.Errorf("structured fields leaked the key: %+v", be)
			}
			// The surrounding diagnosis must survive the scrub.
			if !strings.Contains(be.Message, "provider rejected") {
				t.Errorf("scrubbing destroyed the message: %q", be.Message)
			}
		}
	})

	t.Run("raw undecodable body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = io.WriteString(w, "<html>proxy error for token "+key+"</html>")
		}))
		defer srv.Close()

		c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: key, Retry: RetryPolicy{MaxAttempts: 1}})
		if _, err := c.Capabilities(context.Background()); err == nil || strings.Contains(err.Error(), key) {
			t.Errorf("raw-body error leaked the key: %v", err)
		}
	})

	t.Run("terminal SSE error event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: error\ndata: {\"error\":{\"code\":\"upstream_error\",\"message\":\"bad key "+key+"\"}}\n\n")
		}))
		defer srv.Close()

		c := NewClient(ClientConfig{BaseURL: srv.URL, APIKey: key, Retry: RetryPolicy{MaxAttempts: 1}})
		_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
		if err == nil {
			t.Fatal("want a stream error")
		}
		if strings.Contains(err.Error(), key) {
			t.Errorf("SSE error leaked the key: %q", err.Error())
		}
	})
}

// Routing must agree with classification. Every spelling AllowsUnverifiedSignIn accepts
// as local must also bypass the proxy — otherwise a spelling classified onto the lenient
// sign-in path could be routed through HTTP_PROXY, handing a remote proxy the spendable
// bearer token (in clear text over http) and letting it answer for the backend.
//
// Go's stock ProxyFromEnvironment bypasses only the exact lowercase "localhost" and
// parseable loopback IP literals, so the four table entries below are precisely the gap.
func TestProxyIsBypassedForEverySpellingClassifiedAsLocal(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://proxy.invalid:3128")
	t.Setenv("HTTPS_PROXY", "http://proxy.invalid:3128")
	t.Setenv("NO_PROXY", "")

	for _, raw := range []string{
		"http://127.0.0.1:8473/v1/daintree/capabilities",
		"http://localhost:8473/v1/daintree/capabilities",
		"http://LOCALHOST:8473/v1/daintree/capabilities",
		"http://localhost.:8473/v1/daintree/capabilities",
		"http://dev.localhost:8473/v1/daintree/capabilities",
		"http://127.0.0.1.:8473/v1/daintree/capabilities",
		"http://[::1]:8473/v1/daintree/capabilities",
	} {
		if !AllowsUnverifiedSignIn(raw) {
			t.Fatalf("test premise broken: %q is not classified as local", raw)
		}
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parse %q: %v", raw, err)
		}
		proxy, err := proxyExceptLoopback(&http.Request{URL: u, Host: u.Host})
		if err != nil {
			t.Fatalf("proxyExceptLoopback(%q): %v", raw, err)
		}
		if proxy != nil {
			t.Errorf("%q is classified local but would be routed through %s", raw, proxy)
		}
	}

	// The converse: a remote endpoint still honours the environment's proxy.
	u, _ := url.Parse("https://assistant.daintree.org/v1/daintree/capabilities")
	proxy, err := proxyExceptLoopback(&http.Request{URL: u, Host: u.Host})
	if err != nil {
		t.Fatalf("proxyExceptLoopback(remote): %v", err)
	}
	if proxy == nil {
		t.Error("a remote endpoint must still use the configured proxy")
	}
}

// `usable` answers a different question from `valid`, and the CLI must prefer the
// backend's own answer while still working against a deployment that predates the field.
// The absent case is the one that bites: `usable` is a pointer because decoding a
// missing field as `false` would tell every user of an older backend that their working
// key has no credit.
func TestKeyVerificationIsUsable(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	b := func(v bool) *bool { return &v }

	cases := []struct {
		name string
		v    KeyVerification
		want bool
	}{
		{"backend says usable", KeyVerification{Valid: true, Usable: b(true), Reason: "ok"}, true},
		{"backend says exhausted", KeyVerification{Valid: true, Usable: b(false), Reason: "credits_exhausted"}, false},
		// The server's verdict wins over our own arithmetic: it judges conservatively and
		// knows things about the account shape that a bare number does not.
		{"backend verdict beats a stale limit", KeyVerification{Valid: true, Usable: b(true), LimitRemaining: f(0)}, true},
		{"backend verdict beats a healthy limit", KeyVerification{Valid: true, Usable: b(false), LimitRemaining: f(99)}, false},

		// Older backend: fall back to the limit, and treat "not reported" as fine — an
		// unlimited or pay-as-you-go key reports no limit at all.
		{"older backend, credit left", KeyVerification{Valid: true, LimitRemaining: f(1.5)}, true},
		{"older backend, spent", KeyVerification{Valid: true, LimitRemaining: f(0)}, false},
		{"older backend, negative", KeyVerification{Valid: true, LimitRemaining: f(-2)}, false},
		{"older backend, no limit reported", KeyVerification{Valid: true}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.IsUsable(); got != tc.want {
				t.Errorf("IsUsable() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Narrowing IsAuth() to exclude the provider-account codes opened a hole here: those
// codes stopped matching the hard-failure branch and would have fallen into the soft
// "could not confirm" branch, which PERSISTS the sign-in. Persisting a key the provider
// has just told us is unusable is precisely what verification exists to prevent.
//
// The route is contracted to report a rejection as 200 {"valid": false} instead, so this
// is defence against the contract moving rather than a live path — which is exactly why
// it needs a test: nothing else would notice.
func TestCheckSignInTreatsProviderAccountErrorsAsVerdicts(t *testing.T) {
	cases := []struct {
		name      string
		code      string
		status    int
		wantErr   bool
		wantWarn  bool
		wantInErr string
	}{
		{"revoked key is a hard failure", CodeProviderInvalidAPIKey, 401, true, false, "does not recognise this key"},
		{"forbidden key is a hard failure", CodeProviderKeyForbidden, 403, true, false, "will not let this key use this model"},
		// Fixable, so it warns and persists — the same treatment a `usable:false`
		// verdict gets, and for the same reason.
		{"no credit warns and persists", CodeProviderInsufficientCredit, 402, false, true, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/auth/verify") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.status)
					_, _ = io.WriteString(w, `{"error":{"type":"authentication_error","code":"`+tc.code+`","message":"nope"}}`)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"server_version":"1","protocol":{"min":2,"max":2}}`)
			}))
			t.Cleanup(srv.Close)

			c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1}})
			_, warning, err := CheckSignIn(context.Background(), c)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("%s was accepted — an unusable key would have been persisted", tc.code)
				}
				if !errors.Is(err, ErrKeyRejected) {
					t.Errorf("error is %v, want it to wrap ErrKeyRejected so sign-in renders it as a key verdict", err)
				}
				// The precise reason must survive the wrap. Both sign-in surfaces
				// recover it exactly this way to say WHICH account problem it was;
				// wrapping only the sentinel would silently re-generalise the verdict
				// one line after computing it.
				var berr *Error
				if !errors.As(err, &berr) {
					t.Fatalf("the underlying *backend.Error did not survive wrapping: %v", err)
				}
				if tc.wantInErr != "" && !strings.Contains(berr.ProviderAccountReason(), tc.wantInErr) {
					t.Errorf("reason %q does not name the specific problem (%q)", berr.ProviderAccountReason(), tc.wantInErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected hard failure: %v", err)
			}
			if tc.wantWarn && warning == "" {
				t.Error("persisted with no warning — the user gets no hint that turns will fail")
			}
		})
	}
}

// `Usable` must decode through the REAL response path, not just as a struct literal.
// The pointer exists to keep "absent" distinct from "false", and that distinction only
// holds if the JSON tag and type survive — a plain bool here would silently declare
// every key on an older deployment unusable, and a literal-only test cannot see it.
func TestVerifyKeyDecodesUsableAndReason(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantUsable *bool
		wantReason string
		wantIsable bool
	}{
		{
			name:       "usable true",
			body:       `{"valid":true,"usable":true,"reason":"ok","detail":"fine"}`,
			wantUsable: boolPtrForTest(true),
			wantReason: "ok",
			wantIsable: true,
		},
		{
			// The case a plain bool cannot distinguish from the one below it.
			name:       "explicitly unusable",
			body:       `{"valid":true,"usable":false,"reason":"credits_exhausted","detail":"empty"}`,
			wantUsable: boolPtrForTest(false),
			wantReason: "credits_exhausted",
			wantIsable: false,
		},
		{
			name:       "older backend omits both",
			body:       `{"valid":true,"detail":"fine"}`,
			wantUsable: nil,
			wantReason: "",
			wantIsable: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(srv.Close)

			v, err := NewClient(ClientConfig{BaseURL: srv.URL}).VerifyKey(context.Background())
			if err != nil {
				t.Fatalf("VerifyKey: %v", err)
			}
			switch {
			case tc.wantUsable == nil && v.Usable != nil:
				t.Errorf("Usable = %v, want nil (absent must not decode as false)", *v.Usable)
			case tc.wantUsable != nil && v.Usable == nil:
				t.Errorf("Usable = nil, want %v", *tc.wantUsable)
			case tc.wantUsable != nil && *v.Usable != *tc.wantUsable:
				t.Errorf("Usable = %v, want %v", *v.Usable, *tc.wantUsable)
			}
			if v.Reason != tc.wantReason {
				t.Errorf("Reason = %q, want %q", v.Reason, tc.wantReason)
			}
			if got := v.IsUsable(); got != tc.wantIsable {
				t.Errorf("IsUsable() = %v, want %v", got, tc.wantIsable)
			}
		})
	}
}

func boolPtrForTest(b bool) *bool { return &b }
