package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"time"
)

// Backend is the full client surface the app depends on (satisfied by *Client, and
// trivially by a fake in tests). Holding the app's dependency as this interface lets
// tests inject a fake backend without a live server.
type Backend interface {
	RespondStream(ctx context.Context, req RespondRequest, cb StreamCallbacks) (RespondResult, error)
	RunTask(ctx context.Context, req TaskRequest) (TaskResult, error)
	Capabilities(ctx context.Context) (Capabilities, error)
	VerifyKey(ctx context.Context) (KeyVerification, error)
	Account(ctx context.Context) (AccountStatus, error)
	Version(ctx context.Context) (Version, error)
	Health(ctx context.Context) error
	Ready(ctx context.Context) error
	BaseURL() string
}

// Client is the native Daintree backend HTTP client. It speaks the Daintree-native
// protocol (NOT OpenAI), streams the respond endpoint as named SSE events, and
// runs server-owned utility tasks. It is safe for concurrent use.
type Client struct {
	baseURL string
	// tokens supplies the bearer credential per REQUEST rather than per client, so an
	// hourly access-token refresh reaches every in-flight consumer without rebuilding
	// the transport, the retry policy or the cost hook. Never nil — NewClient
	// substitutes NoTokenSource, which is exactly the old empty-key behaviour.
	tokens TokenSource
	// http serves the streamed respond POST; jsonHTTP serves everything routed
	// through doJSON. They differ only in ResponseHeaderTimeout (see NewClient).
	http     *http.Client
	jsonHTTP *http.Client
	info     ClientInfo
	retry    RetryPolicy
	onRetry  func(RetryInfo)
	onTask   func(TaskTraceInfo)
	onCost   func(CostEvent)
	routing  func() Routing
	// streamIdleTimeout overrides sseIdleTimeout for the respond stream's idle
	// watchdog. Zero selects the default; tests shrink it to exercise the abort.
	streamIdleTimeout time.Duration
}

// ClientConfig configures a Client. Both credential fields are OPTIONAL and empty on
// virtually every install: the backend holds its own upstream key and serves a request
// carrying no Authorization header at all. HTTPClient defaults to one with NO global
// timeout (a streamed turn can run for minutes; cancellation is via context).
type ClientConfig struct {
	BaseURL string
	// APIKey is the fixed-credential spelling, kept for DAINTREE_API_KEY and for tests.
	// It is sugar for TokenSource: a non-empty value becomes a StaticTokenSource.
	// Naming both is a configuration mistake, not a precedence question, so NewClient
	// takes TokenSource and ignores APIKey when both are set.
	APIKey string
	// TokenSource supplies a credential that can CHANGE between requests — an account
	// access token that expires hourly and is refreshed underneath the client. Nil
	// falls back to APIKey, then to NoTokenSource.
	TokenSource TokenSource
	HTTPClient  *http.Client
	ClientInfo  ClientInfo
	// Retry tunes transient-failure retries for every backend call. The zero value
	// selects DefaultRetryPolicy (10 attempts settling into a 10–15s poll — the
	// backend owns provider retries; this covers only the CLI↔backend hop). Set
	// Retry.MaxAttempts to 1 to disable retries.
	Retry RetryPolicy
	// OnRetry, if set, is invoked just before each backoff sleep when a transient
	// failure will be retried — on the streamed respond turn AND on the JSON
	// endpoints (tasks / capabilities / health / ready). Observability only; it must
	// not block. RetryInfo.Op names which call is being replayed.
	OnRetry func(RetryInfo)
	// OnTask, if set, is invoked after every RunTask round trip (success or failure).
	// Observability only — it must not block. Without it the utility tasks are the
	// one backend surface a session log cannot see: a /compact's checkpoint +
	// memory_distill calls (and every watcher classify/judge/extract) would leave no
	// trace at all.
	OnTask func(TaskTraceInfo)
	// OnCost, if set, is invoked once for every billed upstream call this client makes
	// — every respond turn AND every utility task. Observability only; it must not
	// block.
	//
	// It lives on the CLIENT rather than on the agent session because a session sees
	// only turns. A day of orchestration also spends money on dozens of
	// terminal.summarize / terminal.extract / watcher-classify tasks, fired from tools,
	// watchers and compaction alike — real spend on the backend's upstream credential,
	// outside any turn. One hook at the layer every call passes through is the only way
	// to count all of it without every future caller remembering to.
	OnCost func(CostEvent)
	// RoutingPreference, if set, is read for every request this client makes — turns AND
	// utility tasks. It lives on the client for the same reason OnCost does: a task
	// sends the caller's content upstream just as a turn does, and a privacy choice that
	// covered only the visible path would be the most misleading kind of half-measure.
	// A zero Routing means "no preference" and omits the block.
	RoutingPreference func() Routing
}

// CostEvent is one billed backend REQUEST, reported to ClientConfig.OnCost.
//
// One event is not one provider call: a single turn can bill the runbook selector, a
// repair pass, a losing speculative generation and the main completion. Amount is the
// request's total across all of them, which is what the backend's upstream credential
// was charged for this one request.
//
// It is emitted even when Amount is nil — "this request happened and reported no cost"
// is the fact that turns a session total into a LOWER BOUND, and an accumulator that
// only heard about the requests carrying numbers would present a partial sum as a
// receipt.
type CostEvent struct {
	// Op is "respond" for a turn, or the task id ("terminal_summarize", "checkpoint", …).
	Op string
	// Amount is USD for the whole request, or nil when nothing was reported.
	// nil means UNKNOWN. It never means zero.
	Amount *float64
	// Complete is false when this request ran work whose cost could not be measured, so
	// Amount is a floor rather than a sum. Set from the backend's own `cost.complete`,
	// and forced false when an earlier retried attempt of the same call already billed.
	Complete bool
	// CachedTokens/PromptTokens back the prompt-cache hit ratio. They cover the MAIN
	// completion only — the selector's usage is not exposed per call — so the ratio is
	// about the main call, not the whole request, and consumers should say so. It rides
	// here because it EXPLAINS the spend beside it: the backend's byte-stable prompt
	// assembly exists to keep ~18k tokens of tool schemas cached, and a collapse in this
	// ratio is the first symptom of a regression that spends real money on the backend's
	// upstream credential.
	CachedTokens int
	PromptTokens int
}

// TaskTraceInfo describes one completed RunTask round trip for the OnTask hook.
// Err is nil on success; sizes are the serialized envelope input and the raw task
// output (bounded facts for a log line — never the payloads themselves).
type TaskTraceInfo struct {
	Task        string
	Duration    time.Duration
	InputBytes  int
	OutputBytes int
	Err         error
}

// Connection-establishment timeouts shared by both default transports. These bound
// the phases that can hang silently on a dead network path; they do NOT bound how
// long an accepted request may take to answer or stream.
const (
	transportDialTimeout         = 5 * time.Second
	transportTLSHandshakeTimeout = 5 * time.Second
	// streamResponseHeaderTimeout bounds how long the respond POST may wait for the
	// response headers. The backend commits the SSE response as soon as runbook
	// selection completes (~1.5–2.5s), well before the upstream model produces
	// anything, so 10s is generous headroom without letting a wedged backend pin a
	// turn. doJSON's client deliberately has NO header timeout — a utility task runs
	// its whole model call before responding, which routinely exceeds 10s.
	streamResponseHeaderTimeout = 10 * time.Second
)

// proxyExceptLoopback is http.ProxyFromEnvironment with one guarantee added: a URL this
// package classifies as loopback is NEVER sent to a proxy.
//
// Without it, routing and classification disagree. Go's own proxy bypass fires only for
// the exact lowercase host "localhost" and for parseable loopback IP literals, whereas
// AllowsUnverifiedSignIn (IsLoopbackURL) also accepts "LOCALHOST",
// "localhost.", "dev.localhost", and "127.0.0.1." — every one of which genuinely
// addresses this machine, and every one of which stock ProxyFromEnvironment would hand
// to HTTP_PROXY. That gap is exactly wrong in two ways at once: a host classified as
// trusted-local would be routed through a third party, and over plain http that party
// would see the whole turn — prose, tool arguments, results, and any bearer the request
// happens to carry — in clear text.
//
// Deriving both from the same predicate makes the two agree by construction, so
// widening the loopback definition later cannot silently reopen this.
func proxyExceptLoopback(req *http.Request) (*url.URL, error) {
	if req != nil && req.URL != nil && IsLoopbackURL(req.URL.String()) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(req)
}

// errRedirectRefused is returned by noRedirects. A sentinel so the transport-error
// mapping can classify it as final rather than as a retriable "connect" — replaying a
// refused redirect nine times over a 75-second backoff only re-derives the same answer.
var errRedirectRefused = errors.New("backend: refusing to follow a redirect")

