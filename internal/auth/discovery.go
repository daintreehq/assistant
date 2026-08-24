package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// discovery.go fetches and validates the backend's non-secret auth manifest.
//
// The manifest exists so the CLI does not hardcode a Supabase project, an OAuth client
// id, an account URL or a callback. Those differ per environment and drift; a compiled
// copy would silently disagree with the backend after any redeployment.
//
// The trade is that a value the CLI trusts now arrives over the wire, so Validate is
// the boundary. Pointing at a backend is already a trust decision — but not this one:
// what is at stake here is not this conversation, it is the user's credential at a THIRD
// party, and a backend should not be able to reach that.
//
// The checks fall into two kinds, and the distinction matters because only one of them
// is a real defence:
//
//   - RELATIVE checks (same origin as the issuer, under the issuer's path). These stop
//     an endpoint being MOVED off an otherwise-correct issuer. They are worth having and
//     they catch real misconfiguration — but on their own they prove nothing, because a
//     hostile manifest simply names its own issuer and passes every one of them.
//   - The ANCHOR check (issuerHostAllowed). This is what actually binds the manifest to
//     a provider we chose. Without it the whole exercise is circular: a document cannot
//     establish its own trust anchor.
//
// The attack the anchor closes is concrete. A manifest naming issuer, authorize, token
// and JWKS all on relay.example is internally consistent. Its authorize endpoint
// redirects the browser to the REAL Supabase, preserving our client id, redirect URI,
// state and PKCE challenge; the real authorization code comes back to our loopback
// listener exactly as expected; we then post that code AND the verifier to the relay's
// token endpoint, and the relay redeems both upstream. Nothing looks wrong at any step.
// Pinning the issuer host is what stops it.

// DiscoveryPath is the public manifest endpoint. It is in backend.publicPaths, so
// fetching it never requires (or triggers a fetch of) a credential — necessarily, since
// this is how a credential is obtained.
const DiscoveryPath = "/v1/daintree/auth/config"

// SupportedManifestVersion is the only manifest shape this build understands.
const SupportedManifestVersion = 1

// manifestTimeout bounds the fetch. Short on purpose: this runs before an interactive
// login, and a user staring at a terminal is the wrong place to spend a 60-second
// default.
const manifestTimeout = 10 * time.Second

// maxManifestBytes bounds the response body. The document is a few hundred bytes; a
// backend that answers with megabytes is broken or hostile, and either way should not
// be allowed to sit in memory.
const maxManifestBytes = 64 << 10

// manifestCacheTTL is how long a validated manifest is reused. It is non-secret and
// nearly static, so re-fetching per operation is waste — but it CAN change on a
// redeploy, so it must not be cached for the process lifetime.
const manifestCacheTTL = 5 * time.Minute

// allowedScopes is the closed set this client will request. Supabase supports standard
// OAuth scopes but not application-defined ones, so there is nothing to widen this to;
// a manifest naming anything else is describing a different provider.
var allowedScopes = map[string]bool{"openid": true, "email": true, "profile": true}

// allowedEnvironments is the closed set of deployment identities.
var allowedEnvironments = map[string]bool{
	"development": true, "test": true, "staging": true, "production": true,
}

// allowedIssuerSuffixes pins the OAuth trust anchor.
//
// Hardcoding a domain here is deliberate, and it is NOT the same mistake as hardcoding
// a provider's URL path would be. A path is a layout detail that changes without
// meaning anything; the issuer domain IS the trust decision, and pinning a trust anchor
// while discovering everything under it is the ordinary shape — it is what certificate
// pinning does. Discovery still supplies the project, the client id, the endpoints and
// the links; it just cannot choose which company issues the user's tokens.
//
// A host matches when it equals a suffix or is a subdomain of it. Add an entry here if
// the identity provider changes; a manifest naming anything else is refused outright,
// which is the intended failure mode.
var allowedIssuerSuffixes = []string{
	"supabase.co",  // Supabase-hosted projects
	"daintree.org", // a self-hosted or aliased Supabase under Daintree's own domain
}

// allowedLinkSuffixes pins where the manifest may send the user's BROWSER.
//
// Separate from the issuer anchor because these are ordinary web pages rather than a
// token endpoint, but pinned for the same reason: without it a manifest can point
// "manage your account" and "buy a plan" at a phishing site, and the user has every
// reason to trust a link the assistant opened for them.
var allowedLinkSuffixes = []string{"daintree.org"}

