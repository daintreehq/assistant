package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// token_client.go exchanges an authorization code for tokens, and refreshes them.
//
// It talks DIRECTLY to the identity provider's token endpoint, not through the Daintree
// backend. That is deliberate: the backend never sees the refresh token, never sees the
// authorization code, and never sees the PKCE verifier, so a compromised backend cannot
// mint credentials for an account. The backend's only role in authentication is telling
// us WHERE the provider is (the manifest), and that answer is bounded by the issuer
// anchor in discovery.go.
//
// No client secret is used or shipped. This is a public client; PKCE is what binds the
// code to this process.

// tokenRequestTimeout bounds one token call. Short because a user is waiting: during
// login they are staring at a terminal, and during a refresh a turn is blocked behind it.
const tokenRequestTimeout = 30 * time.Second

// maxTokenResponseBytes bounds the token response body.
const maxTokenResponseBytes = 64 << 10

// TokenSet is one successful token response.
//
// AccessToken and RefreshToken are live credentials: neither is ever logged, printed, or
// put in a structured event. ExpiresAt is derived here rather than trusted from a claim,
// so the refresh schedule does not depend on the token being parseable.
type TokenSet struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	TokenType    string
}

// NeedsRefresh reports whether the access token should be refreshed proactively.
//
// Refreshing early rather than on failure is the difference between an invisible
// rotation and a user-visible error: a token that expires mid-turn produces a 401 the
// retry ladder has to unwind, and on a streaming turn there may be no safe way to replay.
func (t TokenSet) NeedsRefresh(now time.Time) bool {
	if t.AccessToken == "" {
		return true
	}
	if t.ExpiresAt.IsZero() {
		return false
	}
	return now.Add(refreshLeadTime).After(t.ExpiresAt)
}

const (
	// refreshLeadTime is how far ahead of expiry a proactive refresh happens.
	refreshLeadTime = 5 * time.Minute
	// minTokenLifetime rejects an expires_in this client cannot work with.
	//
	// It must EXCEED refreshLeadTime, and that is the whole point of the value: a token
	// issued with less life than the proactive-refresh window is born already needing a
	// refresh, so every single request would rotate a one-time-use token. The previous
	// value here was 30 seconds — under the lead time — which accepted exactly the
	// tokens that produce that loop while the comment claimed otherwise.
	minTokenLifetime = refreshLeadTime + time.Minute
	// maxTokenLifetime rejects an implausible one. Supabase issues one-hour access
	// tokens; a claim of a year is either a misconfiguration or a hostile response, and
	// believing it would mean never refreshing and never noticing a revocation.
	maxTokenLifetime = 24 * time.Hour
)

// tokenClient performs the two token-endpoint calls.
type tokenClient struct {
	http *http.Client
	now  func() time.Time
}

// newTokenClient builds a bounded, redirect-refusing token client.
func newTokenClient(httpClient *http.Client) *tokenClient {
	c := &http.Client{Timeout: tokenRequestTimeout}
	if httpClient != nil {
		copied := *httpClient
		c = &copied
		if c.Timeout <= 0 {
			c.Timeout = tokenRequestTimeout
		}
	}
	// The same rule the backend client and discovery both enforce, and it matters most
	// here: this request body carries the authorization code and the PKCE verifier, and
	// a 307 replays the POST body at the new location. Following one would hand both to
	// whatever answered.
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return errRedirected }
	return &tokenClient{http: c, now: time.Now}
}

// tokenResponse is the provider's wire shape.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	// Error fields, present on a failure.
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

// Exchange trades an authorization code for a token set.
func (c *tokenClient) Exchange(ctx context.Context, m *Manifest, code, verifier string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {m.ClientID},
		"redirect_uri":  {RedirectURI()},
		"code_verifier": {verifier},
	}
	return c.post(ctx, m.TokenEndpoint, form, CodeExchangeFailed)
}

// Refresh trades a refresh token for a new token set.
//
// The response may carry a NEW refresh token; Supabase rotates them and treats reuse as
// a signal of theft. The caller must persist whatever comes back before using the access
// token, which is why this returns the whole set rather than just the access token.
func (c *tokenClient) Refresh(ctx context.Context, m *Manifest, refreshToken string) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {m.ClientID},
	}
	return c.post(ctx, m.TokenEndpoint, form, CodeRefreshFailed)
}