// noRedirects refuses EVERY redirect, on both clients.
//
// Without a CheckRedirect, Go follows redirects with the default policy, and a 307/308
// REPLAYS the POST body at the new location. The body of a respond request is the whole
// conversation — prose, file paths, tool arguments, results — so a backend answering
// with a remote Location moves all of it off-box in one hop.
//
// That is not a hypothetical for this engine specifically. Daintree pins the assistant
// to loopback precisely BECAUSE the native panel is unauthenticated and carries
// everything in that request; a followed redirect walks straight through the pin, and
// through the sibling defence next door (proxyExceptLoopback), both of which only ever
// inspect the endpoint the session was CONFIGURED with.
//
// Refusing outright rather than validating each hop against isLoopbackHost, because the
// assistant API has no legitimate reason to redirect at all: the client owns the full
// URL, builds fixed paths against it, and speaks JSON and SSE — there is no HTML
// navigation, no auth handshake, and no canonical-host normalisation for it to follow.
// DefaultBaseURL is already https, so not even an http→https upgrade applies. "Follows
// no redirects" is also a property a reader can verify by reading one function, where
// "follows only safe ones" has to be re-verified against the loopback predicate every
// time that predicate changes.
func noRedirects(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("%w to %s — the assistant API does not redirect, and following it would replay this request (the whole conversation, file paths and tool arguments) at an endpoint this session was never pointed at", errRedirectRefused, req.URL.Redacted())
}

// transportError maps a *http.Client.Do failure onto the wire error taxonomy. A refused
// redirect gets its OWN code so the retry layer treats it as final: every code except
// "connect" falls through isRetriable to false, the same trick doJSON uses for "timeout".
func transportError(err error) *Error {
	if errors.Is(err, errRedirectRefused) {
		return &Error{Code: "redirect_refused", Message: "assistant backend redirected the request: " + err.Error()}
	}
	return &Error{Code: "connect", Message: "could not reach assistant backend: " + err.Error()}
}

// newTransport builds the structured default transport: bounded dial + TLS
// handshake, keep-alives on (connection reuse matters — every turn round-trips),
// and an optional response-header bound (0 = none).
func newTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: proxyExceptLoopback,
		DialContext: (&net.Dialer{
			Timeout:   transportDialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   transportTLSHandshakeTimeout,
		ResponseHeaderTimeout: responseHeaderTimeout,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
	}
}

// NewClient builds a Client. An empty BaseURL falls back to DefaultBaseURL.
func NewClient(cfg ClientConfig) *Client {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		base = DefaultBaseURL
	}
	// A caller-supplied client is used for BOTH paths (tests inject one fake
	// transport). Otherwise build two clients over structured transports:
	//
	//   - stream client: NO client-wide Timeout — a streamed turn can legitimately
	//     run for minutes, so liveness is enforced by the connection-phase timeouts
	//     above plus the rolling SSE idle watchdog (see respondStreamOnce), never by
	//     a whole-request deadline. Cancellation is via context.
	//   - JSON client: also NO client-wide Timeout, because doJSON guarantees every
	//     call a context deadline (60s default when the caller sets none) — a
	//     per-call ctx deadline composes with caller-chosen budgets, where a global
	//     Timeout would silently cap them. No ResponseHeaderTimeout either: utility
	//     tasks answer only after their full server-side model call.
	hc := cfg.HTTPClient
	jc := cfg.HTTPClient
	if hc == nil {
		hc = &http.Client{Transport: newTransport(streamResponseHeaderTimeout), CheckRedirect: noRedirects}
		jc = &http.Client{Transport: newTransport(0), CheckRedirect: noRedirects}
	}
	retry := cfg.Retry
	if retry.MaxAttempts == 0 {
		retry = DefaultRetryPolicy()
	}
	// TokenSource wins over APIKey when both are given: a caller that built a live
	// source has strictly more information than one that froze a string, and silently
	// preferring the frozen one would pin the client to a token that stops working in
	// an hour.
	tokens := cfg.TokenSource
	if tokens == nil {
		if key := strings.TrimSpace(cfg.APIKey); key != "" {
			tokens = StaticTokenSource{Token: key}
		} else {
			tokens = NoTokenSource{}
		}
	}
	return &Client{
		baseURL:  base,
		tokens:   tokens,
		http:     hc,
		jsonHTTP: jc,
		info:     cfg.ClientInfo,
		retry:    retry,
		onRetry:  cfg.OnRetry,
		onTask:   cfg.OnTask,
		onCost:   cfg.OnCost,
		routing:  cfg.RoutingPreference,
	}
}

// BaseURL returns the configured backend base URL (for diagnostics / doctor).
func (c *Client) BaseURL() string { return c.baseURL }

// setHeaders applies the common headers and, for a PROTECTED path, obtains and
// attaches the bearer credential.
//
// The public/protected split is new and load-bearing. Previously every request got the
// header whenever a key existed, which was harmless for a frozen string and is not
// harmless for a token source: /healthz, /readyz and /version are exactly what someone
// probes when their login is broken, and making those probes wait on — or fail with —
// a credential fetch would take the diagnostic offline at the moment it is needed.
// /v1/daintree/auth/config is public for a sharper reason still: it is what the auth
// layer READS to learn how to obtain a token, so requiring one would be a bootstrap no
// client could satisfy.
//
// An error from the source aborts the request rather than sending it bare. Falling
// back to an anonymous request would silently downgrade an authenticated session and
// bill or refuse the wrong principal, which is far worse than a visible failure.
func (c *Client) setHeaders(ctx context.Context, req *http.Request, accept string, path string) error {
	token, err := c.credential(ctx, path)
	if err != nil {
		return err
	}
	c.applyHeaders(req, accept, token)
	return nil
}

// credential obtains the bearer for one request, or "" when the path is public or no
// credential is configured.
//
// The failure is CodeCredentialUnavailable, never CodeAuthRequired: no request was
// sent, so this is not the backend rejecting us. The distinction is load-bearing
// downstream — auth_required means "sign in", which is the wrong instruction for a
// locked keychain, and it would also make doctor report that the backend refused a
// request it never received. The cause is wrapped rather than formatted so errors.Is
// can still recover whatever sentinel the auth layer raised.
func (c *Client) credential(ctx context.Context, path string) (string, error) {
	if isPublicPath(path) {
		return "", nil
	}
	token, err := c.tokens.AccessToken(ctx)
	if err != nil {
		return "", &Error{
			Code:    CodeCredentialUnavailable,
			Type:    "authentication_error",
			Message: "could not obtain an account credential: " + err.Error(),
			cause:   err,
		}
	}
	return token, nil
}

// accountAttempt records which credential a protected request was made with, so its
// outcome can be attributed to that credential and not to whatever has replaced it.
//
// The zero value is inert: every method is a no-op, which is what a public path, an
// anonymous request, or a token source that does not observe all produce. That keeps
// the call sites free of nil checks on the hot path.
type accountAttempt struct {
	obs   AccountObserver
	gen   uint64
	token string
	// deferSuccess suppresses the automatic MarkIdentityLive on a 2xx, leaving the
	// verdict to whoever decodes the body.
	//
	// It exists for exactly one endpoint. /v1/daintree/account answers 200 for a caller
	// with NO plan — that is the whole design, since it is a status read rather than
	// paid work — so the transport's "a protected request succeeded, therefore this
	// session is confirmed" inference is at best redundant there and at worst wrong:
	// the decoded body is about to say something more specific, including that the
	// entitlement could not be established at all.
	//
	// It is deliberately kept even though MarkIdentityLive no longer grants anything.
	// The liveness stamp it writes is the same field ApplyAccountStatus writes, and
	// letting the transport set it from a body the decoder is about to REJECT
	// (account_contract_invalid) would record a verification that never happened.
	//
	// A malformed body therefore lands the way it should: nothing is confirmed, the
	// credential is untouched, and the caller reports "could not verify".
	deferSuccess bool
}

// beginAccountAttempt samples the identity generation before a request goes out.
//
// Sampling BEFORE rather than reading after is the whole point: a request can outlive a
// logout, and a verdict applied against whatever generation happens to be current when
// the answer lands would let a stale 401 clear a session the user just created.
//
// Nothing is observed when no credential was sent. An anonymous request's 401 says the
// backend wants an account, not that any stored credential is bad, and feeding it to the
// state machine would mark a perfectly good session signed-out on a deployment that had
// simply started requiring auth on a path we called without one.
func (c *Client) beginAccountAttempt(path, token string) accountAttempt {
	if token == "" {
		return accountAttempt{}
	}
	obs, ok := c.tokens.(AccountObserver)
	if !ok {
		return accountAttempt{}
	}
	return accountAttempt{
		obs:          obs,
		gen:          obs.Generation(),
		token:        token,
		deferSuccess: path == AccountStatusPath,
	}
}

// succeeded reports a protected 2xx for the credential this attempt carried, unless the
// endpoint's own body is the authoritative verdict (see deferSuccess).
//
// What it reports is IDENTITY liveness and never entitlement: this layer knows only
// that the request came back 2xx, and most protected routes answer 2xx whatever billing
// says. See AccountObserver.MarkIdentityLive.
func (a accountAttempt) succeeded() {
	if a.obs != nil && !a.deferSuccess {
		a.obs.MarkIdentityLive(a.gen)
	}
}

// failed hands an account verdict to the observer. Non-account errors are passed too —
// the observer's own taxonomy check is the authority on what counts, and duplicating
// that judgement here would let the two drift.
func (a accountAttempt) failed(ctx context.Context, err error) {
	if a.obs != nil {
		a.obs.ApplyBackendVerdict(ctx, a.gen, a.token, err)
	}
}

