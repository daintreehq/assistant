package app

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/queue"
	"github.com/daintreehq/daintree-assistant/internal/storage"
	"github.com/daintreehq/daintree-assistant/internal/tools/agenttaskx"
)

// ConnectMcp connects the MCP transport, rolls up any tool drift to one log line,
// and refreshes the startup context cache. The runtime context is sent fresh as
// structured data on every backend round (built from PromptContext), so there is no
// session message to rewrite here anymore.
func (a *App) ConnectMcp(ctx context.Context) mcp.Status {
	// Kick off the docs-MCP handshake in PARALLEL (fire-and-forget) so it rides alongside
	// the Daintree connect during the boot splash WITHOUT sitting on the critical path —
	// a slow/unreachable public docs server must never add latency to boot, one-shot runs,
	// doctor, or /reconnect. The docs tools fail cleanly with MCP_UNAVAILABLE until it lands.
	a.connectDocsAsync(false)
	st := a.MCP.Connect(ctx)
	a.logMcpCredentials(st)
	a.warnOnDrift(st)
	a.refreshStartupContext(ctx, st.Connected)
	// Boot-only: reconcile the durable ledger against the live terminal inventory on
	// the first successful connect (idempotency-guarded inside).
	a.maybeReconcileLedger(ctx, st.Connected)
	return st
}

// ReconnectMcp re-establishes the transport.
func (a *App) ReconnectMcp(ctx context.Context) mcp.Status {
	// Reconnect the docs MCP in parallel too (fire-and-forget), so /reconnect refreshes
	// BOTH transports without the docs link gating the primary status it returns.
	a.connectDocsAsync(true)
	st := a.MCP.Reconnect(ctx)
	a.logMcpCredentials(st)
	a.warnOnDrift(st)
	a.refreshStartupContext(ctx, st.Connected)
	// If the initial connect never succeeded, the boot reconcile runs on this first
	// successful (re)connect instead; the once-guard makes a later reconnect a no-op.
	a.maybeReconcileLedger(ctx, st.Connected)
	return st
}

// docsConnectTimeout bounds the docs-MCP handshake so a wedged or unreachable docs
// server can never leave a connect goroutine running indefinitely. docsCloseTimeout
// bounds the teardown at shutdown for the same reason (a public, un-timed endpoint).
const docsConnectTimeout = 6 * time.Second
const docsCloseTimeout = 2 * time.Second

// closeMCPWithTimeout closes an mcp.Client but never blocks process exit longer than d:
// the close issues a network teardown over an un-timed http.Client, so a wedged public
// endpoint (the anonymous docs server) could otherwise hang exit. On timeout we abandon
// the close goroutine — the process is exiting anyway, so the leak is bounded by exit.
func closeMCPWithTimeout(c *mcp.Client, d time.Duration) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = c.Close()
	}()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// connectDocsAsync (re)connects the docs MCP on a background goroutine bounded by
// docsConnectTimeout. FIRE-AND-FORGET (no join handle returned) so it never gates the
// caller, but tracked on docsConnectWG so Shutdown JOINS it before closing the client —
// otherwise a late applyConnected could install a session AFTER Close and leak it. The
// goroutine derives its context from a.baseCtx (not the caller's), so Shutdown's
// baseCancel aborts an in-flight connect; a failed docs connect is non-fatal and is NOT
// folded into the primary mcp.Status.
func (a *App) connectDocsAsync(reconnect bool) {
	if a.DocsMCP == nil {
		return
	}
	a.docsConnectWG.Add(1)
	go func() {
		defer a.docsConnectWG.Done()
		ctx, cancel := context.WithTimeout(a.baseCtx, docsConnectTimeout)
		defer cancel()
		if reconnect {
			a.DocsMCP.Reconnect(ctx)
		} else {
			a.DocsMCP.Connect(ctx)
		}
	}()
}

// startupReadTimeout bounds each startup-context Daintree read (worktree.getCurrent)
// so a wedged server can't stall a (re)connect. These are cheap local lookups in
// practice (single-digit ms), so a few seconds is generous slack, not a real ceiling.
// (ConfiguredAgentIDs applies its own equivalent bound internally.)
const startupReadTimeout = 5 * time.Second

// noActiveWorktreeLabel is the message[1] label when Daintree reports no current
// worktree (worktree.getCurrent → {worktree: null}) — a definitive answer, distinct
// from the "(unknown — read with context.snapshot)" placeholder used when the read
// wasn't attempted or failed (degraded mode).
const noActiveWorktreeLabel = "(none — not in a worktree)"

