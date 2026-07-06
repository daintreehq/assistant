// Package mcp connects the assistant to Daintree's local MCP server and exposes a
// small, degradation-tolerant API.
//
// Transport: Streamable HTTP primary, legacy SSE fallback, both with a
// `Authorization: Bearer <token>` header. The client NEVER throws on
// construct/connect — it records lastError and reports connected:false; tools that
// need Daintree then fail cleanly with MCP_UNAVAILABLE. It caches the tool list
// (warmed once on connect) and warns on documented-vs-live drift.
package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP client wire identity — must stay byte-stable (Daintree keys off it).
const (
	clientName    = "daintree-assistant-cli"
	clientVersion = "0.1.0"
)

// Transport kinds (exact value set for Status.Transport).
const (
	transportNone           = "none"
	transportInjected       = "injected"
	transportStreamableHTTP = "streamable-http"
	transportSSE            = "sse"
)

// defaultCallTimeout backstops a single CallTool when the caller sets no Timeout (the
// main-turn tool adapters pass CallOptions{}). Without it, a Daintree server that accepts
// the request but never responds would block the turn INDEFINITELY (the user could still
// Esc, but an un-cancelled turn would hang forever). MCP RPCs to Daintree are request/
// response — long work runs async — so this is deliberately generous; the daemon poll path
// sets its own, shorter McpReadTimeoutMS, which still takes precedence.
const defaultCallTimeout = 120 * time.Second

// resourceUpdateBuffer bounds the resource-update wake-signal channel. The SDK
// handler runs on the JSON-RPC receive loop and forwards a dirty URI with a
// non-blocking send, dropping it when the buffer is full (the periodic reconcile
// tick is the backstop). Sized generously so a burst of transitions is rarely lost.
const resourceUpdateBuffer = 64

// sseRewriteRe rewrites a trailing /mcp or /mcp/ to /sse for the SSE fallback. A
// path that does not end in /mcp is left unchanged.
var sseRewriteRe = regexp.MustCompile(`/mcp/?$`)

// ServerInfo is the server's reported implementation info.
type ServerInfo struct {
	Name    string
	Version string
}

// CallResult is the normalized tool-call response (McpCallResult).
type CallResult struct {
	Text              string // flattened text of all text blocks, joined by "\n"
	Content           []any  // raw content blocks verbatim (default [])
	StructuredContent any    // optional; verbatim
	IsError           bool   // Boolean(res.isError); default false
}

// CallOptions are caller-facing knobs for one CallTool.
type CallOptions struct {
	Timeout time.Duration // 0 = unset; per-request timeout
	Retries int           // clamped >=0; READ-ONLY callers only (default 0 → no retry)
}

// Status is a snapshot of the client. Empty drift arrays collapse to nil (never
// []), and the slices/ServerInfo are defensive copies.
type Status struct {
	Connected      bool
	URL            string
	Transport      string
	ToolCount      *int // nil when cache cold
	Error          string
	DriftWarnings  []string // nil when none
	DriftToolNames []string // nil when none; index-aligned with DriftWarnings
	ServerInfo     *ServerInfo
}

// Options configures Client construction. clientOverride injects a pre-built,
// already-connected low-level client for tests (no network).
type Options struct {
	ClientOverride LowLevelClient

	// URL overrides the connection endpoint. nil ⇒ cfg.McpURL (the primary Daintree
	// control-plane MCP). Set (with Anonymous + DriftBaseline) for a SECOND server like
	// the docs MCP, whose URL is a fixed product constant rather than a Daintree env var.
	URL *string
	// Anonymous connects WITHOUT a bearer token — for a public, no-auth server (the docs
	// MCP). When true, no Authorization header is sent and an empty token is NOT treated
	// as a "DAINTREE_MCP_TOKEN not set" misconfiguration.
	Anonymous bool
	// DriftBaseline overrides the documented-tool drift baseline. nil ⇒
	// DocumentedMcpToolNames (the 60 Daintree tools). A second server passes its OWN
	// short baseline (e.g. DocsDocumentedToolNames) so it never false-warns that the
	// Daintree tools are missing from it.
	DriftBaseline []string
}