// wantsRefreshReplay reports an error whose ONE correct response is to refresh the
// credential and send the request again.
//
// Bounded to a single replay by the caller, and gated there on nothing having been shown
// to the user yet. Two conditions qualify and no others: an expired token (a refresh is
// exactly the fix) and an invalid one (a refresh is worth trying when a stored session
// exists to refresh from). A revoked session and the two 403s are deliberately excluded
// — refreshing those mints another credential wrong in the same way, forever.
func wantsRefreshReplay(err error) bool {
	var be *Error
	if !errors.As(err, &be) {
		return false
	}
	switch be.AuthRemedy() {
	case RemedyRefresh, RemedyRefreshOrSignIn:
		return true
	}
	return false
}

// renewedCredential fetches the credential again after a verdict has dropped the old
// one, and reports whether it is actually usable for a replay.
//
// The check is "non-empty AND different", and both halves earn their place. An empty
// result means the refresh could not produce one — often because the provider rejected
// the grant and the session was deleted — and replaying then sends the request with no
// credential at all: on a backend that serves anonymous callers that SUCCEEDS, as the
// wrong principal, and reports a confirmed session for one that has just been removed.
// An unchanged result means no renewal happened, so the replay would re-present the
// exact value the backend refused a moment ago.
func (c *Client) renewedCredential(ctx context.Context, path, previous string) (string, bool) {
	next, err := c.credential(ctx, path)
	if err != nil || next == "" || next == previous {
		return "", false
	}
	return next, true
}

// replaySafe reports that a streamed turn can still be replayed without a human noticing.
//
// contentStreamed alone is NOT that boundary, which is the mistake this replaces. It
// tracks executor prose only — a preamble is painted on screen before any of it arrives,
// so replaying after one duplicates visible text. Meta is a different objection with the
// same answer: it proves the selector already ran, so the backend has already been paid
// for work a replay would buy a second time.
func (c *Client) replaySafe(contentStreamed bool, shownPreamble string, meta ...*StreamMeta) bool {
	if contentStreamed || shownPreamble != "" {
		return false
	}
	for _, m := range meta {
		if m != nil {
			return false
		}
	}
	return true
}

// applyHeaders sets the common headers plus the bearer, if any.
func (c *Client) applyHeaders(req *http.Request, accept, token string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Daintree-Protocol", fmt.Sprintf("%d", ProtocolVersion))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

// currentSecrets returns the credential values worth masking in text destined for a
// human. Empty unless the token source opts into TokenScrubber — a source with nothing
// to protect (NoTokenSource, and the open-door install it represents) costs nothing.
func (c *Client) currentSecrets() []string {
	s, ok := c.tokens.(TokenScrubber)
	if !ok {
		return nil
	}
	return s.Secrets()
}

// scrubSecrets removes every credential this client has issued from text.
func (c *Client) scrubSecrets(text string) string {
	for _, secret := range c.currentSecrets() {
		text = ScrubKey(text, secret)
	}
	return text
}

// RespondStream runs one generation round against /v1/daintree/respond as a
// named-event SSE stream and returns the accumulated result. It forces
// generation.stream = true. A failure before the backend opens the SSE response
// arrives as an ordinary JSON error; upstream connection/generation failures after
// the eager meta event arrive as terminal SSE error events — both surface as *Error.
// The caller owns cancellation via ctx.
//
// Transient failures (connect errors, 5xx/gateway statuses, rate limits, and the
// mid-stream upstream/truncation errors the backend surfaces after the 200) are
// retried with exponential backoff per the client's RetryPolicy — but ONLY while no
// visible content has streamed yet, since a replay after the user has seen tokens
// would duplicate them. Replays are safe and near-free: the conversation prefix is
// unchanged and the backend prefix-caches it.
func (c *Client) RespondStream(ctx context.Context, req RespondRequest, cb StreamCallbacks) (RespondResult, error) {
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = ProtocolVersion
	}
	if req.Generation == nil {
		req.Generation = &Generation{}
	}
	req.Generation.Stream = true
	if req.Client == nil && c.info != (ClientInfo{}) {
		info := c.info
		req.Client = &info
	}
	// Same single stamping point as RunTask, so turns and tasks cannot end up under
	// different policies.
	if req.Routing == nil {
		req.Routing = c.routingPreference()
	}

	body, err := json.Marshal(req)
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: marshal respond request: %w", err)
	}

	// Retries must not double-fire side-effecting callbacks.
	//
	//   - OnRawMeta is intentionally observational and remains per-attempt. It may
	//     fire more than once so latency/debug instrumentation sees the real transport
	//     timeline; it must never adopt state or produce user-visible effects.
	//   - OnRunbookLoaded is intentionally EAGER: a selector result is worth recording before
	//     the upstream model connects or emits a token, so a trace shows selection latency
	//     separately from generation. The same request can be retried after receiving meta,
	//     so identical runbook refs are de-duplicated across attempts before reaching the
	//     caller — one load, one record. A failed attempt's signed state is
	//     also adopted into the next POST so the backend reuses that selection instead of
	//     paying for a second selector run that could land somewhere else.
	//   - OnContent is the retry BOUNDARY: once any visible token has reached the
	//     caller the turn is committed (a replay would duplicate on-screen text), so we
	//     only retry failures that arrive before the first content fragment.
	//   - OnMeta carries committed state/version metadata. Firing it once per attempt
	//     would advance state from a doomed attempt, so we CAPTURE each attempt's meta
	//     and forward it exactly once — from the attempt that commits (first content) or
	//     when the loop returns. If the terminal/cancelled attempt dies before meta, the
	//     last received (and retry-adopted) meta is forwarded so its signed state is not
	//     lost after selection has already been paid for and recorded.
	//
	// (OnToolCallDelta is intentionally NOT a boundary — see its doc on StreamCallbacks:
	// callers must treat the returned ToolCalls as authoritative, since a pre-content
	// failure that already streamed tool-call fragments is still replayed.)
	contentStreamed := false
	metaForwarded := false
	var pendingMeta *StreamMeta
	var lastReceivedMeta *StreamMeta
	userOnMeta := cb.OnMeta
	userOnRunbookLoaded := cb.OnRunbookLoaded
	userOnContent := cb.OnContent
	seenRunbookLoads := make(map[string]struct{})

	flushMeta := func() {
		if metaForwarded {
			return
		}
		meta := pendingMeta
		if meta == nil {
			// A later retry can fail or be cancelled before receiving its own meta.
			// Preserve the last selector/state outcome already surfaced and adopted
			// rather than silently losing it at the terminal boundary.
			meta = lastReceivedMeta
		}
		if meta == nil {
			return
		}
		metaForwarded = true
		if userOnMeta != nil {
			userOnMeta(*meta)
		}
	}

	cb.OnMeta = func(m StreamMeta) {
		mm := m
		pendingMeta = &mm // captured; not forwarded until the attempt commits
		lastReceivedMeta = &mm
	}
	if userOnRunbookLoaded != nil {
		cb.OnRunbookLoaded = func(refs []RunbookRef) {
			unseen := make([]RunbookRef, 0, len(refs))
			for _, ref := range refs {
				// The id is the stable runbook identity. Fall back to the display title for
				// malformed refs, but do not let harmless title drift on a retry produce
				// a duplicate card for the same id.
				key := strings.TrimSpace(ref.ID)
				if key == "" {
					key = strings.TrimSpace(ref.Title)
				}
				if key == "" {
					continue
				}
				if _, ok := seenRunbookLoads[key]; ok {
					continue
				}
				seenRunbookLoads[key] = struct{}{}
				unseen = append(unseen, ref)
			}
			if len(unseen) > 0 {
				userOnRunbookLoaded(unseen)
			}
		}
	}
	cb.OnContent = func(s string) {
		flushMeta() // first token commits this attempt → its meta is the real one
		contentStreamed = true
		if userOnContent != nil {
			userOnContent(s)
		}
	}
	// The preamble is FIRST-WINS across attempts, and deliberately not the retry
	// boundary — see StreamCallbacks.OnPreamble.
	//
	// First-wins is what keeps the screen and the committed history the same text.
	// A replayed attempt asks the fast model again and gets its own wording, so
	// last-wins would commit a sentence the user never read: they saw attempt 1's
	// preamble, and nothing on screen is rewritten by attempt 2 arriving. The cost
	// is that the winning attempt's executor was handed slightly different wording
	// as its prior turn than the history records — a one-turn inconsistency on a
	// rare path, against showing one thing and recording another on every retry.
	shownPreamble := ""
	userOnPreamble := cb.OnPreamble
	cb.OnPreamble = func(p StreamPreamble) {
		if shownPreamble != "" {
			return // already on screen; a replay must not stack a second one
		}
		shownPreamble = p.Content
		if userOnPreamble != nil {
			userOnPreamble(p)
		}
	}

	// Cost accounting spans the whole retried call, not one attempt, and it is the one
	// piece of bookkeeping that must survive EVERY exit path — a retried turn is real
	// money off the backend's upstream credential whether or not the caller ever sees a
	// reply.
	//
	// abandonedSpend records that some earlier attempt reached the point of billing
	// (it got a meta event, which means the runbook selector already ran and charged)
	// and then failed. That money is invisible: a failed attempt never reaches its
	// `done` event, and the succeeding attempt's `cost.total` covers only ITS OWN
	// request — the backend aggregates re-rolls within one request, never across
	// separate HTTP attempts. So a replayed turn's reported total is a floor, and
	// saying otherwise would present a number below the real bill as exact.
	abandonedSpend := false
	// finalize emits EXACTLY ONE cost event for this RespondStream call, whatever
	// happens. Every return below routes through it.
	reported := false
	finalize := func(result RespondResult, failed bool) {
		if reported {
			return
		}
		reported = true
		switch {
		case !failed:
			c.reportRespondCost(result, abandonedSpend)
		case abandonedSpend:
			// Failed, cancelled, or gave up — but money was already spent that nothing
			// will ever report. An unknown amount, which is what makes a session total
			// a lower bound rather than silently omitting a turn that cost real money.
			c.reportCost(CostEvent{Op: RespondOp, Complete: false})
		}
		// A failure with no billing (a refused socket, a 400, a key the provider never
		// accepted) reports nothing: there is no spend to account for, and inventing an
		// "unknown" would caveat a total that has nothing wrong with it.
	}

	// The credential this turn is about, sampled before the first attempt. See the same
	// block in doJSONRetry for why the observation is best-effort.
	sampledToken, _ := c.credential(ctx, respondPath)
	acct := c.beginAccountAttempt(respondPath, sampledToken)
	refreshReplayed := false

	started := time.Now()
	for attempt := 0; ; attempt++ {
		pendingMeta = nil // each attempt brings its own meta
		result, serr := c.respondStreamOnce(ctx, body, cb)
		if serr == nil {
			acct.succeeded()
			flushMeta() // committed success (incl. pure tool-call turns with no content)
			// Commit exactly what was shown. Joined onto the front of the assistant
			// content in this one place, so every caller downstream — history, the
			// display's end hook, the trace — records the turn the user actually saw
			// without needing to know this feature exists. The backend hands the
			// executor these same bytes as its own prior assistant turn, so ONE
			// joined message is the honest shape; committing two would claim a turn
			// boundary the executor never saw.
			result.Preamble = shownPreamble
			if shownPreamble != "" {
				if body := result.Message.Content; body != "" {
					result.Message.Content = shownPreamble + "\n\n" + body
				} else {
					// A tool-call round can legitimately produce no prose at all.
					// The preamble is still what the user saw, and still belongs to
					// this turn.
					result.Message.Content = shownPreamble
				}
			}
			finalize(result, false)
			return result, nil
		}
		// This attempt billed if it got far enough for the selector to have run. Recorded
		// BEFORE every failure exit, so an attempt that dies on the way out still counts.
		if pendingMeta != nil || lastReceivedMeta != nil || contentStreamed {
			abandonedSpend = true
		}
		// Caller cancellation (Escape / shutdown) is a clean stop, never a retry — but
		// the verdict that arrived first is still recorded. A revocation racing an
		// Escape keypress must not be lost.
		if ctx.Err() != nil {
			acct.failed(ctx, serr)
			flushMeta()
			finalize(result, true)
			return result, serr
		}
		// The auth ladder, gated on nothing having been SHOWN yet — the same boundary
		// the transient retry uses, and for the same reason. Once a token has reached
		// the caller the turn is committed: replaying it would duplicate on-screen text,
		// and no credential problem is worth doing that over. An expired token that
		// surfaces mid-stream therefore ends the turn, and the verdict below still
		// reaches the state machine so the NEXT turn starts with a fresh credential.
		if !refreshReplayed && c.replaySafe(contentStreamed, shownPreamble, pendingMeta, lastReceivedMeta) &&
			wantsRefreshReplay(serr) {
			refreshReplayed = true
			acct.failed(ctx, serr)
			next, ok := c.renewedCredential(ctx, respondPath, acct.token)
			if !ok {
				flushMeta()
				finalize(result, true)
				return result, serr
			}
			acct = c.beginAccountAttempt(respondPath, next)
			continue
		}
		be, ok := serr.(*Error)
		// Stop if content already streamed (can't replay), the failure isn't a
		// transient class, retries are disabled, or the attempt budget is spent.
		if contentStreamed || !ok || !isRetriable(be) || attempt+1 >= c.retry.MaxAttempts || retriesDisabled(ctx) {
			acct.failed(ctx, serr)
			flushMeta()
			finalize(result, true)
			return result, serr
		}
		// Selection already completed if this attempt delivered meta. Pin that signed
		// outcome into the next request before replaying the turn: selection has already
		// been paid for, so a full-request retry must reuse the same active set rather
		// than rerun a selector that could choose something different. OnMeta remains
		// deferred; adopting the opaque token here is transport bookkeeping only.
		if pendingMeta != nil && pendingMeta.State != "" {
			state := pendingMeta.State
			req.State = &state
			nextBody, marshalErr := json.Marshal(req)
			if marshalErr != nil {
				// Practically infallible, and still a terminal exit: the failure that
				// sent us here is reported before the marshal error replaces it.
				acct.failed(ctx, serr)
				flushMeta()
				finalize(result, true)
				return result, fmt.Errorf("backend: marshal retry request: %w", marshalErr)
			}
			body = nextBody
		}
		// Retrying: this attempt's meta is discarded (reset at the loop top).
		delay := c.retry.backoff(attempt, be.RetryAfter)
		// A stream attempt is not free — it can burn its whole idle window before
		// failing — so the attempt budget alone does not bound the turn. Stop once
		// the restart-recovery window is spent and surface the failure.
		if c.retry.exhausted(time.Since(started), delay) {
			acct.failed(ctx, serr)
			flushMeta()
			finalize(result, true)
			return result, serr
		}
		info := RetryInfo{Attempt: attempt, MaxAttempts: c.retry.MaxAttempts, Delay: delay, Op: RespondOp, Err: be}
		if c.onRetry != nil {
			c.onRetry(info)
		}
		// The per-call hook drives the live "retrying…" cue. With a budget that can
		// now span a minute, a silent spinner would read as a hang.
		if cb.OnRetry != nil {
			cb.OnRetry(info)
		}
		if !sleepCtx(ctx, delay) {
			// Cancelled mid-backoff: surface the last failure (the caller reads
			// ctx.Err() and treats it as a clean cancel).
			acct.failed(ctx, serr)
			flushMeta()
			finalize(result, true)
			return result, serr
		}
	}
}