// refreshStartupContext refreshes the message[1] startup cache — the configured-agents
// roster and the current worktree label — from Daintree. The caller follows it with a
// RefreshRuntimeContext so the refreshed facts land in message[1]. It is the ONLY
// writer of the rosterMu-guarded cache.
//
// Both reads are best-effort and bounded: a hung or absent Daintree must never stall a
// (re)connect, so each failure falls open to empty. When not connected we clear the
// cache so message[1] honestly drops the roster line and shows the worktree
// "(unknown)" placeholder rather than stale facts from a prior connection. The cache is
// replaced ATOMICALLY after both reads complete (never cleared before the fetch), so a
// concurrent PromptContext on a mid-session /reconnect never observes a half-populated
// snapshot — it sees the prior values until both reads finish, then the new ones.
func (a *App) refreshStartupContext(ctx context.Context, connected bool) {
	var agentIDs []string
	var worktree string
	if connected {
		// agentSettings.get — the user-configured agents roster. ConfiguredAgentIDs bounds
		// its own read and fails open (nil) on any error, so the roster line simply omits.
		agentIDs = agenttaskx.ConfiguredAgentIDs(ctx, agentTaskMCPAdapter{c: a.MCP})
		// worktree.getCurrent — the active worktree label. Bound it with a CANCEL-based
		// deadline (not context.WithTimeout): mcp.Client degrades the connection on any
		// non-abort CallTool error, and a DeadlineExceeded is NOT an abort — only a
		// Canceled is. A best-effort startup read must never tear down a working
		// connection just because it was slow, so a timeout surfaces as a cancel; on any
		// error/IsError the label stays "" → the "(unknown)" placeholder.
		wctx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(startupReadTimeout, cancel)
		res, err := a.MCP.CallTool(wctx, "worktree.getCurrent", map[string]any{}, mcp.CallOptions{})
		timer.Stop()
		cancel()
		if err == nil && !res.IsError {
			worktree = parseCurrentWorktreeLabel(res.StructuredContent, res.Text)
		}
	}
	a.rosterMu.Lock()
	a.cachedAgentIDs = agentIDs
	a.cachedActiveWorktree = worktree
	a.rosterMu.Unlock()
}

// parseCurrentWorktreeLabel turns a worktree.getCurrent result into a short, human-
// readable label for message[1]. Daintree returns { worktree: { id, path, branch, … }
// | null }. The label prefers branch (most meaningful to the model), then id, then
// path. A definitively-null worktree (the key is present but null) yields
// noActiveWorktreeLabel so the runtime context states "not in a worktree" rather than
// telling the model to go read it; an absent/unparseable payload yields "" so the
// caller falls back to the "(unknown — read with context.snapshot)" placeholder. Unions
// structuredContent and the JSON text body (Daintree returns results in text), mirroring
// the other Daintree parsers, so a divergence between the two sources can't strand the
// label. Never throws.
func parseCurrentWorktreeLabel(structured any, text string) string {
	if sc, ok := structured.(map[string]any); ok {
		if wt, present := sc["worktree"]; present {
			if wt == nil {
				return noActiveWorktreeLabel
			}
			if m, ok := wt.(map[string]any); ok {
				if label := worktreeLabelFromMap(m); label != "" {
					return label
				}
			}
		}
	}
	if strings.TrimSpace(text) != "" {
		var parsed struct {
			Worktree json.RawMessage `json:"worktree"`
		}
		if json.Unmarshal([]byte(text), &parsed) == nil && len(parsed.Worktree) > 0 {
			if strings.TrimSpace(string(parsed.Worktree)) == "null" {
				return noActiveWorktreeLabel
			}
			var m map[string]any
			if json.Unmarshal(parsed.Worktree, &m) == nil {
				if label := worktreeLabelFromMap(m); label != "" {
					return label
				}
			}
		}
	}
	return ""
}

// worktreeLabelFromMap picks the best human-readable label from a worktree-summary
// object: branch first, then id, then path. Returns "" when none are usable strings.
func worktreeLabelFromMap(m map[string]any) string {
	for _, key := range []string{"branch", "id", "path"} {
		if s, ok := m[key].(string); ok {
			if s = strings.TrimSpace(s); s != "" {
				return s
			}
		}
	}
	return ""
}