// Client is the high-level DaintreeMcpClient. All mutable state is guarded by mu
// because callers (UI hook, daemon ticks, doctor) may invoke concurrently.
type Client struct {
	cfg config.AppConfig

	// endpoint, anonymous, and driftBaseline are resolved ONCE in New (immutable
	// after, so they need no lock). endpoint is the connection URL (cfg.McpURL unless
	// Options.URL overrides it); anonymous suppresses the bearer header for a no-auth
	// server; driftBaseline is the documented-tool set the warmToolCache drift check
	// compares against (DocumentedMcpToolNames for the primary client, a server-specific
	// list for a second server like the docs MCP).
	endpoint      string
	anonymous     bool
	driftBaseline []string

	// sdkClient is the SDK client built ONCE in New with the resource-updated
	// handler wired in. The handler must be present BEFORE Connect (it cannot be
	// attached to an existing client) and is set on the *Client, so it survives
	// reconnects — only the subscriptions themselves are re-issued per new session.
	// Immutable after New, so it needs no lock. nil only for an injected test client.
	sdkClient *sdkmcp.Client
	// resourceUpdates is the buffered wake-signal channel the SDK handler forwards
	// dirty resource URIs onto. Immutable after New (read by the daemon loop).
	resourceUpdates chan string
	// gov is the per-client request governor (see governor.go): a small in-flight
	// cap plus paced starts, so no combination of uncoordinated callers (daemon
	// ticks, async coordinator, in-turn polls, UI previews) can burst-hammer the
	// server. Immutable after New.
	gov *governor

	mu             sync.Mutex
	low            LowLevelClient // active low-level client (nil when disconnected)
	generation     uint64         // bumped whenever c.low is installed/detached; identity tag for in-flight calls
	connected      bool
	transportKind  string
	lastError      string
	toolCache      []ToolInfo // nil = cold
	cacheWarm      bool       // distinguishes nil-empty-cache from cold
	driftWarnings  []string
	driftToolNames []string
	serverInfo     *ServerInfo
	// subs reference-counts the resource URIs this client holds a subscription for,
	// keyed URI→local-subscriber-count. Guarded by mu. The server holds ONE
	// subscription per (session, URI), but two watchers can watch the same terminal,
	// so only the first local subscriber issues the wire Subscribe and only the last
	// withdrawal issues the wire Unsubscribe — otherwise one watcher stopping would
	// silently kill the other's push path. A subscription is SESSION-scoped (lost
	// when the session is replaced), so applyConnected re-issues every live URI on
	// each new session, sparing the watcher any cross-reconnect re-subscribe.
	subs map[string]int
}

// closeLowLevel closes a detached low-level client off the hot path, swallowing
// errors. nil is a no-op, so it is always safe to call (idempotent). Kept a free
// helper so it can run OUTSIDE c.mu — closing a Streamable-HTTP/SSE session can
// block on the network and must never be held under the client lock.
func closeLowLevel(low LowLevelClient) {
	if low != nil {
		_ = low.Close()
	}
}

// New constructs a Client. If opts.ClientOverride is set the client is connected
// from construction with transport "injected" — but the cache is NOT warmed here
// (Connect warms it once).
func New(cfg config.AppConfig, opts Options) *Client {
	// Resolve the connection identity once. URL/Anonymous/DriftBaseline let a SECOND
	// instance target a different server (the docs MCP) without reading the Daintree env.
	endpoint := cfg.McpURL
	if opts.URL != nil {
		endpoint = *opts.URL
	}
	driftBaseline := opts.DriftBaseline
	if driftBaseline == nil {
		driftBaseline = DocumentedMcpToolNames
	}
	c := &Client{
		cfg:             cfg,
		endpoint:        endpoint,
		anonymous:       opts.Anonymous,
		driftBaseline:   driftBaseline,
		transportKind:   transportNone,
		resourceUpdates: make(chan string, resourceUpdateBuffer),
		gov:             newGovernor(governorMaxConcurrent, governorMinInterval),
		subs:            map[string]int{},
	}
	// Build the SDK client once, with the resource-updated handler wired in before
	// any Connect. An injected override never connects through it, but constructing
	// it is pure (no I/O), so we always do — keeping the reconnect path uniform.
	c.sdkClient = sdkmcp.NewClient(
		&sdkmcp.Implementation{Name: clientName, Version: clientVersion},
		&sdkmcp.ClientOptions{ResourceUpdatedHandler: c.onResourceUpdated},
	)
	if opts.ClientOverride != nil {
		c.low = opts.ClientOverride
		c.connected = true
		c.transportKind = transportInjected
	}
	return c
}

