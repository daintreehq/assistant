package auth

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Most of the listener's security properties belong to the HANDLER, and the handler
// needs no socket at all. Driving it through httptest.NewRecorder is not merely faster:
// it is the only way to exercise a non-loopback RemoteAddr, and it means a developer
// running the assistant on port 42813 cannot make CI skip the whole security suite while
// still reporting success. The three tests that genuinely need the bind path keep it,
// and FAIL rather than skip when the port is busy.

// handlerFixture builds a listener without binding anything.
func handlerFixture() *listener {
	return &listener{done: make(chan callbackOutcome, 1)}
}

// callbackRequest builds a synthetic callback with full control of Host and RemoteAddr.
func callbackRequest(path string, q url.Values, host, remote, method string) *http.Request {
	target := path
	if len(q) > 0 {
		target += "?" + q.Encode()
	}
	r := httptest.NewRequest(method, "http://"+callbackAddr()+target, nil)
	r.Host = host
	r.RemoteAddr = remote
	return r
}

// serveCallback runs one synthetic request through the handler.
func serveCallback(l *listener, state string, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	l.handle(state)(w, r)
	return w
}

// settled reports the outcome if one was recorded, without blocking.
func settled(l *listener) (callbackOutcome, bool) {
	select {
	case out := <-l.done:
		return out, true
	default:
		return callbackOutcome{}, false
	}
}

func TestRedirectURIIsTheOneCompiledValue(t *testing.T) {
	want := "http://127.0.0.1:42813/oauth/callback"
	if got := RedirectURI(); got != want {
		t.Fatalf("RedirectURI() = %q, want %q — this string is registered with the identity provider and cannot drift", got, want)
	}
}

func TestAValidCallbackYieldsTheCode(t *testing.T) {
	const state = "the-expected-state"
	l := handlerFixture()
	w := serveCallback(l, state, callbackRequest(CallbackPath,
		url.Values{"code": {"auth-code-123"}, "state": {state}},
		callbackAddr(), "127.0.0.1:51000", http.MethodGet))

	out, ok := settled(l)
	if !ok {
		t.Fatal("a valid callback did not settle the attempt")
	}
	if out.err != nil {
		t.Fatalf("outcome error: %v", out.err)
	}
	if out.code != "auth-code-123" {
		t.Fatalf("code = %q", out.code)
	}
	for _, secret := range []string{"auth-code-123", state} {
		if strings.Contains(w.Body.String(), secret) {
			t.Errorf("the success page leaked %q into the browser", secret)
		}
	}
}

// An attacker who can reach the port can feed us their own authorization code. State is
// what stops it — and a mismatch must NOT settle the attempt, or refusing it becomes a
// denial of service in its own right.
func TestAMismatchedStateIsRefusedWithoutSettling(t *testing.T) {
	for _, bad := range []url.Values{
		{"code": {"attacker-code"}, "state": {"wrong-state"}},
		{"code": {"attacker-code"}},                                // absent
		{"code": {"attacker-code"}, "state": {""}},                 // empty
		{"code": {"attacker-code"}, "state": {"expected-state-x"}}, // prefix-extended
		{"code": {"attacker-code"}, "state": {"expected-stat"}},    // prefix-truncated
	} {
		l := handlerFixture()
		w := serveCallback(l, "expected-state",
			callbackRequest(CallbackPath, bad, callbackAddr(), "127.0.0.1:51000", http.MethodGet))
		if w.Code != http.StatusForbidden {
			t.Errorf("state %v: status %d, want 403", bad["state"], w.Code)
		}
		if _, ok := settled(l); ok {
			t.Errorf("state %v: an unauthenticated request settled the attempt", bad["state"])
		}
		if l.mismatches.Load() != 1 {
			t.Errorf("state %v: the mismatch was not counted", bad["state"])
		}
	}
}