// respondStreamOnce performs a single respond attempt: build the request from the
// already-marshaled body, POST it, and parse the SSE stream. Each attempt builds a
// fresh *http.Request because the body reader is consumed.
//
// The parse is guarded by a rolling idle watchdog: any read progress on the response
// body (data, comments/heartbeats, anything) resets it; a stream that stays open but
// goes silent past the window is aborted by cancelling THIS attempt's request context
// and surfaced as a typed stream_idle_timeout error — distinct from a caller
// cancellation, which leaves the parent ctx.Err() non-nil.
func (c *Client) respondStreamOnce(ctx context.Context, body []byte, cb StreamCallbacks) (RespondResult, error) {
	attemptCtx, cancelAttempt := context.WithCancel(ctx)
	defer cancelAttempt()

	// The credential is acquired BEFORE the transport recorder is installed, and that
	// ordering is deliberate. Obtaining an account token can mean a synchronous refresh
	// against the identity provider — a whole DNS lookup, TLS handshake and round trip
	// of its own. Fetched under the traced context, every one of those events would be
	// recorded as if it were the BACKEND's, and the elapsed marks would silently include
	// refresh latency. A turn that looked slow because the token was stale would be
	// indistinguishable from one where the backend was slow, which is precisely the
	// distinction these marks exist to draw.
	//
	// It uses attemptCtx all the same, so the refresh honours this attempt's
	// cancellation and deadline rather than outliving it.
	token, err := c.credential(attemptCtx, respondPath)
	if err != nil {
		// Scrubbed like every other error leaving this client: a token source can echo
		// a credential it issued into its own failure text, and this one reaches the
		// turn's error rendering and the debug log unmediated.
		return RespondResult{}, c.scrubError(err)
	}

	// Client-side latency marks for THIS attempt. The backend's own timings begin when
	// the request lands, so without these the difference between a client-measured round
	// and the server's total is one opaque number covering dial, TLS, upload and the
	// flight home. See transport.go.
	marks := newTransportRecorder()
	attemptCtx = httptrace.WithClientTrace(attemptCtx, marks.trace())

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.baseURL+respondPath, bytes.NewReader(body))
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: build respond request: %w", err)
	}
	c.applyHeaders(httpReq, "text/event-stream", token)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// Marks even here: a transport failure that got as far as a TLS handshake before
		// dying is a completely different diagnosis from a refused socket, and only the
		// marks tell them apart. A truly refused connection records nothing, which is
		// the honest answer rather than a row of zeroes.
		return RespondResult{Transport: marks.result()}, transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// A 502/503 unambiguously reached the wire — it HAS a first byte. Dropping its
		// marks would leave the log implying the opposite.
		out := RespondResult{Transport: marks.result()}
		return out, c.readErrorResponse(resp)
	}

	idle := c.streamIdleTimeout
	if idle <= 0 {
		idle = sseIdleTimeout
	}
	watchdog := newIdleWatchdog(idle, cancelAttempt)
	defer watchdog.Stop()

	// The request id rides the 200's headers, which parseRespondStream never sees — so
	// without stamping it here, a mid-stream failure (the COMMON case for an upstream
	// problem, since the backend emits `meta` before it opens the upstream stream) would
	// be the one class of failure with no correlation id at all. Read once, before the
	// early return below, so the idle-timeout path gets it too.
	requestID := safeRequestID(resp.Header.Get(requestIDHeader))

	result, perr := parseRespondStream(&idleResetReader{r: resp.Body, reset: watchdog.Reset}, cb)
	// Stamped on every exit below, success or failure: a stream that died halfway is
	// exactly when "did we even reach them, and how long did that take" is worth asking.
	result.Transport = marks.result()
	if perr != nil && watchdog.Fired() && ctx.Err() == nil {
		return result, &Error{
			Code:      "stream_idle_timeout",
			Message:   fmt.Sprintf("stream idle: no bytes from the backend for %s", idle),
			Stream:    true,
			RequestID: requestID,
		}
	}
	// A terminal SSE `error` event carries an upstream-controlled message that reaches
	// the turn's error rendering and the debug log; parseRespondStream is a free
	// function with no access to the key, so the scrub happens here. See scrubError.
	return result, c.scrubError(withRequestID(perr, requestID))
}