// onResourceUpdated is the SDK ResourceUpdatedHandler. It runs SYNCHRONOUSLY on the
// JSON-RPC receive loop, so it must NEVER block — calling Subscribe/ReadResource
// here would deadlock the session. It only forwards the dirty resource URI as a
// wake signal via a non-blocking send; the daemon does the actual read off-loop.
// The notification carries no payload (just "this URI changed"). A full buffer
// drops the signal, which is safe: the periodic reconcile tick still catches up.
func (c *Client) onResourceUpdated(_ context.Context, req *sdkmcp.ResourceUpdatedNotificationRequest) {
	if req == nil || req.Params == nil {
		return
	}
	select {
	case c.resourceUpdates <- req.Params.URI:
	default:
	}
}

// ResourceUpdates exposes the wake-signal channel: each value is a resource URI the
// server reported as changed for a subscription this client holds. The daemon loop
// selects on it to trigger an immediate watcher re-check instead of waiting a tick.
func (c *Client) ResourceUpdates() <-chan string { return c.resourceUpdates }

// IsConnected reports the connection flag.
func (c *Client) IsConnected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected
}

// HasGrantSupport is observational only — never opens a connection, never an auth
// gate. false when disconnected; else delegates to the pure predicate over the
// (possibly empty) cache. With GrantToolNames empty today, always false.
func (c *Client) HasGrantSupport() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected {
		return false
	}
	return ToolsAdvertiseGrantSupport(c.toolCache, GrantToolNames)
}

// Status returns a snapshot with defensive copies and empty→nil drift collapse.
func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.statusLocked()
}

func (c *Client) statusLocked() Status {
	s := Status{
		Connected: c.connected,
		// Report the ACTUAL connection endpoint (c.endpoint), not cfg.McpURL — for the
		// primary client they're identical, but a second server (docs) overrides the URL,
		// so this keeps Status honest for diagnostics on either client.
		URL:       c.endpoint,
		Transport: c.transportKind,
		Error:     c.lastError,
	}
	if c.cacheWarm {
		n := len(c.toolCache)
		s.ToolCount = &n
	}
	if len(c.driftWarnings) > 0 {
		s.DriftWarnings = append([]string(nil), c.driftWarnings...)
	}
	if len(c.driftToolNames) > 0 {
		s.DriftToolNames = append([]string(nil), c.driftToolNames...)
	}
	if c.serverInfo != nil {
		cp := *c.serverInfo
		s.ServerInfo = &cp
	}
	return s
}

// Connect attempts a connection. NEVER returns an error — always returns Status.
func (c *Client) Connect(ctx context.Context) Status {
	c.mu.Lock()
	// 1. Already connected.
	if c.connected {
		// Injected clients are "connected" from construction but were never warmed.
		needWarm := c.transportKind == transportInjected && !c.cacheWarm
		c.mu.Unlock()
		if needWarm {
			c.warmToolCache(ctx)
		}
		return c.Status()
	}

	// 2. Offline mode.
	if c.cfg.Offline {
		c.lastError = "offline mode"
		s := c.statusLocked()
		c.mu.Unlock()
		return s
	}

	// 3. Missing URL/token. An anonymous (no-auth) server needs only a URL; the bearer
	// token requirement applies to the primary Daintree control-plane MCP alone.
	if c.endpoint == "" || (!c.anonymous && c.cfg.McpToken == "") {
		c.lastError = "DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN not set"
		s := c.statusLocked()
		c.mu.Unlock()
		return s
	}

	rawURL := c.endpoint
	token := ""
	if !c.anonymous {
		token = c.cfg.McpToken
	}
	c.mu.Unlock()

	// 4. Parse URL.
	u, err := url.Parse(rawURL)
	if err != nil {
		c.mu.Lock()
		c.lastError = "invalid DAINTREE_MCP_URL: " + errMsg(err)
		c.connected = false
		s := c.statusLocked()
		c.mu.Unlock()
		return s
	}

	// 5/6. Try Streamable HTTP first. An empty token (anonymous server) gets a plain
	// client with no Authorization header.
	httpClient := httpClientFor(token)
	session, httpErr := c.connectStreamableHTTP(ctx, u.String(), httpClient)
	if httpErr == nil {
		c.applyConnected(session, transportStreamableHTTP)
		c.warmToolCache(ctx)
		return c.Status()
	}

	// 7. SSE fallback. Rewrite a trailing /mcp(/) → /sse; other paths unchanged.
	sseURL := *u
	sseURL.Path = sseRewriteRe.ReplaceAllString(sseURL.Path, "/sse")
	session, sseErr := c.connectSSE(ctx, sseURL.String(), httpClient)
	if sseErr == nil {
		c.applyConnected(session, transportSSE)
		c.warmToolCache(ctx)
		return c.Status()
	}

	// Both failed — concatenate in the exact format.
	c.mu.Lock()
	c.lastError = fmt.Sprintf("streamable-http: %s; sse: %s", errMsg(httpErr), errMsg(sseErr))
	c.connected = false
	s := c.statusLocked()
	c.mu.Unlock()
	return s
}