// logMcpCredentials dumps the RAW Daintree MCP URL and bearer token to the debug log on
// every (re)connect, so a developer can replay MCP calls by hand — e.g. curl the same
// endpoint that terminal.extract hits — and see the real server responses while chasing
// the extract breakage. It deliberately writes the UNREDACTED token (the whole point is to
// reuse it).
//
// TODO: remove this method and its two call sites above once the terminal.extract
// investigation is closed — it is a temporary debug aid, not a permanent feature.
//
// It is doubly gated: it only writes when full debug logging is already enabled
// (DAINTREE_ASSISTANT_DEBUG_LOG=1) and only into the 0600 per-session log under
// ~/.daintree/logs. The line's `note` field flags it as temporary so it stays greppable.
func (a *App) logMcpCredentials(st mcp.Status) {
	url := a.Config.McpURL
	if url == "" {
		url = st.URL // fall back to the resolved/connected URL
	}
	if url == "" && a.Config.McpToken == "" {
		return // nothing to replay against — don't emit a useless line
	}
	debuglog.LogDebug(
		debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		"mcp.credentials",
		map[string]any{
			"note":      "TODO: temporary debug — remove this. Raw Daintree MCP URL + bearer token so calls can be replayed by hand to debug terminal.extract.",
			"url":       url,
			"token":     a.Config.McpToken,
			"connected": st.Connected,
			"transport": st.Transport,
		},
	)
}

// warnOnDrift emits ONE rollup log line when the live server advertises fewer
// tools than documented. Preview shows the first 3 names.
func (a *App) warnOnDrift(st mcp.Status) {
	if len(st.DriftToolNames) == 0 {
		return
	}
	names := st.DriftToolNames
	n := len(names)
	noun := "tool"
	if n != 1 {
		noun = "tools"
	}
	preview := names
	suffix := ""
	if n > 3 {
		preview = names[:3]
		suffix = ", +" + itoa(n-3) + " more"
	}
	msg := "⚠️  MCP drift: " + itoa(n) + " documented " + noun +
		" not advertised by the live server (" + strings.Join(preview, ", ") + suffix +
		"). Run /doctor for the full list."
	if log := a.snapshotHooks().Log; log != nil {
		log(msg)
	}
}

// StartScheduler builds and starts the in-process daemon (idempotent). A second
// call rebinds the attention callback rather than leaking a second ticker.
// After the first start the runtime context is refreshed so the
// prompt reflects an active scheduler.
func (a *App) StartScheduler(ctx context.Context, onAttention func(events []domain.QueueEvent)) *daemon.Scheduler {
	if a.scheduler != nil {
		a.scheduler.SetOnAttention(onAttention)
		return a.scheduler
	}
	a.scheduler = daemon.NewScheduler(daemon.SchedulerDeps{
		Store:    a.Store,
		Queue:    daemonQueueAdapter{q: a.Queue},
		Registry: daemonRegistryAdapter{app: a},
		CtxFor:   a.daemonCtxFor,
		// Resource-update wake channel: a pushed agent-state transition triggers an
		// immediate watcher re-check instead of waiting the next tick interval.
		ResourceUpdates: a.MCP.ResourceUpdates(),
		OnAttention:     onAttention,
	})
	a.scheduler.Start(ctx)
	return a.scheduler
}

// ClearWatchers tears down ALL live watchers in this session — revokes their
// grants, cancels them, and resolves their open attention events — for a completely
// clean slate. Used by /clear; it mirrors what the session-boundary sweep does on
// the next launch (the two situations that wipe supervision). The actual Daintree
// terminals keep running; the assistant simply stops supervising them. Returns how
// many watchers were cancelled. Best-effort: the error is returned for logging only.
func (a *App) ClearWatchers() (int, error) {
	titles, err := a.Store.CancelLiveWatchers(domain.NowMS(), storage.ReasonSessionCleared)
	return len(titles), err
}

// ClearInbox resolves every open attention event for a clean-slate inbox (the !N
// badge → 0). Used by /clear alongside ClearWatchers; the session-boundary open does
// the same on the next launch. Returns how many events were resolved. Best-effort.
func (a *App) ClearInbox() (int, error) {
	return a.Store.ResolveAllOpenEvents(domain.NowMS())
}

// daemonCtxFor builds a per-actor daemon.CheckContext. The
// non-interactive actor reaches read-only MCP + the small-model classifier/judge.
func (a *App) daemonCtxFor(ctx context.Context, actor domain.ToolActor, actorID string) *daemon.CheckContext {
	return &daemon.CheckContext{
		Ctx:          ctx,
		Store:        a.Store,
		Queue:        daemonQueueAdapter{q: a.Queue},
		MCP:          daemonMcpAdapter{c: a.MCP},
		Model:        watcherModelAdapter{tasks: a.Backend},
		MemoryWriter: a.Store,
		SessionID:    a.SessionID,
		ProjectPath:  a.Config.ProjectPath,
	}
}