// hostAllowed reports whether host equals one of the suffixes or is a subdomain of one.
//
// Subdomain matching is on a LABEL boundary, so "notdaintree.org" and
// "daintree.org.evil.example" are both refused — the substring version of this check is
// the classic way a domain allowlist gets bypassed.
func hostAllowed(host string, suffixes []string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	h = strings.ToLower(strings.TrimSuffix(strings.Trim(h, "[]"), "."))
	for _, suffix := range suffixes {
		if h == suffix || strings.HasSuffix(h, "."+suffix) {
			return true
		}
	}
	return false
}

// errRedirected is the sentinel CheckRedirect returns, so the fetch path can tell a
// policy refusal apart from an ordinary transport failure.
var errRedirected = errors.New("auth: the sign-in configuration endpoint must not redirect")

// SessionPolicy is the manifest's declaration of token and session lifetimes. It is
// advisory: the CLI schedules a proactive refresh from the access token's ACTUAL expiry,
// never from this. It is carried so status can tell the user how long a login lasts.
type SessionPolicy struct {
	AccessTokenSeconds   int `json:"access_token_seconds"`
	SessionMaxAgeSeconds int `json:"session_max_age_seconds"`
}

// Manifest is the backend's non-secret OAuth environment description.
type Manifest struct {
	Version     int    `json:"version"`
	Environment string `json:"environment"`
	// Configured reports whether this deployment has accounts set up AT ALL.
	//
	// A backend with no identity provider configured answers with only version,
	// environment, configured and required — no issuer, no client id, no endpoints.
	// Without this field that body fails validation as "the backend named no issuer",
	// which is both wrong and unhelpful: nothing is malformed, the deployment simply does
	// not offer accounts, and the honest answer is to say so and carry on anonymously.
	//
	// It is a POINTER so an older backend that omits the field is treated as configured,
	// which is what it is — a bare false would silently disable sign-in against every
	// deployment predating the flag.
	Configured *bool `json:"configured,omitempty"`
	// Required reports whether this deployment refuses anonymous requests. Advisory: the
	// backend answers 401 either way, and that answer is the authority.
	Required              bool          `json:"required,omitempty"`
	Issuer                string        `json:"issuer"`
	AuthorizationEndpoint string        `json:"authorization_endpoint"`
	TokenEndpoint         string        `json:"token_endpoint"`
	JWKSURI               string        `json:"jwks_uri"`
	ClientID              string        `json:"client_id"`
	RedirectURI           string        `json:"redirect_uri"`
	Scopes                []string      `json:"scopes"`
	AccountURL            string        `json:"account_url"`
	SubscribeURL          string        `json:"subscribe_url"`
	SessionPolicy         SessionPolicy `json:"session_policy"`
}

// maxClientIDLen bounds the client id. It goes into a URL query string and into a
// credential-store account key; an unbounded value from the wire has no business in
// either.
const maxClientIDLen = 200