// withRequestID stamps a correlation id onto a *Error, leaving any other error (and an
// already-stamped one) untouched.
func withRequestID(err error, id string) error {
	var be *Error
	if id == "" || !errors.As(err, &be) || be.RequestID != "" {
		return err
	}
	be.RequestID = id
	return err
}

// Respond runs a non-streaming generation round (used for tests / simple callers).
func (c *Client) Respond(ctx context.Context, req RespondRequest) (RespondResponse, error) {
	if req.ProtocolVersion == 0 {
		req.ProtocolVersion = ProtocolVersion
	}
	if req.Generation == nil {
		req.Generation = &Generation{}
	}
	req.Generation.Stream = false
	if req.Client == nil && c.info != (ClientInfo{}) {
		info := c.info
		req.Client = &info
	}
	if req.Routing == nil {
		req.Routing = c.routingPreference()
	}
	var out RespondResponse
	spendAbandoned, err := c.doJSONTracked(ctx, http.MethodPost, "/v1/daintree/respond", req, &out)
	if err != nil {
		// The SAME rule RunTask applies, asked in the same order and for the same reason.
		// Only the abandoned-attempt half was asked here, so a single terminal
		// `upstream_unavailable` — a turn the backend may well have paid for and then
		// failed to deliver — reported nothing at all, while the identical error on a
		// task correctly caveated the total. One transport spending one credential must
		// not keep two accounting rules.
		if spendAbandoned || taskMayHaveBilled(err) {
			c.reportCost(CostEvent{Op: RespondOp, Complete: false})
		}
		return RespondResponse{}, err
	}
	// The non-streaming path spends the same upstream credential exactly like the
	// streamed one, so it reports through the same seam. Reusing RespondResult keeps the
	// two from growing different accounting rules for the same request.
	c.reportRespondCost(RespondResult{Usage: out.Usage, Cost: out.Cost}, spendAbandoned)
	return out, nil
}

// RunTask runs a server-owned utility task against /v1/daintree/tasks. The CLI
// sends task DATA only; the backend owns the prompt, model, schema, and output
// mode. Decode TaskResult.Output into the task-specific output struct.
func (c *Client) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	// Stamp the routing preference HERE rather than at each call site. Tasks are fired
	// from tools, watchers, the async coordinator and compaction; a per-call-site
	// convention would be forgotten by the next one added, and the failure mode is
	// silent — the content still goes upstream, just under a weaker policy than the
	// user asked for.
	if req.Routing == nil {
		req.Routing = c.routingPreference()
	}
	start := time.Now()
	var out TaskResult
	spendAbandoned, err := c.doJSONTracked(ctx, http.MethodPost, "/v1/daintree/tasks", req, &out)
	if c.onTask != nil {
		// Guarded side-channel: a hook panic must never fail the task call itself.
		func() {
			defer func() { _ = recover() }()
			info := TaskTraceInfo{Task: req.Task, Duration: time.Since(start), Err: err}
			// The input size is re-serialized ONLY when someone is listening; the
			// envelope is small next to the model round trip it narrates.
			if blob, merr := json.Marshal(req.Input); merr == nil {
				info.InputBytes = len(blob)
			}
			info.OutputBytes = len(out.Output)
			c.onTask(info)
		}()
	}
	if err != nil {
		// A FAILED task still costs money more often than not: `task_output_invalid` is
		// raised only after a billed completion, usually after a second billed repair
		// pass. Report it as unknown spend so the session total becomes a lower bound
		// rather than silently omitting it — but only when a generation can actually
		// have run (see taskMayHaveBilled), so a 400 or a refused key does not caveat a
		// total that is perfectly accurate.
		// spendAbandoned is asked FIRST and separately: taskMayHaveBilled answers for
		// the error in hand, which is the LAST attempt, while a replayed call can have
		// billed on an attempt whose error no longer exists.
		if spendAbandoned || taskMayHaveBilled(err) {
			c.reportCost(CostEvent{Op: req.Task, Complete: false})
		}
		return TaskResult{}, err
	}
	// A task's usage.cost IS that task's total — the figure covers a repair pass too.
	// It does NOT cover an earlier HTTP attempt this call replaced, so a retried task
	// reports its figure as a floor rather than as the bill.
	c.reportCost(CostEvent{
		Op:           req.Task,
		Amount:       out.Usage.Cost,
		Complete:     !spendAbandoned,
		CachedTokens: out.Usage.CachedTokens,
		PromptTokens: out.Usage.PromptTokens,
	})
	return out, nil
}

// RespondOp is the CostEvent.Op value for a conversation turn. Every other value is a
// utility task, named by its task id.
const RespondOp = "respond"

// respondPath is the streamed turn's route. Named because three places now need to
// agree on it — the request itself, the credential sample the account ladder takes, and
// the re-sample after a refresh — and a literal repeated at each would let one drift
// onto a path with different auth requirements.
const respondPath = "/v1/daintree/respond"

// routingPreference returns the caller's endpoint-routing block, or nil when they
// expressed none — which omits the block and leaves the server default in force. Sending
// an empty object instead would be a different statement on the wire.
func (c *Client) routingPreference() *Routing {
	if c.routing == nil {
		return nil
	}
	r := c.routing()
	if r.IsZero() {
		return nil
	}
	return &r
}

// reportRespondCost emits the turn's spend. A missing `cost` block becomes an event with
// a nil Amount rather than no event at all: the turn definitely happened and definitely
// cost something, so the honest accumulator answer is "the running total is now a lower
// bound", never "nothing to add".
//
// abandonedSpend forces Complete=false. An earlier attempt of this same call billed and
// then failed, and the reported total covers only the succeeding request — the backend
// aggregates re-rolls WITHIN one request, never across separate HTTP attempts.
func (c *Client) reportRespondCost(res RespondResult, abandonedSpend bool) {
	ev := CostEvent{
		Op:           RespondOp,
		Complete:     res.Cost.IsComplete() && !abandonedSpend,
		CachedTokens: res.Usage.CachedTokens,
		PromptTokens: res.Usage.PromptTokens,
	}
	if res.Cost != nil {
		total := res.Cost.Total
		ev.Amount = &total
	}
	c.reportCost(ev)
}

// taskMayHaveBilled reports whether a FAILED task call could already have charged the
// backend's upstream credential — the difference between "nothing to account for" and
// "spend we cannot see".
//
// It matters because the most common task failure is not a rejection: `task_output_invalid`
// is raised only AFTER a billed completion, and often after a second billed repair pass.
// Treating that as free would let a session run dozens of paid extractions and report a
// total that omits every one of them.
//
// The listed conditions are the ones where no generation can have run: we never reached
// the backend, it refused the request at its own door, or the provider refused before
// generating. Everything else — a 5xx, a malformed output verdict, a client-side timeout
// on an accepted request — is counted as unknown spend.
//
// Every condition that names a backend VERDICT is read off the stable code. The statuses
// beside those codes in errors.go are orientation (a mid-stream envelope carries none at
// all, and `provider_invalid_api_key` is moving off the 401 it has always worn), so a
// rule that asked the number would answer differently for the same condition depending
// only on how far the request got. What remains status-shaped is a contract bug (a 400 by
// definition), a protocol mismatch, and the trailing unrecognised-body backstop.
//
// It answers for the error IN HAND, which on a retried call is the last attempt only.
// Whether an earlier attempt billed is a different question, answered by the
// spendAbandoned result of doJSONTracked and combined by the caller.
// billedVerdictCodes are the stable codes that mean the model ALREADY RAN and this is the
// verdict on what it produced. They are the one group whose spend is not in doubt.
//
// It overlaps nonRetriableAppCodes in retry.go and is deliberately a separate set: that
// one answers "would a replay reach the same answer", this one answers "was money spent",
// and the two questions have different memberships as soon as a code is added that is
// retriable but billed. `internal_error` is excluded for exactly that reason — it is the
// backend failing in a way that says nothing either way about whether a generation ran,
// so it belongs in the conservative fallthrough rather than being asserted as billed.
var billedVerdictCodes = map[string]bool{
	// A completion ran and its output did not satisfy the task's contract. The repair
	// pass this usually follows is billed too.
	"task_output_invalid": true,
}