// SetHooks merges partial hook updates so prior confirm/log/events survive when
// only one is replaced. The session's eventProxy reads them live.
func (a *App) SetHooks(h AppHooks) {
	a.hooksMu.Lock()
	defer a.hooksMu.Unlock()
	if h.Confirm != nil {
		a.hooks.Confirm = h.Confirm
	}
	if h.Log != nil {
		a.hooks.Log = h.Log
	}
	if h.AgentEvents != nil {
		a.hooks.AgentEvents = h.AgentEvents
	}
}

// Shutdown tears the dependencies down in reverse: stop+drain the scheduler, close
// MCP, close the store. Safe to call once.
func (a *App) Shutdown() error {
	// Cancel the app-scoped background context first so any detached jobs
	// (terminal.extract.async, the post-compaction distill goroutine) stop touching
	// MCP/Router/Store before we close them.
	if a.baseCancel != nil {
		a.baseCancel()
	}
	if a.scheduler != nil {
		a.scheduler.Stop()
		a.scheduler.Drain()
	}
	// Join the session's detached distill goroutine BEFORE closing the Router/Store it
	// writes to. baseCancel above already cancelled its context, so this returns fast.
	if a.Session != nil {
		a.Session.DrainBackgroundWork()
	}
	if a.MCP != nil {
		_ = a.MCP.Close()
	}
	if a.DocsMCP != nil {
		// Join any in-flight docs (re)connect first — baseCancel above already aborted it,
		// so this returns fast — so a late applyConnected can't install a session AFTER
		// Close and leak it. Then bound the close: the docs server is public and anonymous
		// (un-timed http.Client), so a wedged endpoint must not hang process exit.
		a.docsConnectWG.Wait()
		closeMCPWithTimeout(a.DocsMCP, docsCloseTimeout)
	}
	if a.Store != nil {
		return a.Store.Close()
	}
	return nil
}

// --- daemon adapters ---

// daemonQueueAdapter strips the context from the *queue.Queue methods to satisfy
// the daemon.Queue seam (the daemon ticks carry their own background context).
type daemonQueueAdapter struct{ q *queue.Queue }

func (a daemonQueueAdapter) Publish(args domain.QueuePublishArgs) error {
	_, err := a.q.Publish(context.Background(), args)
	return err
}
func (a daemonQueueAdapter) Digest(opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	return a.q.Digest(context.Background(), opts)
}
func (a daemonQueueAdapter) MarkNotified(ids []string) error {
	return a.q.MarkNotified(context.Background(), ids)
}

// daemonRegistryAdapter runs a call_safe_tool timer payload through the registry as
// the given non-interactive actor, building a per-actor ToolContext.
type daemonRegistryAdapter struct{ app *App }

func (a daemonRegistryAdapter) Dispatch(ctx context.Context, actor domain.ToolActor, actorID, name, argsJSON string) (domain.ToolResult, error) {
	// Pass actorID (the firing timer's id) so dispatch's grant lookup (Branch A,
	// keyed on ActorID) can match a timer-scoped automation grant.
	tctx := a.app.buildContext(actor, actorID)
	res := a.app.Registry.Dispatch(ctx, name, []byte(argsJSON), tctx)
	return res, nil
}

// DaemonMCP exposes the same read-only MCP adapter the daemon ticks use, so the
// cockpit's off-loop dashboard build can drive daemon.FetchPreviews through it. The
// adapter just wraps a pointer (no I/O to construct) and *mcp.Client is concurrent-
// safe, so the UI hook and the scheduler can share one client without coordination.
func (a *App) DaemonMCP() daemon.MCP { return daemonMcpAdapter{c: a.MCP} }

// daemonMcpAdapter adapts *mcp.Client onto the daemon.MCP read-only seam, mapping
// the normalized CallResult onto daemon.MCPResult (structuredContent + text).
type daemonMcpAdapter struct{ c *mcp.Client }

func (a daemonMcpAdapter) Connected() bool { return a.c.IsConnected() }

func (a daemonMcpAdapter) SupportsSubscribe() bool { return a.c.SupportsSubscribe() }