// Validate checks a manifest before any of it is used to build a request.
//
// Each rule below closes a specific way a bad manifest could do harm, and the ordering
// is deliberate: identity first (is this shape even ours?), then the exfiltration
// checks, then the cosmetic ones.
func (m *Manifest) Validate(expectedRedirect string) error {
	if m == nil {
		return newError(CodeDiscoveryInvalid, "the backend returned no auth configuration")
	}
	if m.Version != SupportedManifestVersion {
		return newError(CodeDiscoveryInvalid, fmt.Sprintf(
			"this backend describes auth configuration version %d; this build understands version %d",
			m.Version, SupportedManifestVersion)).
			withHint("Update the assistant, or point it at a backend matching this build.")
	}
	if !allowedEnvironments[m.Environment] {
		return newError(CodeDiscoveryInvalid, fmt.Sprintf("unrecognised environment %q", safeEcho(m.Environment)))
	}
	// A deployment with no accounts is not a malformed one. Checked here — after the
	// shape checks that apply to every body, before the ones that need an issuer.
	if m.Configured != nil && !*m.Configured {
		return newError(CodeAccountsUnavailable, "this backend does not offer account sign-in").
			withHint("It serves requests without an account. Nothing needs to be done.")
	}

	// The issuer is the root of trust: every other endpoint is checked against it, so
	// it is validated first and hardest.
	issuer, err := parseAuthURL(m.Issuer, "issuer")
	if err != nil {
		return err
	}
	// THE anchor check. Everything below is relative to the issuer and therefore proves
	// nothing on its own; this is the one rule a self-consistent hostile manifest cannot
	// satisfy. Loopback is exempt because a local development provider is, by
	// construction, one the developer is already running.
	if !isLoopbackHost(issuer.Host) && !hostAllowed(issuer.Host, allowedIssuerSuffixes) {
		return newError(CodeDiscoveryInvalid, fmt.Sprintf(
			"the backend names %s as the sign-in issuer, which is not a Daintree identity provider",
			safeEcho(issuer.Host))).
			withHint("This build only signs in against Daintree's own identity provider. Check DAINTREE_BACKEND_URL.")
	}

	// The exfiltration checks. An endpoint that shares neither the issuer's origin nor
	// its path prefix is a different server wearing the issuer's name — and the token
	// endpoint in particular receives the authorization code AND the PKCE verifier, so
	// a manifest that could move it anywhere could harvest accounts.
	//
	// Note this is deliberately NOT the "must start with /auth/v1" rule the guide
	// describes. That hardcodes one provider's URL layout into the CLI and breaks the
	// day it changes, while checking strictly less: `/auth/v1` on a different HOST
	// would pass it. Requiring the issuer's own origin and path prefix is both stronger
	// and provider-agnostic.
	for _, ep := range []struct{ raw, name string }{
		{m.AuthorizationEndpoint, "authorization_endpoint"},
		{m.TokenEndpoint, "token_endpoint"},
		{m.JWKSURI, "jwks_uri"},
	} {
		u, err := parseAuthURL(ep.raw, ep.name)
		if err != nil {
			return err
		}
		if u.Scheme != issuer.Scheme || u.Host != issuer.Host {
			return newError(CodeDiscoveryInvalid, fmt.Sprintf(
				"%s is on %s, which is not the issuer's origin %s",
				ep.name, safeEcho(u.Scheme+"://"+u.Host), safeEcho(issuer.Scheme+"://"+issuer.Host)))
		}
		if !underPath(u, issuer) {
			return newError(CodeDiscoveryInvalid, fmt.Sprintf(
				"%s is not under the issuer path %s", ep.name, safeEcho(path(issuer))))
		}
	}

	// Trimmed values are written BACK onto the manifest, so the value validated is the
	// value later sent. Validating a trimmed copy while transmitting the raw one is how
	// " valid-client " passes every check and then goes out with spaces in it.
	m.ClientID = strings.TrimSpace(m.ClientID)
	m.Issuer = strings.TrimSpace(m.Issuer)
	m.AuthorizationEndpoint = strings.TrimSpace(m.AuthorizationEndpoint)
	m.TokenEndpoint = strings.TrimSpace(m.TokenEndpoint)
	m.JWKSURI = strings.TrimSpace(m.JWKSURI)
	m.RedirectURI = strings.TrimSpace(m.RedirectURI)
	m.AccountURL = strings.TrimSpace(m.AccountURL)
	m.SubscribeURL = strings.TrimSpace(m.SubscribeURL)
	for i := range m.Scopes {
		m.Scopes[i] = strings.TrimSpace(m.Scopes[i])
	}

	id := m.ClientID
	if id == "" {
		return newError(CodeDiscoveryInvalid, "the backend named no OAuth client id")
	}
	if len(id) > maxClientIDLen {
		return newError(CodeDiscoveryInvalid, fmt.Sprintf("the OAuth client id is %d characters, which is not a plausible identifier", len(id)))
	}
	if !isClientIDCharset(id) {
		return newError(CodeDiscoveryInvalid, "the OAuth client id contains characters that are not valid in one")
	}

	// The redirect must be OUR compiled loopback callback, exactly. This is the single
	// most important check in the file: it is what guarantees the authorization code
	// comes back to this process on this machine, and not to a URL the manifest chose.
	if m.RedirectURI != expectedRedirect {
		return newError(CodeDiscoveryInvalid, fmt.Sprintf(
			"the backend expects the sign-in callback at %s; this build only accepts %s",
			safeEcho(m.RedirectURI), expectedRedirect)).
			withHint("The backend and this build disagree about the callback. Update the assistant, or point it at a matching backend.")
	}

	for _, s := range m.Scopes {
		if !allowedScopes[s] {
			return newError(CodeDiscoveryInvalid, fmt.Sprintf("unsupported OAuth scope %q", safeEcho(s)))
		}
	}

	// Account and subscribe links are opened in the user's browser, so they get the
	// same scheme treatment as everything else — a manifest must not be able to hand
	// the browser a file:// or a custom-scheme URL.
	for _, l := range []struct{ raw, name string }{
		{m.AccountURL, "account_url"},
		{m.SubscribeURL, "subscribe_url"},
	} {
		if l.raw == "" {
			continue // optional
		}
		u, err := parseAuthURL(l.raw, l.name)
		if err != nil {
			return err
		}
		// Pinned for the same reason the issuer is. These are opened in the user's
		// browser under the assistant's own recommendation — "manage your account", "buy
		// a plan" — so a manifest that could choose them could point someone at a
		// convincing phishing page with the assistant vouching for it.
		if !isLoopbackHost(u.Host) && !hostAllowed(u.Host, allowedLinkSuffixes) {
			return newError(CodeDiscoveryInvalid, fmt.Sprintf(
				"%s points at %s, which is not a Daintree site", l.name, safeEcho(u.Host)))
		}
	}
	return nil
}

