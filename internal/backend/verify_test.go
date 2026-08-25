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
// trusted-local path could be routed through HTTP_PROXY, handing a remote proxy the spendable
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

		// A stated failure reason with NO flag and NO balance. Reading the fallback
		// literally ("nothing was reported, so nothing is wrong") would pass this green
		// while the response's own machine-readable half says the account is spent.
		{"reason alone, exhausted", KeyVerification{Valid: true, Reason: ReasonCreditsExhausted}, false},
		{"reason alone, rejected", KeyVerification{Valid: true, Reason: ReasonProviderRejected}, false},
		{"reason alone, unrecognised", KeyVerification{Valid: true, Reason: "account_suspended"}, false},
		// `ok` is a stated SUCCESS and must not be caught by the same rule.
		{"reason ok, nothing else", KeyVerification{Valid: true, Reason: ReasonOK}, true},
		// The explicit flag still wins over the reason, in both directions — it is the
		// backend's own verdict, and the reason is its label for that verdict.
		{"flag beats a contradictory reason", KeyVerification{Valid: true, Usable: b(true), Reason: ReasonCreditsExhausted}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.v.IsUsable(); got != tc.want {
				t.Errorf("IsUsable() = %v, want %v", got, tc.want)
			}
		})
	}
}

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