// Any page the user visits can issue
// `<img src="http://127.0.0.1:42813/oauth/callback?error=access_denied">`. The browser
// sets a valid Host, the peer is loopback, the method is GET and the path matches — so
// every check except state passes. With the error branch ahead of the state check, that
// image tag silently cancels a sign-in in progress.
func TestAForgedProviderErrorCannotCancelALoginInProgress(t *testing.T) {
	const state = "real-attempt-state"
	for _, forged := range []url.Values{
		{"error": {"access_denied"}},
		{"error": {"access_denied"}, "state": {"guessed"}},
		{"error": {"server_error"}},
		{"code": {"planted"}},
	} {
		l := handlerFixture()
		w := serveCallback(l, state,
			callbackRequest(CallbackPath, forged, callbackAddr(), "127.0.0.1:51000", http.MethodGet))
		if w.Code != http.StatusForbidden {
			t.Errorf("%v: status %d, want 403", forged, w.Code)
		}
		if out, ok := settled(l); ok {
			t.Errorf("%v: a forged request settled the attempt as %+v — this is a login DoS", forged, out)
		}
	}
}

// A real denial carries the real state, and IS a decision to honour.
func TestAGenuineDenialIsACancellation(t *testing.T) {
	const state = "s"
	l := handlerFixture()
	serveCallback(l, state, callbackRequest(CallbackPath, url.Values{
		"error":             {"access_denied"},
		"error_description": {"user denied"},
		"state":             {state},
	}, callbackAddr(), "127.0.0.1:51000", http.MethodGet))

	out, ok := settled(l)
	if !ok {
		t.Fatal("a genuine denial did not settle the attempt")
	}
	if !IsCancelled(out.err) {
		t.Fatalf("code = %q, want %q", CodeOf(out.err), CodeCancelled)
	}
}

// A page on any origin can point a request at 127.0.0.1:42813 under its own hostname.
func TestAForeignHostHeaderIsRefused(t *testing.T) {
	const state = "s"
	for _, host := range []string{
		"attacker.example", "attacker.example:42813", "localhost:42813",
		"127.0.0.1", "127.0.0.1:42814", "",
	} {
		l := handlerFixture()
		w := serveCallback(l, state, callbackRequest(CallbackPath,
			url.Values{"code": {"c"}, "state": {state}}, host, "127.0.0.1:51000", http.MethodGet))
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403 — a rebound request was served", host, w.Code)
		}
		if _, ok := settled(l); ok {
			t.Errorf("Host %q: settled the attempt", host)
		}
	}
}

// Unreachable through a socket bound to 127.0.0.1, which is exactly why it is worth
// asserting directly: this check is the backstop for the bind itself being wrong.
func TestANonLoopbackPeerIsRefused(t *testing.T) {
	const state = "s"
	for _, remote := range []string{"10.0.0.5:51000", "192.168.1.9:51000", "[2001:db8::1]:51000", "garbage"} {
		l := handlerFixture()
		w := serveCallback(l, state, callbackRequest(CallbackPath,
			url.Values{"code": {"c"}, "state": {state}}, callbackAddr(), remote, http.MethodGet))
		if w.Code != http.StatusForbidden {
			t.Errorf("RemoteAddr %q: status %d, want 403", remote, w.Code)
		}
	}
}

func TestOnlyGetOnTheExactPathIsServed(t *testing.T) {
	const state = "s"
	q := url.Values{"code": {"c"}, "state": {state}}

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		l := handlerFixture()
		w := serveCallback(l, state, callbackRequest(CallbackPath, q, callbackAddr(), "127.0.0.1:51000", method))
		if w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", method, w.Code)
		}
		if _, ok := settled(l); ok {
			t.Errorf("%s settled the attempt", method)
		}
	}
	for _, p := range []string{"/", "/callback", "/oauth/callback/extra", "/OAUTH/CALLBACK", "/oauth"} {
		l := handlerFixture()
		w := serveCallback(l, state, callbackRequest(p, q, callbackAddr(), "127.0.0.1:51000", http.MethodGet))
		if w.Code != http.StatusNotFound {
			t.Errorf("GET %s: status %d, want 404", p, w.Code)
		}
	}
}

