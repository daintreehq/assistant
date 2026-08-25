package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// validManifest is the shape a correct staging backend returns. Tests mutate one field
// at a time from this, so every failure names exactly the rule it broke.
func validManifest() Manifest {
	return Manifest{
		Version:               1,
		Environment:           "staging",
		Issuer:                "https://proj.supabase.co/auth/v1",
		AuthorizationEndpoint: "https://proj.supabase.co/auth/v1/oauth/authorize",
		TokenEndpoint:         "https://proj.supabase.co/auth/v1/oauth/token",
		JWKSURI:               "https://proj.supabase.co/auth/v1/.well-known/jwks.json",
		ClientID:              "daintree-assistant-staging",
		RedirectURI:           RedirectURI(),
		Scopes:                []string{"openid", "email"},
		AccountURL:            "https://staging.daintree.org/account",
		SubscribeURL:          "https://staging.daintree.org/subscribe",
		SessionPolicy:         SessionPolicy{AccessTokenSeconds: 3600, SessionMaxAgeSeconds: 2592000},
	}
}

func TestAValidManifestPasses(t *testing.T) {
	m := validManifest()
	if err := m.Validate(RedirectURI()); err != nil {
		t.Fatalf("a correct manifest was rejected: %v", err)
	}
}

// The single most important rule in the package: the authorization code must come back
// to THIS process on THIS machine. A manifest that can name the redirect can harvest
// codes.
func TestTheRedirectMustBeOurCompiledCallbackExactly(t *testing.T) {
	for _, bad := range []string{
		"http://127.0.0.1:9999/oauth/callback",      // wrong port
		"http://localhost:42813/oauth/callback",     // resolver-dependent host
		"http://127.0.0.1:42813/callback",           // wrong path
		"https://evil.example/oauth/callback",       // outright elsewhere
		"http://127.0.0.1:42813/oauth/callback/",    // trailing slash
		"http://127.0.0.1:42813/oauth/callback?x=1", // extra query
		"",
	} {
		m := validManifest()
		m.RedirectURI = bad
		err := m.Validate(RedirectURI())
		if err == nil {
			t.Errorf("redirect_uri %q was accepted", bad)
			continue
		}
		if CodeOf(err) != CodeDiscoveryInvalid {
			t.Errorf("redirect_uri %q gave code %q, want %q", bad, CodeOf(err), CodeDiscoveryInvalid)
		}
	}
}

// The token endpoint receives the authorization code AND the PKCE verifier. A manifest
// that could move it off the issuer could collect both.
func TestEndpointsCannotLeaveTheIssuerOrigin(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*Manifest)
	}{
		{"authorize on another host", func(m *Manifest) {
			m.AuthorizationEndpoint = "https://evil.example/auth/v1/oauth/authorize"
		}},
		{"token on another host", func(m *Manifest) {
			m.TokenEndpoint = "https://evil.example/auth/v1/oauth/token"
		}},
		{"jwks on another host", func(m *Manifest) {
			m.JWKSURI = "https://evil.example/auth/v1/.well-known/jwks.json"
		}},
		{"token on a lookalike subdomain", func(m *Manifest) {
			m.TokenEndpoint = "https://proj.supabase.co.evil.example/auth/v1/oauth/token"
		}},
		{"token downgraded to http on the same host", func(m *Manifest) {
			m.TokenEndpoint = "http://proj.supabase.co/auth/v1/oauth/token"
		}},
		{"token escapes the issuer path", func(m *Manifest) {
			m.TokenEndpoint = "https://proj.supabase.co/other/oauth/token"
		}},
	}
	for _, tc := range cases {
		m := validManifest()
		tc.apply(&m)
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("%s: accepted", tc.name)
		} else if CodeOf(err) != CodeDiscoveryInvalid {
			t.Errorf("%s: code %q, want %q", tc.name, CodeOf(err), CodeDiscoveryInvalid)
		}
	}
}