// applyConnected installs a freshly-connected SDK session as the low-level client.
// Any client it replaces (e.g. a racing concurrent Connect that already installed
// one) is detached and Closed outside the lock so its transport goroutines/
// connections don't leak. The session generation is bumped so an in-flight call
// against the OLD low client can detect it is stale and refuse to degrade us.
func (c *Client) applyConnected(session *sdkmcp.ClientSession, kind string) {
	c.mu.Lock()
	old := c.low
	fresh := &sdkLowLevel{session: session}
	c.low = fresh
	c.generation++
	c.connected = true
	c.transportKind = kind
	c.lastError = ""
	// Snapshot the intended subscriptions to re-issue on this fresh session — a
	// subscription does not survive a session swap, so without this a reconnect
	// would silently stop delivering resource notifications.
	var uris []string
	for u := range c.subs {
		uris = append(uris, u)
	}
	c.mu.Unlock()
	if old != nil && old != LowLevelClient(fresh) {
		closeLowLevel(old)
	}
	if len(uris) > 0 {
		// Off the connect path: re-subscribing is a series of network calls and must
		// not block Connect. Issue against the freshly-installed session directly.
		go c.resubscribe(fresh, uris)
	}
}

// resubscribe re-issues a snapshot of subscription URIs against a specific
// (freshly-connected) low client. Best-effort: a failure on one URI is ignored;
// the periodic reconcile tick is the backstop, and the next reconnect retries.
// Each URI is re-checked against c.subs first — a concurrent Unsubscribe between
// the applyConnected snapshot and now may have dropped it, and we must not
// resurrect a subscription nothing wants anymore.
func (c *Client) resubscribe(low LowLevelClient, uris []string) {
	ctx, cancel := context.WithTimeout(context.Background(), defaultCallTimeout)
	defer cancel()
	for _, u := range uris {
		c.mu.Lock()
		want := c.subs[u] > 0
		c.mu.Unlock()
		if !want {
			continue
		}
		_ = low.Subscribe(ctx, u)
	}
}

// Reconnect closes, resets all state, then connects. Close() already
// detached+closed the prior low client; here we bump the generation so any call
// still in flight against the old session can't degrade the fresh one Connect
// installs.
func (c *Client) Reconnect(ctx context.Context) Status {
	_ = c.Close()
	c.mu.Lock()
	c.connected = false
	c.toolCache = nil
	c.cacheWarm = false
	c.transportKind = transportNone
	c.lastError = ""
	c.driftWarnings = nil
	c.driftToolNames = nil
	c.serverInfo = nil
	c.low = nil
	c.generation++
	c.mu.Unlock()
	return c.Connect(ctx)
}

// warmToolCache lists tools + runs the drift check. Best-effort: a transient
// tool-list failure must NOT flip a freshly-connected transport to degraded (it
// would also CLOSE the live low client). So warm lists with degradeOnErr=false —
// on failure the connection is left exactly as Connect installed it and only the
// tool count stays unknown.
func (c *Client) warmToolCache(ctx context.Context) {
	if _, err := c.listTools(ctx, true, false); err != nil {
		return
	}
	c.runDriftCheck()
}

// runDriftCheck records server info + missing-documented-tool warnings. Warning
// only; never affects connected. Two isolated zones: a server-info
// fetch failure must not suppress the drift comparison.
func (c *Client) runDriftCheck() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. Reset.
	c.driftWarnings = nil
	c.driftToolNames = nil

	// 2. Server info (isolated): nil-safe GetServerVersion.
	c.serverInfo = nil
	if c.low != nil {
		if info := c.low.GetServerVersion(); info != nil {
			cp := *info
			c.serverInfo = &cp
		}
	}

	// 3. Build the live name set.
	live := make(map[string]struct{}, len(c.toolCache))
	for _, t := range c.toolCache {
		live[t.Name] = struct{}{}
	}
	// live.size === 0 → return (unknown, not "everything drifted").
	if len(live) == 0 {
		return
	}

	// 4. Missing-only: documented names absent from live, in array order. The baseline is
	// per-client (DocumentedMcpToolNames for the primary, a server-specific list for a
	// second server like the docs MCP) so the two never cross-contaminate.
	for _, name := range c.driftBaseline {
		if _, ok := live[name]; !ok {
			c.driftToolNames = append(c.driftToolNames, name)
			c.driftWarnings = append(c.driftWarnings,
				fmt.Sprintf("MCP drift: tool '%s' is documented but missing from the live server", name))
		}
	}
}