func taskMayHaveBilled(err error) bool {
	var be *Error
	if !errors.As(err, &be) {
		// Not a backend error at all: a context cancellation or deadline on a request the
		// backend may well have accepted and be billing right now. Assume it billed —
		// over-caveating a total is recoverable, under-reporting a bill is not.
		return true
	}
	switch {
	case billedVerdictCodes[be.Code]:
		// FIRST, above every other arm including the status-shaped ones. These codes mean
		// the model demonstrably RAN and this is the verdict on its output, so the money
		// is already spent whatever status the envelope happens to be wearing.
		//
		// Ordering is the whole point. `task_output_invalid` at a 400, 401, 403 or 426
		// used to be answered by an arm below that reads the number — and every one of
		// those arms returns false. That is the dangerous direction: over-caveating an
		// accurate total is a cosmetic annoyance, while reporting a real charge as free
		// is a number the user cannot recover. A code that names spend must never be
		// overridable by a status, which is exactly what putting it last permitted.
		return true
	case be.IsConnect(), be.IsContract(), be.IsProtocolMismatch():
		return false
	case be.Code == CodeCredentialUnavailable:
		// Raised before dispatch: this process could not produce a credential, so no
		// request left the machine. Checked by CODE, because it carries no HTTP status
		// at all and the status arm below would miss it — leaving every locked-keychain
		// failure to permanently caveat the session total as a lower bound over spend
		// that provably never happened.
		return false
	case be.IsAccountCode(), be.Code == CodeInvalidAPIKey:
		// The backend refused at its own door, before any provider call.
		//
		// IsAccountCode is the WHOLE account union, deliberately, rather than the three
		// predicates (identity, subscription, dependency) this used to name one by one.
		// That enumeration silently omitted the two 429s — `usage_limit_reached` and
		// `account_rate_limited` — which are account verdicts like any other and equally
		// never reach a model call. The cost of the omission grew once a replaced attempt
		// could poison a total: a retried `account_rate_limited` followed by a perfectly
		// good answer reported that answer's exact cost as a floor, over spend that
		// provably never happened. Asking the union means a code added to it later is
		// classified correctly here without anyone remembering to come back.
		//
		// `invalid_api_key` — a malformed bearer, the one door code belonging to neither
		// taxonomy — is named beside it rather than left to the status arm below.
		return false
	case be.Code == CodeProviderInvalidAPIKey, be.Code == CodeProviderKeyForbidden,
		be.Code == CodeProviderInsufficientCredit, be.Code == CodeUpstreamNoCompliantProvider:
		// Refused before a single token was generated: a credential the provider does
		// not recognise, one it will not let use this model, an account with nothing
		// left to spend, or no endpoint to spend it at.
		//
		// The first two used to reach this answer ONLY through the status arm below —
		// they were caught as "a 401" and "a 403" rather than as themselves — which made
		// an honest total depend on a number that is not part of the contract.
		// `provider_invalid_api_key` is moving off 401 on the backend, and the same
		// envelope carries no status at all mid-stream; under either, the status arm
		// misses and the fallthrough caveats a total over a call the provider refused.
		//
		// It is the attempt in hand that generated nothing. Whether the backend billed
		// EARLIER work inside the same request — a completion followed by a repair pass
		// that then hit one of these — is not visible from here; the contract's position
		// is that a billed task failure surfaces as `task_output_invalid`, and this arm
		// rests on it.
		return false
	case be.HTTPStatus == http.StatusUnauthorized, be.HTTPStatus == http.StatusForbidden:
		// Orientation only, and LAST: an auth-shaped refusal whose code this build does
		// not recognise — an older or newer backend, or a proxy that reshaped the body —
		// is still a refusal at somebody's door, before any generation.
		//
		// Every code above is decided before it, so no recognised condition depends on
		// the number any more. What this arm cannot do is prove the negative: it is not
		// restricted to unrecognised codes, so a code that DOES mean spend would be read
		// as free if it ever arrived wearing a 401 or 403. Nothing in the current backend
		// mappings pairs one that way — which is a fact about today's server, not a
		// guarantee. The remedy if it ever fires wrongly is to give that code its own
		// arm above, never to add a number here.
		return false
	}
	return true
}

// reportCost delivers a CostEvent to the OnCost hook, guarded: an accounting side-channel
// must never be able to fail the call it is narrating.
func (c *Client) reportCost(ev CostEvent) {
	if c.onCost == nil {
		return
	}
	defer func() { _ = recover() }()
	c.onCost(ev)
}

// Capabilities fetches the backend's capability descriptor. Cache the result —
// refresh only on startup, reconnect, or /doctor.
func (c *Client) Capabilities(ctx context.Context) (Capabilities, error) {
	var out Capabilities
	if err := c.doJSON(ctx, http.MethodGet, "/v1/daintree/capabilities", nil, &out); err != nil {
		return Capabilities{}, err
	}
	return out, nil
}

// KeyVerification is the backend's verdict on the credential a request would ACTUALLY
// spend — its own upstream key on every install, since the CLI ships none. It is not a
// verdict on any bearer the caller supplied: that is an account token, and the backend
// never sends it upstream.
type KeyVerification struct {
	// Valid is the provider's answer. False means a definite rejection — not
	// "we couldn't check", which surfaces as an error from VerifyKey instead.
	Valid  bool   `json:"valid"`
	Detail string `json:"detail"`
	// Usable answers a DIFFERENT question from Valid: not "does the provider recognise
	// this credential" but "can the account behind it actually fund a turn". A key with
	// a spent balance is valid and unusable, and a client reading only Valid reports
	// health and then fails on the first real request with what looks like an unrelated
	// error.
	//
	// A POINTER because an older backend omits the field entirely, and a plain bool
	// would decode that absence as `false` — declaring every key on every older
	// deployment unusable. nil means "not reported"; fall back to LimitRemaining.
	Usable *bool `json:"usable"`
	// Reason is the stable machine-readable outcome — `ok`, `provider_rejected`,
	// `credits_exhausted`. Branch on this, never on Detail, which is prose.
	Reason string `json:"reason"`
	// Label is the provider's own name for that credential, when it exposes one — useful
	// for telling WHICH upstream key a deployment is spending, not merely that a working
	// one is configured.
	Label string `json:"label"`
	// LimitRemaining is credit left on the key when the provider reports it. A pointer
	// so "not reported" stays distinct from a genuine zero, which is worth warning about.
	LimitRemaining *float64 `json:"limit_remaining"`
	IsFreeTier     bool     `json:"is_free_tier"`
}

// ErrVerifyUnsupported reports a backend that does not serve the key-verification
// route at all — as opposed to serving it and answering. `doctor` FAILS on it for any
// REMOTE endpoint (an obsolete deployment or an intercepting proxy is a compatibility
// failure) and reports it as merely unknown for loopback, where a backend mid-change is
// routine. See AllowsUnverifiedSignIn.
var ErrVerifyUnsupported = errors.New("backend does not support key verification")

// verifyUnsupportedStatuses are the HTTP responses that mean "this deployment does not
// implement the route", as distinct from "the route ran and something went wrong".
//
// 404 is the obvious one. 405 and 501 matter just as much: the client issues the
// contractually correct POST, so a Method Not Allowed or Not Implemented answer is
// itself evidence that the required contract is absent or intercepted — and mapping
// them to an ordinary error would send them down the soft "could not confirm" branch,
// which reports as unknown rather than as the compatibility failure it is.
//
// Deliberately NOT included: transport failures and 5xx other than 501. Those mean
// "could not check", which must never be reported as a verdict about the credential.
var verifyUnsupportedStatuses = map[int]bool{
	http.StatusNotFound:         true,
	http.StatusMethodNotAllowed: true,
	http.StatusNotImplemented:   true,
}

// VerifyKey asks the backend whether the credential this request would spend actually
// works upstream. That is the backend's OWN provider credential, on every install: a
// bearer this request carries says who is calling and is never sent upstream, so setting
// one does not change which credential is verified here.
//
// This is the ONLY meaningful check available. /health and /readyz answer for the
// process, and /v1/daintree/capabilities answers 200 whether or not a turn could be
// funded, so without this call a dead upstream account is discovered on the first real
// turn. `doctor` is the caller.
//
// The CLI must never probe the provider itself: it holds no provider client by design
// (that is what keeps prompts, model choice, and credentials on the server), and the
// credential an account system issues is one only the backend can resolve.
func (c *Client) VerifyKey(ctx context.Context) (KeyVerification, error) {
	var out KeyVerification
	if err := c.doJSON(ctx, http.MethodPost, "/v1/daintree/auth/verify", struct{}{}, &out); err != nil {
		var berr *Error
		if errors.As(err, &berr) && verifyUnsupportedStatuses[berr.HTTPStatus] {
			return KeyVerification{}, ErrVerifyUnsupported
		}
		return KeyVerification{}, err
	}
	// Scrub the key out of the free-text fields BEFORE anyone can render them. Detail,
	// Label and Reason are backend-controlled strings that ride a 200 response — the
	// success path, which no error-scrubbing wrapper covers — and they land in the
	// doctor credential row and the debug log. A no-op when no caller key is set, which
	// is the normal case; when one IS set, a backend or proxy that echoes it back into
	// this response would otherwise persist it in the host's native scrollback, which the
	// attached session never clears. One choke point here beats N display-site fixes.
	//
	// Reason is in the list even though it is contractually a short enum member: it is
	// still a string this process did not author, doctor now renders an UNRECOGNISED
	// one verbatim so a newer backend can name a condition we have no copy for, and a
	// field whose value we do not control is a field that can carry a bearer.
	out.Detail = c.scrubSecrets(out.Detail)
	out.Label = c.scrubSecrets(out.Label)
	out.Reason = c.scrubSecrets(out.Reason)
	return out, nil
}

