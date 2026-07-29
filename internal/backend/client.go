package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
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
	apiKey  string
	// http serves the streamed respond POST; jsonHTTP serves everything routed
	// through doJSON. They differ only in ResponseHeaderTimeout (see NewClient).
	http     *http.Client
	jsonHTTP *http.Client
	info     ClientInfo
	retry    RetryPolicy
	onRetry  func(RetryInfo)
	onTask   func(TaskTraceInfo)
	// streamIdleTimeout overrides sseIdleTimeout for the respond stream's idle
	// watchdog. Zero selects the default; tests shrink it to exercise the abort.
	streamIdleTimeout time.Duration
}

// ClientConfig configures a Client. APIKey is OPTIONAL — local development runs
// the backend unauthenticated (no DAINTREE_API_KEY), so an empty key sends no
// Authorization header. HTTPClient defaults to one with NO global timeout (a
// streamed turn can run for minutes; cancellation is via context).
type ClientConfig struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	ClientInfo ClientInfo
	// Retry tunes transient-failure retries for the streamed respond turn. The zero
	// value selects DefaultRetryPolicy (initial attempt + 1 backoff retry — the
	// backend owns provider retries; this covers only the CLI↔backend hop). Set
	// Retry.MaxAttempts to 1 to disable retries.
	Retry RetryPolicy
	// OnRetry, if set, is invoked just before each backoff sleep when a transient
	// respond failure will be retried (observability only — it must not block).
	OnRetry func(RetryInfo)
	// OnTask, if set, is invoked after every RunTask round trip (success or failure).
	// Observability only — it must not block. Without it the utility tasks are the
	// one backend surface a session log cannot see: a /compact's checkpoint +
	// memory_distill calls (and every watcher classify/judge/extract) would leave no
	// trace at all.
	OnTask func(TaskTraceInfo)
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
	// response headers. The backend commits the SSE response as soon as skill
	// selection completes (~1.5–2.5s), well before the upstream model produces
	// anything, so 10s is generous headroom without letting a wedged backend pin a
	// turn. doJSON's client deliberately has NO header timeout — a utility task runs
	// its whole model call before responding, which routinely exceeds 10s.
	streamResponseHeaderTimeout = 10 * time.Second
)

// newTransport builds the structured default transport: bounded dial + TLS
// handshake, keep-alives on (connection reuse matters — every turn round-trips),
// and an optional response-header bound (0 = none).
func newTransport(responseHeaderTimeout time.Duration) *http.Transport {
	return &http.Transport{
		Proxy: http.ProxyFromEnvironment,
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
		hc = &http.Client{Transport: newTransport(streamResponseHeaderTimeout)}
		jc = &http.Client{Transport: newTransport(0)}
	}
	retry := cfg.Retry
	if retry.MaxAttempts == 0 {
		retry = DefaultRetryPolicy()
	}
	return &Client{
		baseURL:  base,
		apiKey:   strings.TrimSpace(cfg.APIKey),
		http:     hc,
		jsonHTTP: jc,
		info:     cfg.ClientInfo,
		retry:    retry,
		onRetry:  cfg.OnRetry,
		onTask:   cfg.OnTask,
	}
}

// BaseURL returns the configured backend base URL (for diagnostics / doctor).
func (c *Client) BaseURL() string { return c.baseURL }