// ensure returns the active low-level client AND the generation it was snapshotted
// at, or an UnavailableError when not connected / no low client. The generation tag
// lets a caller pass it back to markDegraded so a failure from a now-stale session
// (e.g. after a concurrent Reconnect) can't flip a freshly-connected client to
// degraded. Caller must hold no lock (it takes mu).
func (c *Client) ensure() (LowLevelClient, uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.connected || c.low == nil {
		return nil, 0, newUnavailable(c.lastError)
	}
	return c.low, c.generation, nil
}

// markDegraded sets connected=false, clears cache/drift/serverInfo, records the
// error, and detaches+Closes the low client so its transport goroutines/connections
// don't leak after a transport failure. gen is the generation the failing call
// snapshotted from ensure(): if it no longer matches c.generation the session has
// since been replaced (Reconnect / a racing Connect), so this is a STALE failure
// from an old session and must NOT degrade the fresh one — we no-op. Caller must
// hold no lock; the detached client is Closed outside the lock.
func (c *Client) markDegraded(err error, gen uint64) {
	c.mu.Lock()
	if gen != c.generation {
		// Stale: a newer session is live. Leave it untouched.
		c.mu.Unlock()
		return
	}
	old := c.low
	c.low = nil
	c.generation++
	c.connected = false
	c.toolCache = nil
	c.cacheWarm = false
	c.driftWarnings = nil
	c.driftToolNames = nil
	c.serverInfo = nil
	c.lastError = errMsg(err)
	c.mu.Unlock()
	closeLowLevel(old)
}

// ListTools returns tools cache-first unless force. A real (non-abort)
// failure degrades the connection.
func (c *Client) ListTools(ctx context.Context, force bool) ([]ToolInfo, error) {
	return c.listTools(ctx, force, true)
}