// Version fetches the unauthenticated /version descriptor.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	if err := c.doJSON(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return Version{}, err
	}
	return out, nil
}

// Health probes liveness. Returns nil when the backend reports ok.
//
// The path is /health, NOT /healthz. Both are served by the same handler, but only
// /health is routed on the deployed edge — /healthz there returns a Google 404 page
// from the load balancer, which the CLI would report as "backend UNREACHABLE" against
// a perfectly healthy backend (observed 2026-08-08; the backend repo hit the same trap
// in its release smoke test, assistant-backend 15264f1).
func (c *Client) Health(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/health", nil, &out); err != nil {
		return err
	}
	if out.Status != "ok" {
		return &Error{Code: "not_healthy", Message: "backend health: " + out.Status}
	}
	return nil
}

// Ready probes /readyz (readiness: config, secrets, prompts, catalog, provider).
// Returns nil only when the backend reports ready (a 503 surfaces as *Error).
func (c *Client) Ready(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/readyz", nil, &out); err != nil {
		return err
	}
	if out.Status != "ready" {
		msg := "backend not ready"
		if out.Error != "" {
			msg += ": " + out.Error
		}
		return &Error{HTTPStatus: 503, Code: "not_ready", Message: msg}
	}
	return nil
}

// jsonAttemptTimeout bounds ONE attempt at a non-streaming endpoint when the caller
// supplied no deadline of its own. It is per-attempt, not per-call: a utility task
// runs a whole server-side model call before responding, and that budget should not
// shrink because an earlier attempt died on a refused socket.
const jsonAttemptTimeout = 60 * time.Second

// doJSON performs a JSON request/response against a non-streaming endpoint, retrying
// transient failures under the client's RetryPolicy. A nil body sends no payload
// (GET); a non-2xx decodes the error envelope. out may be nil to discard the body.
//
// These calls are all replay-safe: the GETs are trivially idempotent, and a task POST
// is a stateless server-owned utility call (the CLI sends task DATA; the backend owns
// prompt, model, and schema). isRetriable already excludes the classes where a replay
// would either fail identically (auth, contract, protocol) or risk duplicating a
// server-side effect (500).
//
// The request body is marshaled ONCE and replayed from the same bytes — a fresh
// reader per attempt, since each is consumed.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	_, err := c.doJSONTracked(ctx, method, path, body, out)
	return err
}

// doJSONTracked is doJSON plus the one fact only the retry loop knows: whether an attempt
// it REPLACED may already have billed. Every endpoint that reports spend has to ask,
// because the answer cannot be recovered afterwards — a failed attempt reports no cost of
// its own, and the succeeding attempt's total covers only its own request (the backend
// aggregates re-rolls WITHIN a request, never across separate HTTP attempts). So a
// replayed call's reported figure is a floor, and the callers that report money say so.
//
// The plain doJSON exists for the endpoints that spend nothing — capabilities, health,
// account status — where the answer is real but there is no total for it to qualify.
func (c *Client) doJSONTracked(ctx context.Context, method, path string, body any, out any) (bool, error) {
	// One scrub point for every JSON endpoint. readErrorResponse already scrubs the
	// HTTP-error path (so the retry hook sees a clean error too); this additionally
	// covers marshal/decode errors, whose text can echo the payload. See scrubError.
	spendAbandoned, err := c.doJSONRetry(ctx, method, path, body, out)
	return spendAbandoned, c.scrubError(err)
}

// The bool result is the abandoned-spend answer doJSONTracked documents; it is returned
// beside BOTH outcomes, because a call that eventually succeeds hides the same money as
// one that eventually fails.
func (c *Client) doJSONRetry(ctx context.Context, method, path string, body any, out any) (spendAbandoned bool, err error) {
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return false, fmt.Errorf("backend: marshal %s: %w", path, err)
		}
		payload = b
	}

	// Sample the credential this call is about BEFORE the first attempt. It is fetched
	// again per attempt inside setHeaders (so a refresh underneath is picked up); this
	// copy exists only to attribute the OUTCOME to the right credential.
	//
	// A failure here is not fatal to the request: the per-attempt fetch will raise the
	// same problem, with the error shape the caller already handles. All that is lost is
	// the observation, and reporting a verdict against a credential we could not read
	// would be worse than not reporting one.
	sampled, _ := c.credential(ctx, path)
	acct := c.beginAccountAttempt(path, sampled)
	refreshReplayed := false

	started := time.Now()
	for attempt := 0; ; attempt++ {
		err = c.doJSONOnce(ctx, method, path, payload, body != nil, out)
		if err == nil {
			acct.succeeded()
			// Success does not retire the history: an attempt this one replaced may
			// still have billed, and only the caller knows what to qualify with it.
			return spendAbandoned, nil
		}
		// Caller cancellation (or an exhausted caller-supplied deadline) is a clean
		// stop, never a retry — but the answer that arrived before it is still an
		// answer. A revocation that raced an Escape keypress would otherwise be
		// discarded, leaving the dead credential on disk and the state machine
		// believing in a session the backend has already ended.
		if ctx.Err() != nil {
			acct.failed(ctx, err)
			return spendAbandoned, err
		}
		// The auth ladder, above the transport retry and deliberately separate from it.
		// isRetriable refuses every identity code, so without this an expired token is a
		// hard failure on a call a single refresh would have completed. It is NOT
		// governed by the transient budget either: a caller that set MaxAttempts to 1
		// asked not to replay a flaky endpoint, which is a different request from
		// refusing to renew an expired credential once.
		//
		// The verdict goes to the observer FIRST — that is what drops the dead token, so
		// the replay's setHeaders fetches a fresh one instead of re-presenting the value
		// the backend just refused. ONE replay: a second would mean the refresh did not
		// help, and looping on that is how a client hammers an endpoint that will keep
		// saying no.
		if !refreshReplayed && wantsRefreshReplay(err) {
			refreshReplayed = true
			acct.failed(ctx, err)
			next, ok := c.renewedCredential(ctx, path, acct.token)
			if !ok {
				// No fresh credential, so there is nothing to replay WITH. Replaying
				// anyway would re-present the token the backend just refused, or — if
				// the refresh failed because the session is gone — send the request
				// ANONYMOUSLY, which on an open backend succeeds as the wrong principal
				// and reports a confirmed session that no longer exists.
				return spendAbandoned, err
			}
			// The outcome of the replay belongs to the NEW token, not the one that just
			// failed.
			acct = c.beginAccountAttempt(path, next)
			continue
		}
		be, ok := err.(*Error)
		if !ok || !isRetriable(be) || attempt+1 >= c.retry.MaxAttempts || retriesDisabled(ctx) {
			acct.failed(ctx, err)
			return spendAbandoned, err
		}
		delay := c.retry.backoff(attempt, be.RetryAfter)
		// Attempts are not free here either: a slow-failing endpoint can spend most
		// of jsonAttemptTimeout before each rejection, so the attempt count alone
		// would let a deadline-less caller stall for many minutes.
		if c.retry.exhausted(time.Since(started), delay) {
			// Observed on the way out like every other terminal exit. These are
			// retriable-class errors, which sounds like nothing for the account layer
			// — but the three dependency codes are BOTH retriable and account verdicts,
			// and they are the ones that must reach the state machine as "we could not
			// check" so it preserves the credential instead of inferring a logout.
			acct.failed(ctx, err)
			return spendAbandoned, err
		}
		if c.onRetry != nil {
			c.onRetry(RetryInfo{
				Attempt:     attempt,
				MaxAttempts: c.retry.MaxAttempts,
				Delay:       delay,
				Op:          method + " " + path,
				Err:         be,
			})
		}
		if !sleepCtx(ctx, delay) {
			acct.failed(ctx, err)
			return spendAbandoned, err
		}
		// The attempt is now genuinely being REPLACED, so ask about it the same question
		// the caller will ask about the final error: could it have billed? A retriable
		// 503 or 504 is exactly the case where the backend may have paid for a generation
		// and then failed to deliver it. Recorded here rather than at the retry decision
		// because the two exits above return THIS error, which answers for itself.
		if taskMayHaveBilled(err) {
			spendAbandoned = true
		}
	}
}