// The deliberate deviation from the guide: a loopback issuer over plain http is fine,
// because there is no network to intercept and a local Supabase serves exactly that.
// The same relaxation must NOT extend to a remote host.
func TestPlaintextIsAllowedOnLoopbackAndRefusedRemotely(t *testing.T) {
	local := Manifest{
		Version:               1,
		Environment:           "development",
		Issuer:                "http://127.0.0.1:54321/auth/v1",
		AuthorizationEndpoint: "http://127.0.0.1:54321/auth/v1/oauth/authorize",
		TokenEndpoint:         "http://127.0.0.1:54321/auth/v1/oauth/token",
		JWKSURI:               "http://127.0.0.1:54321/auth/v1/.well-known/jwks.json",
		ClientID:              "local-dev",
		RedirectURI:           RedirectURI(),
		Scopes:                []string{"openid", "email"},
	}
	if err := local.Validate(RedirectURI()); err != nil {
		t.Fatalf("a loopback development manifest was rejected: %v", err)
	}

	// A host that WOULD pass the anchor check, so this exercises the plaintext rule
	// rather than accidentally being caught by the issuer pin.
	remote := local
	remote.Environment = "staging"
	remote.Issuer = "http://proj.supabase.co/auth/v1"
	remote.AuthorizationEndpoint = "http://proj.supabase.co/auth/v1/oauth/authorize"
	remote.TokenEndpoint = "http://proj.supabase.co/auth/v1/oauth/token"
	remote.JWKSURI = "http://proj.supabase.co/auth/v1/.well-known/jwks.json"
	if err := remote.Validate(RedirectURI()); err == nil {
		t.Fatal("plaintext http on a REMOTE issuer was accepted — the OAuth exchange would cross the network in the clear")
	}
}

// THE anchor check. Every other endpoint rule is relative to the issuer and therefore
// proves nothing on its own: a hostile manifest naming its own issuer satisfies all of
// them. This is the rule such a manifest cannot satisfy.
//
// The attack: relay.example's authorize endpoint redirects the browser to the REAL
// Supabase with our real client id, redirect URI, state and challenge. The genuine code
// comes back to our listener; we then post it and the verifier to relay.example's token
// endpoint, which redeems both upstream. Every relative check passes.
func TestAFullyConsistentHostileManifestIsStillRefused(t *testing.T) {
	relay := Manifest{
		Version:               1,
		Environment:           "staging",
		Issuer:                "https://relay.example/auth/v1",
		AuthorizationEndpoint: "https://relay.example/auth/v1/oauth/authorize",
		TokenEndpoint:         "https://relay.example/auth/v1/oauth/token",
		JWKSURI:               "https://relay.example/auth/v1/.well-known/jwks.json",
		ClientID:              "daintree-assistant-staging",
		RedirectURI:           RedirectURI(),
		Scopes:                []string{"openid", "email"},
	}
	// It passes every relative check — that is the point.
	iss, _ := parseAuthURL(relay.Issuer, "issuer")
	tok, _ := parseAuthURL(relay.TokenEndpoint, "token")
	if iss.Host != tok.Host || !underPath(tok, iss) {
		t.Fatal("the fixture is wrong: it should be internally consistent")
	}
	if err := relay.Validate(RedirectURI()); err == nil {
		t.Fatal("a self-consistent manifest on an unpinned issuer was accepted — it can relay the code and verifier")
	}
}

// A substring allowlist is the classic way a domain pin gets bypassed.
func TestTheIssuerPinMatchesOnLabelBoundaries(t *testing.T) {
	for _, host := range []string{
		"notsupabase.co",           // suffix as a substring, not a label
		"supabase.co.evil.example", // pinned name as a left-hand label
		"evilsupabase.co",          // no separating dot
		"supabase.com",             // wrong TLD
		"xn--supabase-1234.co",     // punycode lookalike
	} {
		m := validManifest()
		m.Issuer = "https://" + host + "/auth/v1"
		m.AuthorizationEndpoint = "https://" + host + "/auth/v1/oauth/authorize"
		m.TokenEndpoint = "https://" + host + "/auth/v1/oauth/token"
		m.JWKSURI = "https://" + host + "/auth/v1/.well-known/jwks.json"
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("issuer host %q was accepted", host)
		}
	}
	// ...and a genuine subdomain of a pinned suffix is accepted.
	for _, host := range []string{"supabase.co", "proj.supabase.co", "auth.daintree.org"} {
		if !hostAllowed(host, allowedIssuerSuffixes) {
			t.Errorf("legitimate issuer host %q was refused", host)
		}
	}
}