// post performs one form-encoded token request.
func (c *tokenClient) post(ctx context.Context, endpoint string, form url.Values, failCode string) (TokenSet, error) {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, wrapError(failCode, "could not build the token request", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return TokenSet{}, wrapError(failCode, "could not reach the identity provider", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponseBytes+1))
	if err != nil {
		return TokenSet{}, wrapError(failCode, "could not read the identity provider's response", err)
	}
	if len(raw) > maxTokenResponseBytes {
		return TokenSet{}, newError(failCode, "the identity provider's response was implausibly large")
	}

	var tr tokenResponse
	if err := json.Unmarshal(raw, &tr); err != nil {
		// The BODY is deliberately not echoed. On the success path it contains two live
		// credentials, and this error path is reached whenever the body did not parse —
		// including when it parsed as something unexpected but still carried them.
		return TokenSet{}, newError(failCode, fmt.Sprintf("the identity provider answered %d with a response this build could not read", resp.StatusCode))
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 || tr.Error != "" {
		// invalid_grant gets its OWN code, because it is the single provider answer that
		// means "this session is gone" rather than "this call did not work". The caller
		// deletes the stored credential on that and only that.
		code := failCode
		if resp.StatusCode == http.StatusBadRequest && tr.Error == "invalid_grant" {
			code = CodeGrantRejected
		}
		// A proxy or provider outage can carry a stale OAuth error body. Only
		// the protocol's 400 rejection authorizes deleting a rotating credential.
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return TokenSet{}, newError(code, tokenErrorMessage(resp.StatusCode, ""))
		}
		return TokenSet{}, newError(code, tokenErrorMessage(resp.StatusCode, tr.Error)).
			withHint(tokenErrorHint(tr.Error))
	}

	if strings.TrimSpace(tr.AccessToken) == "" {
		return TokenSet{}, newError(failCode, "the identity provider returned no access token")
	}
	// Bearer is the only type this client knows how to send. Accepting anything else
	// would mean composing an Authorization header we do not understand.
	if tt := strings.TrimSpace(tr.TokenType); tt != "" && !strings.EqualFold(tt, "bearer") {
		return TokenSet{}, newError(failCode, fmt.Sprintf("the identity provider issued an unsupported %q token", safeEcho(tt)))
	}

	// Check seconds before converting: multiplication can wrap a centuries-long
	// expires_in into an ordinary positive duration and bypass the lifetime cap.
	if tr.ExpiresIn > int64(maxTokenLifetime/time.Second) {
		return TokenSet{}, newError(failCode, "the identity provider claimed an implausibly long token lifetime")
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	switch {
	case tr.ExpiresIn <= 0:
		// No lifetime reported. Fall back to the JWT's own exp when it has one, and
		// otherwise leave ExpiresAt zero — which disables proactive refresh rather than
		// inventing a schedule, so the reactive 401 path handles it.
		lifetime = 0
	case lifetime < minTokenLifetime:
		return TokenSet{}, newError(failCode, "the identity provider issued a token that expires immediately")
	}

	set := TokenSet{
		AccessToken:  tr.AccessToken,
		RefreshToken: strings.TrimSpace(tr.RefreshToken),
		TokenType:    "Bearer",
	}
	if lifetime > 0 {
		set.ExpiresAt = c.now().Add(lifetime)
	} else if exp, ok := accessTokenExpiry(tr.AccessToken); ok {
		// The same sanity bound the declared lifetime gets. Without it a JWT claiming an
		// expiry a year out would disable refresh entirely — and a revocation would then
		// go unnoticed for a year — while expires_in claiming the same thing is refused.
		// A bound that one path can walk around is not a bound.
		if d := exp.Sub(c.now()); d > minTokenLifetime && d <= maxTokenLifetime {
			set.ExpiresAt = exp
		}
	}
	return set, nil
}

// tokenErrorMessage renders a provider failure without echoing its description.
//
// error_description is provider-controlled free text that lands in terminal scrollback
// and a debug log. The stable `error` CODE is enough to say something useful, and it is
// the only part of the response this build is willing to repeat.
func tokenErrorMessage(status int, code string) string {
	switch code {
	case "invalid_grant":
		return "the identity provider rejected the sign-in as expired or already used"
	case "invalid_client":
		return "the identity provider does not recognise this application"
	case "invalid_request":
		return "the identity provider rejected the sign-in request as malformed"
	case "unauthorized_client":
		return "this application is not permitted to use this sign-in method"
	case "":
		return fmt.Sprintf("the identity provider answered %d", status)
	}
	return fmt.Sprintf("the identity provider refused the request (%s)", safeEcho(code))
}

// tokenErrorHint names the next action for the codes where there is one.
func tokenErrorHint(code string) string {
	switch code {
	case "invalid_grant":
		return "Sign in again."
	case "invalid_client", "unauthorized_client":
		return "This build's OAuth client does not match the backend it is pointed at."
	}
	return ""
}

// accessTokenExpiry reads `exp` out of a JWT access token, for display and scheduling
// only.
//
// The signature is NOT verified, and it must never be treated as if it were: the backend
// is the authority on whether a token is acceptable, and a client that decided for
// itself would accept a forged token as easily as a real one. All this buys is a refresh
// schedule when the provider did not send expires_in — a convenience, never a check.
func accessTokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