// listTools is the cache-first list core. degradeOnErr controls whether a real
// failure marks the connection degraded (true for the public ListTools); the warm
// path passes false so a transient first-list failure can't tear down the just-
// established transport.
func (c *Client) listTools(ctx context.Context, force, degradeOnErr bool) ([]ToolInfo, error) {
	c.mu.Lock()
	if c.cacheWarm && !force {
		out := append([]ToolInfo(nil), c.toolCache...)
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	low, gen, err := c.ensure()
	if err != nil {
		return nil, err
	}

	// Same governor as CallTool: a tool-list is one more request on the same
	// server, and connect/doctor probes must queue behind (not pile onto) an
	// in-flight read burst. Same discipline too: re-snapshot the session after
	// the queue wait, and release via defer so a panic can't leak the slot.
	if gerr := c.gov.acquire(ctx); gerr != nil {
		return nil, gerr
	}
	if l2, g2, err2 := c.ensure(); err2 == nil {
		low, gen = l2, g2
	}
	rawTools, err := func() ([]rawTool, error) {
		defer c.gov.release()
		return low.ListTools(ctx)
	}()
	if err != nil {
		// An abort (caller cancel) says nothing about connection health → don't
		// degrade. UnavailableError already means disconnected → don't re-degrade.
		if degradeOnErr && !isUnavailable(err) && !isAborted(ctx) {
			c.markDegraded(err, gen)
		}
		return nil, err
	}

	tools := make([]ToolInfo, 0, len(rawTools))
	for _, rt := range rawTools {
		ti := ToolInfo{Name: rt.Name, Description: rt.Description}
		if schema, ok := rt.InputSchema.(map[string]any); ok && schema != nil {
			ti.InputSchema = schema
		} else {
			ti.InputSchema = defaultInputSchema()
		}
		tools = append(tools, ti)
	}

	c.mu.Lock()
	c.toolCache = tools
	c.cacheWarm = true
	out := append([]ToolInfo(nil), tools...)
	c.mu.Unlock()
	return out, nil
}

// CallTool dispatches a tool with retry (read-only callers) + normalization.
// RETRY-BEFORE-DEGRADE ordering is load-bearing: degrading first would
// make the next ensure() throw, killing the retry.
//
// READ-ONLY RETRY GUARD: CallOptions.Retries is honored ONLY for tools on the
// read-only allowlist (readOnlyToolNames). Retrying a mutation (terminal.sendCommand,
// agent.launch, recipe.run, …) risks a double-apply on a transient transport blip,
// so a non-read tool is forced single-shot even if a caller mistakenly set Retries>0.
// This makes retry-safety a property of the tool, not caller discipline.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, opts CallOptions) (result CallResult, err error) {
	if args == nil {
		args = map[string]any{}
	}
	retries := opts.Retries
	if retries < 0 {
		retries = 0
	}
	// Mutations never auto-retry, regardless of the requested budget.
	if !isReadOnlyToolName(name) {
		retries = 0
	}

	// Trace every MCP call ONCE, on exit, with the final outcome (named returns). This
	// is the layer where so many tool failures actually originate (throttles, transport
	// blips, degrade) but the model loop only ever saw the post-parse result — the
	// "logging the MCP details" the trace is meant to capture. Bounded: success logs a
	// preview + hash of the response, never the full body every poll.
	start := time.Now()
	attempts := 0
	var queuedMs int64
	defer func() {
		c.traceCall(name, retries, attempts, time.Since(start).Milliseconds(), queuedMs, result, err)
	}()

	var res rawResult
	for attempt := 0; ; attempt++ {
		attempts = attempt + 1
		low, gen, err := c.ensure()
		if err != nil {
			return CallResult{}, err // UnavailableError: never retried, never degrades.
		}

		// Per-attempt deadline derived from the caller ctx (don't fuse signals). A caller
		// that sets no Timeout still gets defaultCallTimeout so a silent server can't hang
		// the turn forever.
		timeout := opts.Timeout
		if timeout <= 0 {
			timeout = defaultCallTimeout
		}
		// Governor: wait for an in-flight slot + this call's paced start, held only
		// for the ONE network attempt below — the retry/throttle backoff sleeps run
		// with the slot released, so a waiting retry never pins capacity. An abort
		// while queued propagates like an aborted call and never degrades (the
		// transport was never touched). Note the queue wait sits OUTSIDE the
		// per-attempt Timeout below (which measures wire time only); a caller that
		// needs an end-to-end budget bounds ctx itself, which aborts the queue wait
		// too.
		queueStart := time.Now()
		if gerr := c.gov.acquire(ctx); gerr != nil {
			return CallResult{}, gerr
		}
		queuedMs += time.Since(queueStart).Milliseconds()
		// Re-snapshot the session AFTER the queue wait: a Reconnect while this call
		// sat queued replaced the low client, and calling the detached old one would
		// burn the attempt on a guaranteed connection-closed failure. If ensure now
		// fails (degraded while queued), keep the original snapshot — the call fails
		// exactly as it would have, and markDegraded's generation guard keeps that
		// stale failure from touching fresh state.
		if l2, g2, err2 := c.ensure(); err2 == nil {
			low, gen = l2, g2
		}
		res, err = func() (rawResult, error) {
			// Deferred so an SDK panic can't leak the slot — a leaked slot is a
			// permanent capacity loss for the whole process.
			defer c.gov.release()
			callCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return low.CallTool(callCtx, name, args)
		}()
		if err == nil {
			// Transport-level success — but a Daintree READ can come back as a throttle
			// RESULT (IsError=true + MCP_RATE_LIMITED + details.retryAfter) rather than a
			// transport error, which CallTool would otherwise hand straight to the model
			// as a failed read (costing a whole LLM round to re-decide). Absorb it here,
			// below the model, exactly like a transient transport error. The read-only
			// guard is already enforced: `retries` was forced to 0 for mutations above,
			// so `attempt < retries` is false for a non-read tool — a mutation that comes
			// back IsError is never retried (no double-apply).
			if res.IsError && attempt < retries {
				if delay, ok := throttleRetryAfter(res.Text); ok {
					if sleepErr := abortableSleep(ctx, delay); sleepErr != nil {
						return CallResult{}, sleepErr // aborted mid-backoff: propagate, no degrade.
					}
					continue
				}
			}
			break
		}

		aborted := isAborted(ctx)

		// Retry path FIRST (before degrading).
		if !aborted && !isUnavailable(err) && attempt < retries && isRetriableMcpError(err) {
			delay := fullJitterDelay(attempt, mcpReadRetryPolicy.BaseDelayMs, mcpReadRetryPolicy.MaxDelayMs)
			if sleepErr := abortableSleep(ctx, delay); sleepErr != nil {
				// Aborted mid-backoff: propagate without degrading.
				return CallResult{}, sleepErr
			}
			continue
		}

		// Degrade path: only for a real, non-abort, non-unavailable failure, and
		// only if this call's session (gen) is still the live one.
		if !isUnavailable(err) && !aborted {
			c.markDegraded(err, gen)
		}
		return CallResult{}, err
	}

	// Normalize. Content default []; isError default false; text/structured verbatim.
	content := res.Content
	if content == nil {
		content = []any{}
	}
	return CallResult{
		Text:              res.Text,
		Content:           content,
		StructuredContent: res.StructuredContent,
		IsError:           res.IsError,
	}, nil
}