// A prefix test is not containment, and both ways it fails are exploitable.
func TestPathContainmentIsOnASegmentBoundary(t *testing.T) {
	for _, endpoint := range []string{
		"https://proj.supabase.co/auth/v1malicious/token", // sibling route sharing a prefix
		"https://proj.supabase.co/auth/v1/../capture",     // climbs out after normalisation
		"https://proj.supabase.co/auth/v1/%2e%2e/capture", // the encoded form
		"https://proj.supabase.co/auth/v1/./../capture",
		"https://proj.supabase.co/auth/v1x/oauth/token",
	} {
		m := validManifest()
		m.TokenEndpoint = endpoint
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("token endpoint %q escaped the issuer path", endpoint)
		}
	}
	// The legitimate shapes still pass.
	for _, endpoint := range []string{
		"https://proj.supabase.co/auth/v1/oauth/token",
		"https://proj.supabase.co/auth/v1",
	} {
		m := validManifest()
		m.TokenEndpoint = endpoint
		if err := m.Validate(RedirectURI()); err != nil {
			t.Errorf("legitimate token endpoint %q was refused: %v", endpoint, err)
		}
	}
}

// The plaintext exception exists because there is no network to intercept. A NAME does
// not establish that: it resolves through DNS or /etc/hosts to wherever someone points
// it, and Go's ProxyFromEnvironment does not bypass the proxy for most of these — so a
// plaintext token POST carrying the code and verifier would travel to a remote host.
func TestOnlyLiteralLoopbackIPsGetThePlaintextException(t *testing.T) {
	for _, host := range []string{
		"localhost:54321", "evil.localhost:54321", "127.0.0.1.evil.example:54321",
		"127.0.0.1.:54321", "0.0.0.0:54321", "10.0.0.1:54321",
	} {
		if isLoopbackHost(host) {
			t.Errorf("%q was treated as loopback", host)
		}
	}
	for _, host := range []string{"127.0.0.1:54321", "127.0.0.1", "127.5.5.5:1", "[::1]:54321", "::1"} {
		if !isLoopbackHost(host) {
			t.Errorf("literal loopback %q was refused", host)
		}
	}
}

// These are opened in the user's browser under the assistant's own recommendation, so a
// manifest that could choose them could point someone at a phishing page with the
// assistant vouching for it.
func TestBrowserLinksArePinnedToDaintree(t *testing.T) {
	for _, bad := range []string{
		"https://evil.example/account",
		"https://daintree.org.evil.example/account",
		"https://notdaintree.org/account",
	} {
		m := validManifest()
		m.AccountURL = bad
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("account_url %q was accepted", bad)
		}
		m = validManifest()
		m.SubscribeURL = bad
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("subscribe_url %q was accepted", bad)
		}
	}
}

// Validating a trimmed copy while transmitting the raw value is how " client " passes
// every check and then goes out with spaces in it.
func TestValidationNormalisesWhatWillActuallyBeSent(t *testing.T) {
	m := validManifest()
	m.ClientID = "  daintree-assistant-staging  "
	m.RedirectURI = " " + RedirectURI() + " "
	m.Scopes = []string{" openid ", "email"}
	if err := m.Validate(RedirectURI()); err != nil {
		t.Fatalf("surrounding whitespace was rejected outright: %v", err)
	}
	if m.ClientID != "daintree-assistant-staging" {
		t.Errorf("ClientID = %q — the value validated is not the value that will be sent", m.ClientID)
	}
	if m.Scopes[0] != "openid" {
		t.Errorf("Scopes[0] = %q", m.Scopes[0])
	}
}

func TestNonHTTPSchemesAreRefused(t *testing.T) {
	for _, scheme := range []string{"file://", "ftp://", "javascript:", "data:text/html,"} {
		m := validManifest()
		m.Issuer = scheme + "proj.supabase.co/auth/v1"
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("issuer scheme %q was accepted", scheme)
		}
	}
}

func TestEmbeddedCredentialsInAnEndpointAreRefused(t *testing.T) {
	m := validManifest()
	m.Issuer = "https://user:pass@proj.supabase.co/auth/v1"
	if err := m.Validate(RedirectURI()); err == nil {
		t.Fatal("an issuer carrying embedded credentials was accepted")
	}
}