// path returns a URL's path with a guaranteed leading slash, so containment comparison
// cannot be fooled by an empty path. u.Path is already percent-DECODED by url.Parse,
// which is what lets underPath see a "%2e%2e" as the ".." it really is.
func path(u *url.URL) string {
	if u.Path == "" {
		return "/"
	}
	return u.Path
}

// underPath reports whether child's path is contained by parent's, on a SEGMENT
// boundary and with no way to climb back out.
//
// A bare strings.HasPrefix is not containment, and both ways it fails are exploitable:
//
//   - "/auth/v1malicious/token" has "/auth/v1" as a prefix but is a different route,
//     which a sibling handler on the same host can own.
//   - "/auth/v1/%2e%2e/capture" decodes to "/auth/v1/../capture". It passes a prefix
//     test, and any proxy or server that normalises dot segments — most do — resolves
//     it to "/capture", entirely outside the issuer.
//
// So dot segments are refused outright rather than normalised. Normalising would mean
// deciding whether OUR interpretation matches the far end's, and a legitimate OAuth
// endpoint never contains one.
func underPath(child, parent *url.URL) bool {
	cp, pp := path(child), path(parent)
	for _, seg := range strings.Split(cp, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	// A raw "%2f" would let a single decoded segment smuggle a separator past the
	// boundary check below, since url.Path has already collapsed it into the path.
	if strings.Contains(strings.ToLower(child.EscapedPath()), "%2f") {
		return false
	}
	pp = strings.TrimSuffix(pp, "/")
	if pp == "" {
		return true // the issuer is at the root; everything on it is under it
	}
	return cp == pp || strings.HasPrefix(cp, pp+"/")
}

// parseAuthURL parses and scheme-checks one manifest URL.
//
// HTTPS is required for anything reachable over a network. Plaintext loopback is
// permitted, and that is a deliberate DEVIATION from the guide's blanket "must be
// HTTPS": it matches this repo's existing posture (see backend.ValidatePlaintextRemote,
// which permits loopback unconditionally because there is no network to intercept), and
// without it a local Supabase — which serves http://127.0.0.1:54321 — makes local
// development of this whole feature impossible while adding no security whatever.
func parseAuthURL(raw, name string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, newError(CodeDiscoveryInvalid, "the backend named no "+name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, newError(CodeDiscoveryInvalid, name+" is not a valid URL")
	}
	if u.Host == "" {
		return nil, newError(CodeDiscoveryInvalid, name+" has no host")
	}
	// Credentials embedded in the URL would be sent on every request and rendered into
	// any error text that echoed the URL back.
	if u.User != nil {
		return nil, newError(CodeDiscoveryInvalid, name+" carries embedded credentials")
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		if isLoopbackHost(u.Host) {
			return u, nil
		}
		return nil, newError(CodeDiscoveryInvalid, fmt.Sprintf(
			"%s uses plain http on a remote host (%s); an OAuth exchange must not cross the network unencrypted",
			name, safeEcho(u.Host)))
	default:
		return nil, newError(CodeDiscoveryInvalid, fmt.Sprintf("%s uses the %q scheme; only https is accepted", name, safeEcho(u.Scheme)))
	}
}

// isLoopbackHost reports whether a URL host is a LITERAL loopback IP address.
//
// Names are deliberately refused — including "localhost" itself, and anything ending
// ".localhost". The plaintext exception exists because there is no network to
// intercept, and a NAME does not establish that: "evil.localhost" resolves through DNS
// or /etc/hosts to wherever someone points it, and Go's own ProxyFromEnvironment
// bypasses the proxy only for exact "localhost" and for parsed loopback IP literals. So
// a name-based exception would let a plaintext token POST — carrying the authorization
// code AND the PKCE verifier — travel to a remote host through HTTP_PROXY, which is the
// precise outcome the HTTPS requirement exists to prevent.
//
// Nothing is lost: a local Supabase serves 127.0.0.1:54321, which this accepts.
func isLoopbackHost(host string) bool {
	h := host
	if hostOnly, _, err := net.SplitHostPort(host); err == nil {
		h = hostOnly
	}
	// The trailing dot is deliberately NOT stripped. "127.0.0.1." is not IP-literal
	// syntax — it is a fully-qualified DNS name that merely looks like one, and it is
	// resolved as a name. Stripping it would readmit exactly the class this function was
	// narrowed to exclude, and Go's own ProxyFromEnvironment does not bypass it either.
	h = strings.Trim(strings.TrimSpace(h), "[]")
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// isClientIDCharset reports whether every rune is one an OAuth client id plausibly
// uses. Bounded rather than permissive because this value is interpolated into a URL
// query string and into a credential-store account key.
func isClientIDCharset(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == '~':
		default:
			return false
		}
	}
	return true
}