// mcpTracePreviewMax bounds the preview of an MCP response body in the trace. Reads
// (terminal.getStatus/getOutput) are polled frequently, so the full body is never
// dumped — a preview + hash is enough to see what a tool actually received.
const mcpTracePreviewMax = 1000

// traceCall appends one bounded `mcp.call` event to the per-session debug log. The
// MCP layer is where many tool failures truly originate (throttle RESULTS, transport
// blips, degrade) while the model loop only ever saw the post-parse result; this
// makes the underlying call visible. Gated on debug logging (the field-building has
// cost), panic-guarded, and never throws — a logging fault must not affect a call.
// Success logs a preview + hash of the payload; failure logs the normalized error.
func (c *Client) traceCall(name string, retries, attempts int, durationMs, queuedMs int64, result CallResult, err error) {
	dbg := debuglog.Config{DebugLog: c.cfg.DebugLog, LogDir: c.cfg.LogDir}
	if !dbg.DebugLog {
		return
	}
	defer func() { _ = recover() }()
	kind := "mutation"
	if isReadOnlyToolName(name) {
		kind = "read"
	}
	// transportOk (not "ok") deliberately: this is transport/API success — NO Go error —
	// which is NOT the same as the tool succeeding. A Daintree read can return a throttle
	// or tool-level error as a RESULT (transportOk=true, isError=true). Naming it "ok"
	// would clash with tool.call's "ok" (which DOES mean the tool succeeded) and make a
	// failure-hunting grep miss tool-level MCP errors.
	fields := map[string]any{
		"mcpTool":     name,
		"callKind":    kind,
		"retries":     retries,
		"attempts":    attempts,
		"durationMs":  durationMs,
		"transportOk": err == nil,
	}
	// Governor queueing (in-flight slot + pacing waits) — surfaced only when it
	// actually delayed the call, so pressure shows up in log archaeology without
	// adding noise to every unqueued poll.
	if queuedMs > 0 {
		fields["queuedMs"] = queuedMs
	}
	if err != nil {
		fields["error"] = err.Error()
		debuglog.LogDebug(dbg, "mcp.call", fields)
		return
	}
	fields["isError"] = result.IsError
	fields["text"] = debuglog.Summarize(result.Text, mcpTracePreviewMax)
	if result.StructuredContent != nil {
		fields["structured"] = debuglog.SummarizeJSON(result.StructuredContent, mcpTracePreviewMax)
	}
	debuglog.LogDebug(dbg, "mcp.call", fields)
}

// Close swallows low-client errors and always sets connected=false. Idempotent:
// it detaches c.low under the lock (so a second Close is a no-op and an in-flight
// call can't observe a half-closed client) and bumps the generation so any stale
// call still running against the old session can't degrade a future one. The
// detached client is Closed outside the lock.
func (c *Client) Close() error {
	c.mu.Lock()
	low := c.low
	c.low = nil
	c.generation++
	c.connected = false
	c.mu.Unlock()
	closeLowLevel(low) // ignore errors (matches close() try/catch); nil-safe
	return nil
}

// --- resource subscription ---

// SupportsSubscribe reports whether the live server advertised
// capabilities.resources.subscribe. False when disconnected — a caller must treat
// it as "fall back to polling", never as a hard error.
func (c *Client) SupportsSubscribe() bool {
	low, _, err := c.ensure()
	if err != nil {
		return false
	}
	return low.SupportsSubscribe()
}