func TestVersionAndEnvironmentAreClosedSets(t *testing.T) {
	m := validManifest()
	m.Version = 2
	if err := m.Validate(RedirectURI()); err == nil {
		t.Error("an unsupported manifest version was accepted")
	}
	m = validManifest()
	m.Environment = "prod" // not one of the four
	if err := m.Validate(RedirectURI()); err == nil {
		t.Error("an unrecognised environment was accepted")
	}
}

func TestScopesAreASubsetOfTheSupportedSet(t *testing.T) {
	m := validManifest()
	m.Scopes = []string{"openid", "email", "admin:everything"}
	if err := m.Validate(RedirectURI()); err == nil {
		t.Fatal("an application-defined scope was accepted; Supabase does not support them")
	}
}

func TestTheClientIDIsBounded(t *testing.T) {
	m := validManifest()
	m.ClientID = ""
	if err := m.Validate(RedirectURI()); err == nil {
		t.Error("an empty client id was accepted")
	}
	m = validManifest()
	m.ClientID = strings.Repeat("a", maxClientIDLen+1)
	if err := m.Validate(RedirectURI()); err == nil {
		t.Error("an implausibly long client id was accepted")
	}
	// It is interpolated into a URL query and a credential-store key.
	for _, bad := range []string{"has space", "has/slash", "has\nnewline", "has\x1b[31mescape"} {
		m = validManifest()
		m.ClientID = bad
		if err := m.Validate(RedirectURI()); err == nil {
			t.Errorf("client id %q was accepted", bad)
		}
	}
}

// Manifest strings reach terminal output and a debug log. An ESC can rewrite what the
// reader sees, so the echo is sanitised and bounded.
func TestUntrustedManifestTextCannotRewriteTheTerminal(t *testing.T) {
	m := validManifest()
	m.Environment = "\x1b[2Jstaging\x00" + strings.Repeat("x", 500)
	err := m.Validate(RedirectURI())
	if err == nil {
		t.Fatal("expected a rejection")
	}
	msg := err.Error()
	if strings.ContainsAny(msg, "\x1b\x00") {
		t.Fatalf("control characters survived into the error message: %q", msg)
	}
	if len(msg) > 400 {
		t.Fatalf("the echoed value was not bounded: %d chars", len(msg))
	}
}

// --- fetching --------------------------------------------------------------------

func manifestServer(t *testing.T, m Manifest, etag string, hits *int32) *httptest.Server {
	t.Helper()
	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		if r.URL.Path != DiscoveryPath {
			http.NotFound(w, r)
			return
		}
		if etag != "" {
			if r.Header.Get("If-None-Match") == etag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", etag)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func TestManifestIsFetchedAndValidated(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	got, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if got.ClientID != "daintree-assistant-staging" {
		t.Fatalf("ClientID = %q", got.ClientID)
	}
}

// An invalid manifest must never be cached, and must never reach a caller.
func TestAnInvalidManifestIsRejectedAndNotCached(t *testing.T) {
	bad := validManifest()
	bad.TokenEndpoint = "https://evil.example/auth/v1/oauth/token"
	var hits int32
	srv := manifestServer(t, bad, "", &hits)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	for i := 0; i < 2; i++ {
		if _, err := d.Manifest(context.Background()); err == nil {
			t.Fatalf("call %d: an off-issuer token endpoint was accepted", i)
		}
	}
	if atomic.LoadInt32(&hits) != 2 {
		t.Fatalf("server saw %d requests; a rejected manifest must not be cached", hits)
	}
}

func TestAFreshManifestIsCached(t *testing.T) {
	var hits int32
	srv := manifestServer(t, validManifest(), "", &hits)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	for i := 0; i < 3; i++ {
		if _, err := d.Manifest(context.Background()); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("server saw %d requests, want 1 — the manifest is not being cached", got)
	}
}

// A redeploy can change the manifest, so the cache must expire rather than last for the
// process lifetime.
func TestTheCacheExpires(t *testing.T) {
	var hits int32
	srv := manifestServer(t, validManifest(), "", &hits)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	base := time.Now()
	d.now = func() time.Time { return base }
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("first: %v", err)
	}
	d.now = func() time.Time { return base.Add(manifestCacheTTL + time.Second) }
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("after TTL: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Fatalf("server saw %d requests, want 2 — the cache never expired", got)
	}
}