// setHeaders applies the common headers. The Authorization header is only set
// when an API key is configured (local dev runs without one).
func (c *Client) setHeaders(req *http.Request, accept string) {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", accept)
	req.Header.Set("X-Daintree-Protocol", fmt.Sprintf("%d", ProtocolVersion))
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
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

	body, err := json.Marshal(req)
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: marshal respond request: %w", err)
	}

	// Retries must not double-fire side-effecting callbacks.
	//
	//   - OnRawMeta is intentionally observational and remains per-attempt. It may
	//     fire more than once so latency/debug instrumentation sees the real transport
	//     timeline; it must never adopt state or produce user-visible effects.
	//   - OnSkillLoaded is intentionally EAGER: a successful selector result is useful
	//     feedback before the upstream model connects or emits a token. The same request
	//     can be retried after receiving meta, so identical skill refs are de-duplicated
	//     across attempts before reaching the caller. A failed attempt's signed state is
	//     also adopted into the next POST so the backend reuses that selection instead of
	//     selecting a different skill after the first one is already visible.
	//   - OnContent is the retry BOUNDARY: once any visible token has reached the
	//     caller the turn is committed (a replay would duplicate on-screen text), so we
	//     only retry failures that arrive before the first content fragment.
	//   - OnMeta carries committed state/version metadata. Firing it once per attempt
	//     would advance state from a doomed attempt, so we CAPTURE each attempt's meta
	//     and forward it exactly once — from the attempt that commits (first content) or
	//     when the loop returns. If the terminal/cancelled attempt dies before meta, the
	//     last received (and retry-adopted) meta is forwarded so its signed state is not
	//     lost after its eager skill cue was already shown.
	//
	// (OnToolCallDelta is intentionally NOT a boundary — see its doc on StreamCallbacks:
	// callers must treat the returned ToolCalls as authoritative, since a pre-content
	// failure that already streamed tool-call fragments is still replayed.)
	contentStreamed := false
	metaForwarded := false
	var pendingMeta *StreamMeta
	var lastReceivedMeta *StreamMeta
	userOnMeta := cb.OnMeta
	userOnSkillLoaded := cb.OnSkillLoaded
	userOnContent := cb.OnContent
	seenSkillLoads := make(map[string]struct{})

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
	if userOnSkillLoaded != nil {
		cb.OnSkillLoaded = func(refs []SkillRef) {
			unseen := make([]SkillRef, 0, len(refs))
			for _, ref := range refs {
				// The id is the stable skill identity. Fall back to the display title for
				// malformed refs, but do not let harmless title drift on a retry produce
				// a duplicate card for the same id.
				key := strings.TrimSpace(ref.ID)
				if key == "" {
					key = strings.TrimSpace(ref.Title)
				}
				if key == "" {
					continue
				}
				if _, ok := seenSkillLoads[key]; ok {
					continue
				}
				seenSkillLoads[key] = struct{}{}
				unseen = append(unseen, ref)
			}
			if len(unseen) > 0 {
				userOnSkillLoaded(unseen)
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

	for attempt := 0; ; attempt++ {
		pendingMeta = nil // each attempt brings its own meta
		result, serr := c.respondStreamOnce(ctx, body, cb)
		if serr == nil {
			flushMeta() // committed success (incl. pure tool-call turns with no content)
			return result, nil
		}
		// Caller cancellation (Escape / shutdown) is a clean stop, never a retry.
		if ctx.Err() != nil {
			flushMeta()
			return result, serr
		}
		be, ok := serr.(*Error)
		// Stop if content already streamed (can't replay), the failure isn't a
		// transient class, retries are disabled, or the attempt budget is spent.
		if contentStreamed || !ok || !isRetriable(be) || attempt+1 >= c.retry.MaxAttempts {
			flushMeta()
			return result, serr
		}
		// Selection already completed if this attempt delivered meta. Pin that signed
		// outcome into the next request before replaying the turn: the eager skill card
		// is now visible, so a full-request retry must reuse the same active set rather
		// than rerun a selector that could choose something different. OnMeta remains
		// deferred; adopting the opaque token here is transport bookkeeping only.
		if pendingMeta != nil && pendingMeta.State != "" {
			state := pendingMeta.State
			req.State = &state
			nextBody, marshalErr := json.Marshal(req)
			if marshalErr != nil {
				flushMeta()
				return result, fmt.Errorf("backend: marshal retry request: %w", marshalErr)
			}
			body = nextBody
		}
		// Retrying: this attempt's meta is discarded (reset at the loop top).
		delay := c.retry.backoff(attempt, be.RetryAfter)
		if c.onRetry != nil {
			c.onRetry(RetryInfo{Attempt: attempt, Delay: delay, Err: be})
		}
		if !sleepCtx(ctx, delay) {
			// Cancelled mid-backoff: surface the last failure (the caller reads
			// ctx.Err() and treats it as a clean cancel).
			flushMeta()
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

	httpReq, err := http.NewRequestWithContext(attemptCtx, http.MethodPost, c.baseURL+"/v1/daintree/respond", bytes.NewReader(body))
	if err != nil {
		return RespondResult{}, fmt.Errorf("backend: build respond request: %w", err)
	}
	c.setHeaders(httpReq, "text/event-stream")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return RespondResult{}, &Error{Code: "connect", Message: "could not reach assistant backend: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return RespondResult{}, c.readErrorResponse(resp)
	}

	idle := c.streamIdleTimeout
	if idle <= 0 {
		idle = sseIdleTimeout
	}
	watchdog := newIdleWatchdog(idle, cancelAttempt)
	defer watchdog.Stop()

	result, perr := parseRespondStream(&idleResetReader{r: resp.Body, reset: watchdog.Reset}, cb)
	if perr != nil && watchdog.Fired() && ctx.Err() == nil {
		return result, &Error{
			Code:    "stream_idle_timeout",
			Message: fmt.Sprintf("stream idle: no bytes from the backend for %s", idle),
			Stream:  true,
		}
	}
	return result, perr
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
	var out RespondResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/daintree/respond", req, &out); err != nil {
		return RespondResponse{}, err
	}
	return out, nil
}

// RunTask runs a server-owned utility task against /v1/daintree/tasks. The CLI
// sends task DATA only; the backend owns the prompt, model, schema, and output
// mode. Decode TaskResult.Output into the task-specific output struct.
func (c *Client) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	start := time.Now()
	var out TaskResult
	err := c.doJSON(ctx, http.MethodPost, "/v1/daintree/tasks", req, &out)
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
		return TaskResult{}, err
	}
	return out, nil
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

// Version fetches the unauthenticated /version descriptor.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var out Version
	if err := c.doJSON(ctx, http.MethodGet, "/version", nil, &out); err != nil {
		return Version{}, err
	}
	return out, nil
}

// Health probes /healthz (liveness). Returns nil when the backend reports ok.
func (c *Client) Health(ctx context.Context) error {
	var out struct {
		Status string `json:"status"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "/healthz", nil, &out); err != nil {
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

// doJSON performs a JSON request/response with a default short deadline for the
// non-streaming endpoints. A nil body sends no payload (GET); a non-2xx decodes the
// error envelope. out may be nil to discard the body.
func (c *Client) doJSON(ctx context.Context, method, path string, body any, out any) error {
	// Bound the non-streaming calls so a wedged backend can't hang a turn; the
	// streamed respond path does NOT route through here.
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("backend: marshal %s: %w", path, err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("backend: build %s request: %w", path, err)
	}
	c.setHeaders(req, "application/json")

	resp, err := c.jsonHTTP.Do(req)
	if err != nil {
		return &Error{Code: "connect", Message: "could not reach assistant backend: " + err.Error()}
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return c.readErrorResponse(resp)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(out); err != nil {
		return &Error{HTTPStatus: resp.StatusCode, Code: "decode", Message: "could not decode backend response: " + err.Error()}
	}
	return nil
}

// readErrorResponse decodes a non-2xx response into an *Error, preferring the
// stable Daintree error envelope and falling back to the raw body.
func (c *Client) readErrorResponse(resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
	var env Envelope
	if err := json.Unmarshal(raw, &env); err == nil && (env.Error.Code != "" || env.Error.Message != "" || env.Error.Type != "") {
		return newError(resp.StatusCode, env, retryAfter, false)
	}
	e := httpError(resp.StatusCode, string(raw))
	e.RetryAfter = retryAfter
	return e
}