// maxEchoLen bounds how much backend-supplied text appears in an error message.
const maxEchoLen = 120

// safeEcho renders untrusted text for a human without letting it take over the line.
//
// The manifest is data from a server, and its strings end up in terminal output and a
// debug log. Control characters — an ESC in particular — can rewrite what the reader
// sees, and an unbounded string can bury the actual message.
func safeEcho(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i >= maxEchoLen {
			b.WriteString("…")
			break
		}
		if r < 0x20 || r == 0x7f {
			b.WriteRune('?')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Discoverer fetches and caches the auth manifest for one backend origin.
//
// It holds its OWN http.Client rather than reusing the backend client, for two
// reasons. The backend client has no whole-request timeout (a streamed turn runs for
// minutes) and its retry policy settles into a 10-15 second poll — both correct there
// and both wrong for a pre-login fetch a user is waiting on. Separately, it keeps
// discovery reachable without any credential plumbing at all.
type Discoverer struct {
	baseURL string
	http    *http.Client

	mu        sync.Mutex
	cached    *Manifest
	etag      string
	fetchedAt time.Time
	// generation rises on every Invalidate. A fetch samples it before releasing the
	// lock and refuses to store its result if it has moved, so a request that was
	// already in flight when the caller invalidated cannot repopulate the cache
	// afterwards. Without it Invalidate is advisory: the stale fetch simply wins the
	// race and the next caller gets the manifest we just discarded.
	generation uint64
	now        func() time.Time
}

// NewDiscoverer builds a Discoverer for a backend base URL. A nil httpClient gets a
// bounded default.
func NewDiscoverer(baseURL string, httpClient *http.Client) *Discoverer {
	// The no-redirect rule is applied to EVERY client, injected ones included, by
	// copying the caller's client rather than mutating it. Setting it only on the
	// default meant a caller that supplied its own — a test, a future caller wanting a
	// custom transport — silently lost the guarantee and would follow a 307 to another
	// server, then cache that server's manifest as if the configured backend had served
	// it. A security property that depends on a caller not passing an argument is not
	// one.
	c := &http.Client{Timeout: manifestTimeout}
	if httpClient != nil {
		copied := *httpClient
		c = &copied
		if c.Timeout <= 0 {
			c.Timeout = manifestTimeout
		}
	}
	// Same rule as the backend client: this endpoint does not redirect, and following
	// one would send the request somewhere this session was never pointed at.
	c.CheckRedirect = func(*http.Request, []*http.Request) error {
		return errRedirected
	}
	httpClient = c
	return &Discoverer{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		http:    httpClient,
		now:     time.Now,
	}
}

// Manifest returns a validated manifest, using the cached copy while it is fresh.
//
// A cached manifest is only ever a VALIDATED one: Validate runs before anything is
// stored, so no caller can receive a manifest that failed a check.
func (d *Discoverer) Manifest(ctx context.Context) (*Manifest, error) {
	d.mu.Lock()
	if d.cached != nil && d.now().Sub(d.fetchedAt) < manifestCacheTTL {
		m := d.cached.clone()
		d.mu.Unlock()
		return m, nil
	}
	etag := d.etag
	gen := d.generation
	d.mu.Unlock()

	m, newETag, notModified, err := d.fetch(ctx, etag)
	if err != nil {
		return nil, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	// The endpoint may have changed under this fetch. Storing now would serve backend
	// A's manifest to a caller that has since switched to backend B.
	if d.generation != gen {
		return nil, newError(CodeDiscoveryUnavailable, "the sign-in configuration changed while it was being fetched")
	}
	if notModified {
		if d.cached == nil {
			// A 304 with nothing cached means the ETag we sent came from a previous
			// process or a cleared cache. Treat it as unusable rather than inventing a
			// manifest; the next call re-fetches unconditionally.
			d.etag = ""
			return nil, newError(CodeDiscoveryUnavailable, "the backend reported the auth configuration unchanged, but this process has no copy of it")
		}
		d.fetchedAt = d.now()
		return d.cached.clone(), nil
	}
	// Validate BEFORE storing, so nothing that failed a check can ever be cached or
	// returned. It also normalises whitespace onto m, so the stored copy is canonical.
	if err := m.Validate(RedirectURI()); err != nil {
		return nil, err
	}
	d.cached = m.clone()
	d.etag = newETag
	d.fetchedAt = d.now()
	return m.clone(), nil
}

// clone returns a deep copy.
//
// A shallow copy shares the Scopes backing array with the cached manifest, so a caller
// mutating result.Scopes[0] silently rewrites what every later caller receives — and
// races any concurrent reader, which is the kind of bug that surfaces weeks later as an
// inexplicable scope rejection.
func (m *Manifest) clone() *Manifest {
	if m == nil {
		return nil
	}
	out := *m
	if m.Scopes != nil {
		out.Scopes = append([]string(nil), m.Scopes...)
	}
	return &out
}

// fetch performs one conditional GET.
func (d *Discoverer) fetch(ctx context.Context, etag string) (*Manifest, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+DiscoveryPath, nil)
	if err != nil {
		return nil, "", false, wrapError(CodeDiscoveryUnavailable, "could not build the auth configuration request", err)
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := d.http.Do(req)
	if err != nil {
		// A refused redirect is a POLICY violation, not a network blip. Folding it into
		// the transient bucket would tell the caller to retry — and a retry re-derives
		// the same refusal forever, while describing a security decision as an outage.
		if errors.Is(err, errRedirected) {
			return nil, "", false, newError(CodeDiscoveryInvalid,
				"the sign-in configuration endpoint redirected; it must not").
				withHint("A proxy or a misconfigured deployment is intercepting the endpoint.")
		}
		return nil, "", false, wrapError(CodeDiscoveryUnavailable,
			"could not reach the assistant backend for its sign-in configuration", err).
			withHint("Check your network, then try again.")
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if resp.StatusCode == http.StatusNotFound {
		// The single most likely real-world failure: a backend older than this build.
		// Worth its own sentence rather than "unexpected status 404".
		return nil, "", false, newError(CodeDiscoveryUnavailable, "this backend does not serve sign-in configuration").
			withHint("The endpoint predates account sign-in. Point DAINTREE_BACKEND_URL at a current backend.")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", false, newError(CodeDiscoveryUnavailable,
			fmt.Sprintf("the backend answered %d for its sign-in configuration", resp.StatusCode))
	}

	// LimitReader at max+1 so an oversized body is DETECTED rather than silently
	// truncated into something that might still parse.
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes+1))
	if err != nil {
		return nil, "", false, wrapError(CodeDiscoveryUnavailable, "could not read the sign-in configuration", err)
	}
	if len(raw) > maxManifestBytes {
		return nil, "", false, newError(CodeDiscoveryInvalid, "the sign-in configuration is implausibly large")
	}

	var m Manifest
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		// Unknown fields are tolerated on a re-parse: a backend may add a field this
		// build has no opinion about, and refusing to log in over it would make every
		// additive change a breaking one. A body that fails BOTH parses is malformed.
		if err2 := json.Unmarshal(raw, &m); err2 != nil {
			return nil, "", false, wrapError(CodeDiscoveryInvalid, "the sign-in configuration is not valid JSON", err2)
		}
	}
	return &m, strings.TrimSpace(resp.Header.Get("ETag")), false, nil
}

// Invalidate drops the cached manifest, forcing the next call to re-fetch. Used when a
// backend switch means the cached copy describes a different deployment.
func (d *Discoverer) Invalidate() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.cached, d.etag, d.fetchedAt = nil, "", time.Time{}
	d.generation++
}