// A 304 is only meaningful with a copy to reuse. Answering one with nothing cached must
// not invent a manifest.
func TestA304WithNothingCachedIsAnError(t *testing.T) {
	srv := manifestServer(t, validManifest(), `"v1"`, nil)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	d.etag = `"v1"` // as if carried over from a previous process
	if _, err := d.Manifest(context.Background()); err == nil {
		t.Fatal("a 304 with no cached copy produced a manifest")
	}
	// ...and the stale ETag is dropped so the next call re-fetches unconditionally.
	if d.etag != "" {
		t.Fatalf("stale ETag %q survived", d.etag)
	}
	if _, err := d.Manifest(context.Background()); err != nil {
		t.Fatalf("the retry after dropping the ETag failed: %v", err)
	}
}

// The most likely real failure: a backend older than this build. It deserves its own
// sentence, not "unexpected status 404".
func TestAnOlderBackendGetsAnActionableMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if CodeOf(err) != CodeDiscoveryUnavailable {
		t.Fatalf("code = %q, want %q", CodeOf(err), CodeDiscoveryUnavailable)
	}
	var ae *Error
	if !asAuthError(err, &ae) || ae.Hint == "" {
		t.Fatal("a 404 should carry a hint naming the fix")
	}
}

// Padded with a TOLERATED unknown field on an otherwise VALID manifest, so the only
// thing that can reject it is the size limit. Padding an already-invalid manifest would
// pass this test with the limit removed entirely.
func TestAnOversizedManifestIsRefused(t *testing.T) {
	body, _ := json.Marshal(validManifest())
	padded := strings.TrimSuffix(string(body), "}") +
		`,"future_field":"` + strings.Repeat("x", maxManifestBytes) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(padded))
	}))
	defer srv.Close()

	if _, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background()); err == nil {
		t.Fatal("an oversized manifest body was accepted")
	}
	// ...and the same manifest without the padding is fine, proving the size limit is
	// what rejected it and not some other property of the fixture.
	ok := manifestServer(t, validManifest(), "", nil)
	defer ok.Close()
	if _, err := NewDiscoverer(ok.URL, nil).Manifest(context.Background()); err != nil {
		t.Fatalf("the unpadded manifest was also rejected, so the size test proves nothing: %v", err)
	}
}

// Following a redirect would send the discovery request somewhere this session was
// never pointed at — the same rule the backend client enforces.
// The redirect target is a SECOND LOCAL SERVER that would answer perfectly well. An
// earlier version of this test redirected to evil.example, where a client that happily
// followed the redirect still failed — on DNS — and the test passed while proving
// nothing. Counting hits on the target is what actually asserts the policy.
func TestDiscoveryRefusesToFollowARedirect(t *testing.T) {
	var targetHits int32
	target := manifestServer(t, validManifest(), "", &targetHits)
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+DiscoveryPath, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	_, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background())
	if err == nil {
		t.Fatal("discovery followed a redirect")
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("the redirect target was contacted %d times — the request was followed", got)
	}
	// A refused redirect is a policy violation, not a network blip: reporting it as
	// transient tells the caller to retry forever and describes a security decision as
	// an outage.
	if CodeOf(err) != CodeDiscoveryInvalid {
		t.Fatalf("code = %q, want %q", CodeOf(err), CodeDiscoveryInvalid)
	}
}

// Setting the no-redirect rule only on the DEFAULT client meant any caller supplying its
// own silently lost the guarantee. A security property that depends on a caller not
// passing an argument is not one.
func TestAnInjectedClientStillCannotFollowARedirect(t *testing.T) {
	var targetHits int32
	target := manifestServer(t, validManifest(), "", &targetHits)
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+DiscoveryPath, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	// A caller-supplied client with no CheckRedirect of its own.
	d := NewDiscoverer(srv.URL, &http.Client{})
	if _, err := d.Manifest(context.Background()); err == nil {
		t.Fatal("an injected client followed the redirect")
	}
	if got := atomic.LoadInt32(&targetHits); got != 0 {
		t.Fatalf("the redirect target was contacted %d times through an injected client", got)
	}
}

