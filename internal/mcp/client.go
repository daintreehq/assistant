// Package mcp connects the assistant to Daintree's local MCP server and exposes a
// small, degradation-tolerant API. Spec: docs/port/mcp.md.
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
}

// Client is the high-level DaintreeMcpClient. All mutable state is guarded by mu
// because callers (UI hook, daemon ticks, doctor) may invoke concurrently — the TS
// original relied on the single-threaded event loop.
type Client struct {
	cfg config.AppConfig

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
	c := &Client{cfg: cfg, transportKind: transportNone}
	if opts.ClientOverride != nil {
		c.low = opts.ClientOverride
		c.connected = true
		c.transportKind = transportInjected
	}
	return c
}

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
		URL:       c.cfg.McpURL,
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
// See spec §4 for the exact control flow.
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

	// 3. Missing URL/token.
	if c.cfg.McpURL == "" || c.cfg.McpToken == "" {
		c.lastError = "DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN not set"
		s := c.statusLocked()
		c.mu.Unlock()
		return s
	}

	rawURL := c.cfg.McpURL
	token := c.cfg.McpToken
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

	// 5/6. Try Streamable HTTP first.
	httpClient := bearerHTTPClient(token)
	session, httpErr := connectStreamableHTTP(ctx, u.String(), httpClient)
	if httpErr == nil {
		c.applyConnected(session, transportStreamableHTTP)
		c.warmToolCache(ctx)
		return c.Status()
	}

	// 7. SSE fallback. Rewrite a trailing /mcp(/) → /sse; other paths unchanged.
	sseURL := *u
	sseURL.Path = sseRewriteRe.ReplaceAllString(sseURL.Path, "/sse")
	session, sseErr := connectSSE(ctx, sseURL.String(), httpClient)
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
	c.mu.Unlock()
	if old != nil && old != LowLevelClient(fresh) {
		closeLowLevel(old)
	}
}

// Reconnect closes, resets all state, then connects. See spec §4. Close() already
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
// only; never affects connected. See spec §7. Two isolated zones: a server-info
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

	// 4. Missing-only: documented names absent from live, in array order.
	for _, name := range DocumentedMcpToolNames {
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

// ListTools returns tools cache-first unless force. See spec §5. A real (non-abort)
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

	rawTools, err := low.ListTools(ctx)
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

// CallTool dispatches a tool with retry (read-only callers) + normalization. See
// spec §6. RETRY-BEFORE-DEGRADE ordering is load-bearing: degrading first would
// make the next ensure() throw, killing the retry.
//
// READ-ONLY RETRY GUARD: CallOptions.Retries is honored ONLY for tools on the
// read-only allowlist (readOnlyToolNames). Retrying a mutation (terminal.sendCommand,
// agent.launch, recipe.run, …) risks a double-apply on a transient transport blip,
// so a non-read tool is forced single-shot even if a caller mistakenly set Retries>0.
// This makes retry-safety a property of the tool, not caller discipline.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any, opts CallOptions) (CallResult, error) {
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

	var res rawResult
	for attempt := 0; ; attempt++ {
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
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		res, err = low.CallTool(callCtx, name, args)
		cancel()
		if err == nil {
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

// --- transport construction ---

// bearerRoundTripper injects `Authorization: Bearer <token>` on every request.
// The Go MCP SDK transports take an *http.Client but no header hook, so we wrap
// the RoundTripper (spec §11: "wrap the underlying *http.Client to inject bearer").
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

// connectStreamableHTTP connects via the SDK's Streamable HTTP transport.
func connectStreamableHTTP(ctx context.Context, endpoint string, hc *http.Client) (*sdkmcp.ClientSession, error) {
	cli := sdkmcp.NewClient(&sdkmcp.Implementation{Name: clientName, Version: clientVersion}, nil)
	tr := &sdkmcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: hc}
	return cli.Connect(ctx, tr, nil)
}

// connectSSE connects via the SDK's legacy SSE transport.
func connectSSE(ctx context.Context, endpoint string, hc *http.Client) (*sdkmcp.ClientSession, error) {
	cli := sdkmcp.NewClient(&sdkmcp.Implementation{Name: clientName, Version: clientVersion}, nil)
	tr := &sdkmcp.SSEClientTransport{Endpoint: endpoint, HTTPClient: hc}
	return cli.Connect(ctx, tr, nil)
}