// doJSONOnce performs a single JSON attempt. hasBody distinguishes a nil body (GET,
// no payload) from a body that marshaled to an empty-but-present document.
func (c *Client) doJSONOnce(ctx context.Context, method, path string, payload []byte, hasBody bool, out any) error {
	// Bound the attempt so a wedged backend can't hang a turn; the streamed respond
	// path does NOT route through here. A caller-supplied deadline always wins — it
	// is the budget for the WHOLE call, retries included.
	attemptCtx := ctx
	callerBounded := true
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		callerBounded = false
		var cancel context.CancelFunc
		attemptCtx, cancel = context.WithTimeout(ctx, jsonAttemptTimeout)
		defer cancel()
	}

	err := c.doJSONAttempt(attemptCtx, method, path, payload, hasBody, out)
	// Our OWN attempt timeout firing while the parent is still live means the backend
	// accepted the request and simply took too long — a slow task, not a broken hop.
	// Replaying would burn the same minute again, so it is terminal (code "timeout",
	// deliberately not the retriable "connect").
	//
	// This wraps the WHOLE attempt, not just the round trip to first byte. A backend
	// that flushes error headers and then stalls its body would otherwise surface as
	// the retriable status it had already sent (a 503 whose truncated body read is
	// discarded), and get replayed for the full attempt budget.
	if err != nil && !callerBounded && attemptCtx.Err() != nil && ctx.Err() == nil {
		return &Error{Code: "timeout", Message: fmt.Sprintf("backend did not answer %s within %s", path, jsonAttemptTimeout)}
	}
	return err
}

// maxSmallJSONResponseBytes bounds a health/capability/version/auth-verify reply
// — small, fixed-shape status JSON. Generous relative to any legitimate response;
// its job is to cap a misconfigured or compromised backend/proxy's forced
// allocation, not to be a tight budget.
const maxSmallJSONResponseBytes = 1 << 20 // 1 MiB

// maxTaskJSONResponseBytes bounds a non-streaming /respond or /tasks reply, which
// can legitimately carry a model's answer or a utility task's output text —
// larger than a status response, but still finite.
const maxTaskJSONResponseBytes = 16 << 20 // 16 MiB

// jsonResponseLimit maps an endpoint path to its response size bound. Defaults to
// the SMALL limit: a new endpoint that turns out to carry real content earns the
// larger one explicitly, rather than every future endpoint silently inheriting an
// unbounded read by omission.
func jsonResponseLimit(path string) int64 {
	switch path {
	case "/v1/daintree/respond", "/v1/daintree/tasks":
		return maxTaskJSONResponseBytes
	default:
		return maxSmallJSONResponseBytes
	}
}

// doJSONAttempt is one request/response round trip under an already-bounded context.
func (c *Client) doJSONAttempt(attemptCtx context.Context, method, path string, payload []byte, hasBody bool, out any) error {
	var reader io.Reader
	if hasBody {
		reader = bytes.NewReader(payload)
	}

	req, err := http.NewRequestWithContext(attemptCtx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("backend: build %s request: %w", path, err)
	}
	if err := c.setHeaders(attemptCtx, req, "application/json", path); err != nil {
		return err
	}

	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return transportError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.readErrorResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	// Unlike readErrorResponse's io.ReadAll(io.LimitReader(...)), a successful body
	// was decoded directly from resp.Body with no size bound at all — a
	// misconfigured custom backend or compromised proxy could force an unbounded
	// allocation on a normal 2xx response.
	//
	// Read into a bounded buffer FIRST rather than decoding straight off a capped
	// reader: a live *io.LimitedReader can't distinguish "the single JSON value
	// legitimately needed every byte of the cap" from "the value itself is over the
	// limit" once Decode has already succeeded, so an exact multiple of (limit+1)
	// bytes of otherwise-valid JSON would silently pass. A length check on the
	// fully-read buffer has no such ambiguity.
	limit := jsonResponseLimit(path)
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return &Error{HTTPStatus: resp.StatusCode, Code: "read", Message: "could not read backend response: " + err.Error()}
	}
	if int64(len(data)) > limit {
		return &Error{HTTPStatus: resp.StatusCode, Code: "response_too_large",
			Message: fmt.Sprintf("backend response for %s exceeded %d bytes", path, limit)}
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(out); err != nil {
		return &Error{HTTPStatus: resp.StatusCode, Code: "decode", Message: "could not decode backend response: " + err.Error()}
	}
	// One JSON document, not two silently concatenated. NOT dec.More(): its
	// contract is "another element in the CURRENT array/object", not "another
	// top-level document" — it can misreport on a stray trailing `}`/`]` (treating
	// it as the close of an enclosing structure that was never open at this level).
	// Attempting a second decode and requiring io.EOF is the correct idiom here,
	// and — because `data` is now a fixed in-memory buffer, not a live network
	// stream — unambiguous: anything other than io.EOF means real trailing bytes.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return &Error{HTTPStatus: resp.StatusCode, Code: "trailing_json",
			Message: fmt.Sprintf("backend response for %s carried more than one JSON document", path)}
	}
	return nil
}

// readErrorResponse decodes a non-2xx response into an *Error, preferring the
// stable Daintree error envelope and falling back to the raw body.
//
// The result is scrubbed before it leaves: the body is attacker- or upstream-controlled
// text that lands in terminal scrollback, the debug log, and the retry-observability
// hook. See scrubError.
func (c *Client) readErrorResponse(resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	requestID := safeRequestID(resp.Header.Get(requestIDHeader))
	var env Envelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Error.Code != "" || env.Error.Message != "" || env.Error.Type != "") {
		e := newError(resp.StatusCode, env, retryAfter, false)
		e.RequestID = requestID
		return c.scrubBackendError(e)
	}
	e := httpError(resp.StatusCode, string(raw))
	e.RetryAfter = retryAfter
	e.RequestID = requestID
	return c.scrubBackendError(e)
}

// requestIDHeader is the backend's per-request correlation id. It is the only handle a
// user has on a failure whose real detail lives in the server's log — which is exactly
// the situation for the codes that mean a bug to report rather than an account or
// policy problem the caller could fix themselves.
const requestIDHeader = "X-Request-Id"

// maxRequestIDLen bounds what we will echo. Real ids are ~20 characters.
const maxRequestIDLen = 128

// safeRequestID accepts only what a correlation id can legitimately be.
//
// The value is a header from whatever answered the request — for a custom endpoint or
// an intercepting proxy, not necessarily our backend — and it is rendered straight into
// terminal scrollback by the "report this with request id X" advice. The attached session draws
// on the NORMAL screen buffer, so an ANSI escape smuggled through here would still be
// repainting the user's terminal long after the session ended. Anything outside the
// conservative id alphabet is dropped whole rather than sanitised: a mangled id is
// useless for correlation anyway, and the advice reads fine without one.
func safeRequestID(v string) string {
	if v == "" || len(v) > maxRequestIDLen {
		return ""
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '-', r == '_', r == '.', r == ':':
		default:
			return ""
		}
	}
	return v
}

// scrubError removes the API key from any error on its way out of the client.
//
// The bearer token is a live credential whichever form it takes — the account access
// token a signed-in install sends, or the caller key the rare install sets — and while
// it never travels to the provider, everything between here and the backend can echo the
// Authorization header back into an error body: the backend itself, a custom endpoint, an
// intercepting proxy's error page, a terminal SSE `error` event. Those become
// Error.Message and flow straight to surfaces that persist them: the attached session renders on
// the NORMAL screen buffer, so a leak stays in the host's native scrollback long after
// the session, and the same text is appended to the 0600 debug log.
//
// Scrubbing at the client boundary rather than at each display site is deliberate:
// there are many sinks (turn error rendering, doctor rows, login messages, the trace
// writer, the retry hook) and exactly one place all of them get their errors from.
// This is defense-in-depth, not a substitute for the backend not leaking — but custom
// endpoints and proxies are outside our control, so the client cannot assume good
// behaviour upstream.
func (c *Client) scrubError(err error) error {
	var be *Error
	if err == nil || !errors.As(err, &be) {
		// Not a structured backend error: scrub the flat message. A wrapped
		// fmt.Errorf can still carry a decoder's echo of the payload.
		if err == nil {
			return err
		}
		if scrubbed := c.scrubSecrets(err.Error()); scrubbed != err.Error() {
			return errors.New(scrubbed)
		}
		return err
	}
	return c.scrubBackendError(be)
}

// scrubBackendError scrubs the free-text fields of a structured *Error in place. Code
// and Type are stable machine identifiers, but they are backend-controlled strings, so
// they are scrubbed too rather than trusted to be well-behaved.
func (c *Client) scrubBackendError(e *Error) *Error {
	if e == nil {
		return e
	}
	e.Message = c.scrubSecrets(e.Message)
	e.Param = c.scrubSecrets(e.Param)
	e.Code = c.scrubSecrets(e.Code)
	e.Type = c.scrubSecrets(e.Type)
	// RequestID is header text from whatever answered — which for a custom endpoint or
	// an intervening proxy is not necessarily the backend. It is rendered verbatim into
	// terminal scrollback and the debug log by the "report this bug" advice, so it gets
	// the same treatment as every other field we did not author.
	e.RequestID = c.scrubSecrets(e.RequestID)
	return e
}