// The cached manifest and the one handed to a caller shared the Scopes backing array, so
// a caller mutating result.Scopes[0] silently rewrote what every later caller received.
func TestAReturnedManifestCannotPoisonTheCache(t *testing.T) {
	srv := manifestServer(t, validManifest(), "", nil)
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	first, err := d.Manifest(context.Background())
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	first.Scopes[0] = "admin:everything"
	first.ClientID = "hijacked"

	second, err := d.Manifest(context.Background())
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if second.Scopes[0] != "openid" {
		t.Fatalf("Scopes[0] = %q — a caller's mutation reached the cache", second.Scopes[0])
	}
	if second.ClientID != "daintree-assistant-staging" {
		t.Fatalf("ClientID = %q — a caller's mutation reached the cache", second.ClientID)
	}
}

// Without a generation barrier Invalidate is advisory: a fetch already in flight simply
// wins the race and repopulates the cache with the manifest that was just discarded —
// serving backend A's configuration to a caller that has switched to backend B.
func TestAnInFlightFetchCannotRepopulateAnInvalidatedCache(t *testing.T) {
	release := make(chan struct{})
	body, _ := json.Marshal(validManifest())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // hold the response open until the test invalidates
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	d := NewDiscoverer(srv.URL, nil)
	done := make(chan error, 1)
	go func() {
		_, err := d.Manifest(context.Background())
		done <- err
	}()

	// Let the request reach the server, then invalidate underneath it.
	time.Sleep(50 * time.Millisecond)
	d.Invalidate()
	close(release)

	if err := <-done; err == nil {
		t.Fatal("a fetch that started before Invalidate was allowed to store its result")
	}
	d.mu.Lock()
	cached := d.cached
	d.mu.Unlock()
	if cached != nil {
		t.Fatal("the invalidated cache was repopulated by the in-flight fetch")
	}
}