// error_description arrives on a URL a browser was pointed at. It must reach neither the
// page nor the error text that lands in scrollback and the debug log.
func TestProviderErrorTextIsNeverRendered(t *testing.T) {
	const state = "s"
	const poison = "<script>alert(1)</script>\x1b[2J-SECRET-TOKEN"
	l := handlerFixture()
	w := serveCallback(l, state, callbackRequest(CallbackPath, url.Values{
		"error":             {"server_error"},
		"error_description": {poison},
		"state":             {state},
	}, callbackAddr(), "127.0.0.1:51000", http.MethodGet))

	body := w.Body.String()
	if strings.Contains(body, "SECRET-TOKEN") || strings.Contains(body, "<script>") || strings.Contains(body, "\x1b") {
		t.Errorf("provider text was reflected into the page: %q", body)
	}
	out, ok := settled(l)
	if !ok {
		t.Fatal("the error did not settle the attempt")
	}
	if strings.Contains(out.err.Error(), "SECRET-TOKEN") || strings.Contains(out.err.Error(), "\x1b") {
		t.Errorf("provider text reached the error message: %q", out.err.Error())
	}
}

// The page is inert by construction: there is no dynamic text in it to escape.
func TestTheCallbackPageCarriesNoQueryContentAtAll(t *testing.T) {
	// A distinctive state: a one-letter one matches ordinary prose in the page and the
	// assertion would fire on the page's own words rather than on a leak.
	const state = "STATE-MARKER"
	l := handlerFixture()
	w := serveCallback(l, state, callbackRequest(CallbackPath, url.Values{
		"code":  {"CODE-MARKER"},
		"state": {state},
		"extra": {"EXTRA-MARKER"},
	}, callbackAddr(), "127.0.0.1:51000", http.MethodGet))

	body := w.Body.String()
	for _, marker := range []string{"CODE-MARKER", "EXTRA-MARKER", state} {
		if strings.Contains(body, marker) {
			t.Errorf("%q appeared in the callback page", marker)
		}
	}
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("the callback page has no CSP")
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// A refreshed tab, a replayed URL, or a second attempt racing this one must not hand a
// stale code to the exchange after the outcome is decided.
func TestOnlyTheFirstCallbackSettlesTheAttempt(t *testing.T) {
	const state = "s"
	l := handlerFixture()
	for _, code := range []string{"first-code", "second-code", "third-code"} {
		serveCallback(l, state, callbackRequest(CallbackPath,
			url.Values{"code": {code}, "state": {state}}, callbackAddr(), "127.0.0.1:51000", http.MethodGet))
	}
	out, ok := settled(l)
	if !ok {
		t.Fatal("nothing settled")
	}
	if out.code != "first-code" {
		t.Fatalf("code = %q, want the FIRST callback's code", out.code)
	}
	if _, ok := settled(l); ok {
		t.Fatal("a second outcome was queued — the attempt is not single-shot")
	}
}

// --- the tests that genuinely need the real socket ---------------------------------

// requireRealListener binds the real fixed port. Unlike a skip, a busy port FAILS: these
// are the only tests covering the bind path, and silently skipping them is how a port
// regression ships green.
func requireRealListener(t *testing.T, state string) *listener {
	t.Helper()
	l, err := listen(state)
	if err != nil {
		t.Fatalf("could not bind the callback port %d: %v\n"+
			"These tests exercise the real bind path. Stop whatever holds 127.0.0.1:%d and re-run.",
			CallbackPort, err, CallbackPort)
	}
	t.Cleanup(l.close)
	return l
}

// The one test proving the handler is actually reachable at the registered URI.
func TestARealCallbackOverTheBoundSocket(t *testing.T) {
	const state = "real-state"
	l := requireRealListener(t, state)

	got := make(chan callbackOutcome, 1)
	go func() {
		c, err := l.wait(context.Background())
		got <- callbackOutcome{code: c, err: err}
	}()
	waitForListener(t)

	resp, err := http.Get(RedirectURI() + "?" + url.Values{
		"code": {"real-code"}, "state": {state},
	}.Encode())
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	out := <-got
	if out.err != nil {
		t.Fatalf("wait: %v", out.err)
	}
	if out.code != "real-code" {
		t.Fatalf("code = %q", out.code)
	}
	if strings.Contains(string(body), "real-code") {
		t.Error("the page leaked the code")
	}
}

// A silent 403 is right for an unauthenticated request, but a GENUINE mismatch (two
// flows open at once) would otherwise produce a bare five-minute timeout with no clue.
func TestASeenMismatchTurnsATimeoutIntoADiagnosis(t *testing.T) {
	l := requireRealListener(t, "s")
	l.mismatches.Add(1)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := l.wait(ctx)
	if CodeOf(err) != CodeStateMismatch {
		t.Fatalf("code = %q, want %q — a bare timeout hides that something DID come back",
			CodeOf(err), CodeStateMismatch)
	}
}

// The port is registered with the provider, so a collision is a named failure with a
// remedy — never a silent fallback to an unregistered port.
func TestAPortCollisionIsNamedAndNotWorkedAround(t *testing.T) {
	requireRealListener(t, "s")

	second, err := listen("s")
	if err == nil {
		second.close()
		t.Fatal("a second listener bound the same port")
	}
	if CodeOf(err) != CodeCallbackPortInUse {
		t.Fatalf("code = %q, want %q", CodeOf(err), CodeCallbackPortInUse)
	}
	var ae *Error
	if !asAuthError(err, &ae) || ae.Hint == "" {
		t.Fatal("a port collision must carry a hint — the user has to free the port themselves")
	}
	if !strings.Contains(ae.Message, fmt.Sprint(CallbackPort)) {
		t.Errorf("the message should name the port: %q", ae.Message)
	}
}

// http.Server.Shutdown closes only listeners it ADOPTED via Serve. Between listen() and
// wait() there are none, so a failure in between (the browser refusing to open) would
// otherwise leave this process holding the port and every retry colliding with itself.
func TestClosingBeforeServeStillReleasesThePort(t *testing.T) {
	l, err := listen("s")
	if err != nil {
		t.Fatalf("could not bind the callback port %d: %v", CallbackPort, err)
	}
	l.close() // never served — this is the browser-failed-to-open path

	l2, err := listen("s")
	if err != nil {
		t.Fatalf("the port was not released by close(): %v\n"+
			"Every later sign-in in this process would collide with itself.", err)
	}
	l2.close()
}

func TestTheAuthorizeURLCarriesTheRequiredParameters(t *testing.T) {
	m := validManifest()
	const verifier = "the-secret-verifier-value"
	attempt := pkceAttempt{State: "st", Verifier: verifier, Challenge: challengeS256(verifier)}
	raw, err := buildAuthorizeURL(&m, attempt)
	if err != nil {
		t.Fatalf("buildAuthorizeURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	q := u.Query()
	for k, want := range map[string]string{
		"response_type":         "code",
		"client_id":             m.ClientID,
		"redirect_uri":          RedirectURI(),
		"state":                 "st",
		"code_challenge":        challengeS256(verifier),
		"code_challenge_method": "S256",
		"scope":                 "openid email",
	} {
		if got := q.Get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The verifier is the secret that redeems the code. Checking the WHOLE URL, not one
	// parameter: leaking it under another key, in the fragment, or in the path would
	// pass a per-parameter assertion.
	if strings.Contains(raw, verifier) {
		t.Fatalf("the PKCE verifier leaked into the authorization URL: %s", raw)
	}
}

// waitForListener polls until the socket accepts, so the test is neither slow nor flaky.
func waitForListener(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", callbackAddr(), 50*time.Millisecond)
		if err == nil {
			c.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the callback listener never started accepting")
}