// Subscribe/Unsubscribe forward to the client's resource-subscription surface,
// bounding each control call by the same per-read timeout the daemon uses for
// CallRead so a wedged subscribe can't hang the watcher's tick.
func (a daemonMcpAdapter) Subscribe(ctx context.Context, uri string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(daemon.McpReadTimeoutMS)*time.Millisecond)
	defer cancel()
	return a.c.Subscribe(cctx, uri)
}

func (a daemonMcpAdapter) Unsubscribe(ctx context.Context, uri string) error {
	cctx, cancel := context.WithTimeout(ctx, time.Duration(daemon.McpReadTimeoutMS)*time.Millisecond)
	defer cancel()
	return a.c.Unsubscribe(cctx, uri)
}

// daemonReadCallOptions builds the per-request CallOptions for a read-only daemon
// MCP call from the daemon's documented cadence constants. Bounding each attempt
// (20s) and asking for the read-only retry budget (2) stops a watcher/PR-watcher
// read from hanging until the outer tick ctx and lets it survive a transient
// transport blip. These are read-only calls, so CallTool honors Retries (it
// force-disables retry only for mutating tools). Referencing the daemon constants
// keeps the 20s / 2-retry policy in one place. Split out so it can be asserted.
func daemonReadCallOptions() mcp.CallOptions {
	return mcp.CallOptions{
		Timeout: time.Duration(daemon.McpReadTimeoutMS) * time.Millisecond,
		Retries: daemon.McpReadMaxRetries,
	}
}

func (a daemonMcpAdapter) CallRead(ctx context.Context, name string, args map[string]any) (daemon.MCPResult, error) {
	res, err := a.c.CallTool(ctx, name, args, daemonReadCallOptions())
	if err != nil {
		return daemon.MCPResult{}, err
	}
	out := daemon.MCPResult{Text: res.Text, IsError: res.IsError}
	if sc, ok := res.StructuredContent.(map[string]any); ok {
		out.StructuredContent = sc
	}
	return out, nil
}

// watcherModelAdapter satisfies daemon.WatcherModel using the backend's server-owned
// classify/judge tasks. A backend error degrades to the documented fallback rather
// than propagating (the engine also treats a returned error defensively).
type watcherModelAdapter struct{ tasks backend.TaskRunner }

func (w watcherModelAdapter) Classify(ctx context.Context, in daemon.ClassifyInput) (domain.WatcherVerdict, error) {
	fallback := domain.WatcherVerdict{
		Classification:    domain.ClassUnknown,
		Confidence:        0.3,
		Summary:           "Could not classify terminal output.",
		Evidence:          []string{},
		RecommendedAction: domain.ActionNone,
	}
	out, err := backend.RunWatcherClassify(ctx, w.tasks, backend.WatcherClassifyInput{
		Goal: in.Goal,
		TerminalState: backend.TerminalState{
			AgentState:    in.AgentState,
			RuntimeStatus: in.RuntimeStatus,
			LastOutputAt:  in.LastOutputAt,
		},
		PreviousClassification: in.Previous,
		Tail:                   in.Tail,
	})
	if err != nil {
		return fallback, nil
	}
	evidence := out.Evidence
	if evidence == nil {
		evidence = []string{}
	}
	// Validate the classification against the known enum (the old DecodeWatcherVerdict
	// did this) so an unexpected backend string can't flow into the watcher state machine
	// as a bogus, non-terminal classification — default it to ClassUnknown.
	classification := domain.WatcherClassification(out.Classification)
	if !classification.IsValid() {
		classification = domain.ClassUnknown
	}
	action := domain.RecommendedActionVerb(out.RecommendedAction)
	if out.RecommendedAction == "" {
		action = domain.ActionNone
	}
	return domain.WatcherVerdict{
		Classification:    classification,
		Confidence:        out.Confidence,
		Summary:           out.Summary,
		Evidence:          evidence,
		RecommendedAction: action,
	}, nil
}

func (w watcherModelAdapter) Judge(ctx context.Context, in daemon.JudgeInput) (domain.ModelJudgeAnswer, error) {
	ans, err := judgeTerminal(ctx, w.tasks, in.Goal, in.Question, backend.TerminalState{
		AgentState:    in.AgentState,
		RuntimeStatus: in.RuntimeStatus,
		WaitingReason: in.WaitingReason,
		LastOutputAt:  in.LastOutputAt,
	}, in.Tail)
	if err != nil {
		return domain.ModelJudgeAnswer{Reason: "Could not evaluate the question.", Confidence: 0.3, Matched: false}, nil
	}
	return ans, nil
}

// itoa is a tiny base-10 int formatter shared across the app package (avoids a
// strconv import in the hot status/drift paths).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