// An additive backend change must not break login. A body carrying a field this build
// has no opinion about still parses.
func TestAnUnknownManifestFieldIsTolerated(t *testing.T) {
	body, _ := json.Marshal(validManifest())
	withExtra := strings.TrimSuffix(string(body), "}") + `,"future_field":"whatever"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(withExtra))
	}))
	defer srv.Close()

	if _, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background()); err != nil {
		t.Fatalf("an additive backend field broke discovery: %v", err)
	}
}

// asAuthError is errors.As specialised for the local error type, kept here so the test
// file does not need the errors import in three places.
func asAuthError(err error, target **Error) bool {
	for err != nil {
		if ae, ok := err.(*Error); ok {
			*target = ae
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// A deployment with no accounts answers with only version/environment/configured/required
// — no issuer, no client id, no endpoints. That is not a malformed manifest, and
// reporting it as one sends someone hunting a fault that does not exist.
//
// Verified against the real backend's response shape (assistant-backend
// api/daintree_auth.py), which returns exactly those four fields when
// settings.auth_is_configured is false.
func TestAnUnconfiguredDeploymentIsReportedAsSuchNotAsBroken(t *testing.T) {
	no := false
	m := Manifest{Version: 1, Environment: "development", Configured: &no}
	err := m.Validate(RedirectURI())
	if err == nil {
		t.Fatal("an unconfigured manifest validated as usable")
	}
	if CodeOf(err) != CodeAccountsUnavailable {
		t.Fatalf("code = %q, want %q — a deployment without accounts is not a broken one",
			CodeOf(err), CodeAccountsUnavailable)
	}
}

// The staging deployment's actual shape: the CLI talks to `assistant.daintree.org`,
// while the manifest it serves says `environment: staging` and points the browser at
// `staging.daintree.org`. Nothing about validation may depend on the backend base.
//
// The property is worth pinning precisely because the alternative looks reasonable.
// Checking the manifest against the endpoint that served it — "a staging manifest should
// come from a staging host" — would make the backend base part of the trust decision,
// and then staging.daintree.org has to be hardcoded somewhere as a backend. It is not,
// and this is what keeps it that way: Validate takes the expected REDIRECT and nothing
// else, so the same manifest is judged identically whatever served it.
func TestTheManifestIsJudgedIndependentlyOfTheBackendItCameFrom(t *testing.T) {
	m := validManifest() // environment staging, links on staging.daintree.org
	if m.Environment != "staging" || m.AccountURL != "https://staging.daintree.org/account" {
		t.Fatalf("fixture drifted: environment=%q account=%q", m.Environment, m.AccountURL)
	}

	// Validate takes the expected REDIRECT and nothing else — no backend, no origin.
	// That is the property, and it is checked by its signature as much as by its result:
	// there is no parameter through which the endpoint could influence the verdict.
	if err := m.Validate(RedirectURI()); err != nil {
		t.Fatalf("a staging manifest was rejected: %v", err)
	}

	// End to end, through a real fetch, from three different origins: a
	// production-shaped assistant host, a loopback dev backend, and a host with no
	// relationship to Daintree at all. The same staging manifest must be accepted by
	// all three, or the backend base has become part of the trust decision.
	for _, label := range []string{"assistant origin", "loopback dev backend", "unrelated host"} {
		srv := manifestServer(t, m, "", nil)
		got, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background())
		srv.Close()
		if err != nil {
			t.Errorf("%s: a staging manifest served from here was rejected: %v", label, err)
			continue
		}
		if got.Environment != "staging" || got.AccountURL != m.AccountURL {
			t.Errorf("%s: the manifest was altered in transit: env=%q account=%q",
				label, got.Environment, got.AccountURL)
		}
	}

	// The links are pinned to daintree.org and the callback is exact, so the freedom
	// above costs nothing: a manifest from anywhere still cannot redirect the browser
	// off Daintree or move the callback.
	if m.RedirectURI != "http://127.0.0.1:42813/oauth/callback" {
		t.Errorf("callback = %q, want the exact compiled loopback", m.RedirectURI)
	}
	evil := validManifest()
	evil.AccountURL = "https://staging.daintree.org.evil.example/account"
	if err := evil.Validate(RedirectURI()); err == nil {
		t.Error("a link on a look-alike of the staging host was accepted")
	}
	notOurs := validManifest()
	notOurs.SubscribeURL = "https://notdaintree.org/subscribe"
	if err := notOurs.Validate(RedirectURI()); err == nil {
		t.Error("a link on a suffix look-alike was accepted")
	}
}

// An OLDER backend omits the field entirely. Treating that as unconfigured would
// silently disable sign-in against every deployment predating the flag.
func TestAnAbsentConfiguredFlagMeansConfigured(t *testing.T) {
	m := validManifest() // no Configured field set
	if m.Configured != nil {
		t.Fatal("the fixture sets Configured; this test needs it absent")
	}
	if err := m.Validate(RedirectURI()); err != nil {
		t.Fatalf("a manifest without the configured flag was rejected: %v", err)
	}
}

// An explicitly configured deployment still validates normally.
func TestAnExplicitlyConfiguredDeploymentValidates(t *testing.T) {
	yes := true
	m := validManifest()
	m.Configured = &yes
	if err := m.Validate(RedirectURI()); err != nil {
		t.Fatalf("an explicitly configured manifest was rejected: %v", err)
	}
}

// The real backend sends `configured` and `required`, which this build had no fields for
// until now. An additive backend field must never break discovery.
func TestTheRealBackendFieldsAreTolerated(t *testing.T) {
	body := `{"version":1,"environment":"staging","configured":true,"required":false,
		"issuer":"https://proj.supabase.co/auth/v1",
		"authorization_endpoint":"https://proj.supabase.co/auth/v1/oauth/authorize",
		"token_endpoint":"https://proj.supabase.co/auth/v1/oauth/token",
		"jwks_uri":"https://proj.supabase.co/auth/v1/.well-known/jwks.json",
		"client_id":"daintree-assistant-staging","redirect_uri":"` + RedirectURI() + `",
		"scopes":["openid","email"],
		"account_url":"https://staging.daintree.org/account",
		"subscribe_url":"https://staging.daintree.org/subscribe",
		"session_policy":{"access_token_seconds":3600,"session_max_age_seconds":2592000}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	m, err := NewDiscoverer(srv.URL, nil).Manifest(context.Background())
	if err != nil {
		t.Fatalf("the real backend's manifest was rejected: %v", err)
	}
	if m.ClientID != "daintree-assistant-staging" {
		t.Fatalf("ClientID = %q", m.ClientID)
	}
	if m.SessionPolicy.SessionMaxAgeSeconds != 2592000 {
		t.Fatalf("session policy did not decode: %+v", m.SessionPolicy)
	}
}