// Subscribe registers interest in a resource URI so the server pushes
// resources/updated notifications (routed to ResourceUpdates). Single-shot: a
// failure is returned, not retried, and does NOT degrade the connection — one URI
// the server rejects must not tear down a healthy session (the next read degrades
// it if the transport is truly dead). The caller bounds the call via ctx.
func (c *Client) Subscribe(ctx context.Context, uri string) error {
	low, _, err := c.ensure()
	if err != nil {
		return err
	}
	// Reserve a refcount slot under the lock so exactly one caller observes the 0→1
	// edge even under concurrent subscribers to the same URI. Only that first caller
	// issues the wire Subscribe — the server tracks one subscription per (session,
	// URI), so a second wire call is redundant and a later single Unsubscribe would
	// otherwise revoke it for everyone.
	c.mu.Lock()
	if c.subs == nil {
		c.subs = map[string]int{}
	}
	was := c.subs[uri]
	c.subs[uri] = was + 1
	c.mu.Unlock()
	if was > 0 {
		return nil
	}
	if err := low.Subscribe(ctx, uri); err != nil {
		// Roll the reservation back so a later retry re-issues the wire Subscribe.
		c.mu.Lock()
		if c.subs[uri] > 0 {
			c.subs[uri]--
			if c.subs[uri] == 0 {
				delete(c.subs, uri)
			}
		}
		c.mu.Unlock()
		return err
	}
	return nil
}

// Unsubscribe drops one local subscriber's interest in a URI. Only the LAST
// withdrawal (refcount → 0) issues the wire Unsubscribe and forgets the URI, so one
// watcher stopping never tears down a subscription another watcher still relies on.
// Best-effort, same non-degrading contract.
func (c *Client) Unsubscribe(ctx context.Context, uri string) error {
	c.mu.Lock()
	remaining := 0
	if c.subs[uri] > 0 {
		c.subs[uri]--
		remaining = c.subs[uri]
		if remaining == 0 {
			delete(c.subs, uri)
		}
	}
	c.mu.Unlock()
	if remaining > 0 {
		return nil // another local subscriber still needs the wire subscription
	}
	low, _, err := c.ensure()
	if err != nil {
		return err
	}
	return low.Unsubscribe(ctx, uri)
}

// ReadResource fetches a resource's current text contents. Read-only and idempotent
// but kept single-shot (the caller bounds it via ctx) — a stale read is harmless,
// so degrading/retrying here buys nothing the reconcile tick doesn't already cover.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	low, _, err := c.ensure()
	if err != nil {
		return "", err
	}
	return low.ReadResource(ctx, uri)
}

// --- transport construction ---

// bearerRoundTripper injects `Authorization: Bearer <token>` on every request.
// The Go MCP SDK transports take an *http.Client but no header hook, so we wrap
// the RoundTripper (wrap the underlying *http.Client to inject bearer).
type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (b *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone before mutating headers — a RoundTripper must not modify its argument.
	r2 := req.Clone(req.Context())
	r2.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r2)
}

func bearerHTTPClient(token string) *http.Client {
	return &http.Client{Transport: &bearerRoundTripper{token: token, base: http.DefaultTransport}}
}

// httpClientFor returns a bearer-injecting client for the primary Daintree MCP, or a
// plain client (no Authorization header) when token is empty — the public, no-auth docs
// MCP. Sending "Bearer " with an empty token would be a malformed credential some
// gateways reject, so an anonymous server gets no auth header at all.
func httpClientFor(token string) *http.Client {
	if token == "" {
		return &http.Client{}
	}
	return bearerHTTPClient(token)
}

// connectStreamableHTTP connects via the SDK's Streamable HTTP transport. It
// reuses c.sdkClient (built once in New) so the resource-updated handler is
// already registered — a per-connect NewClient would drop notifications.
func (c *Client) connectStreamableHTTP(ctx context.Context, endpoint string, hc *http.Client) (*sdkmcp.ClientSession, error) {
	tr := &sdkmcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}
	return c.sdkClient.Connect(ctx, tr, nil)
}

// connectSSE connects via the SDK's legacy SSE transport, reusing c.sdkClient.
func (c *Client) connectSSE(ctx context.Context, endpoint string, hc *http.Client) (*sdkmcp.ClientSession, error) {
	tr := &sdkmcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc}
	return c.sdkClient.Connect(ctx, tr, nil)
}
