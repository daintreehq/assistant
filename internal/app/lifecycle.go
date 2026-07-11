package app

import (
	"context"
	"net/url"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/queue"
	"github.com/daintreehq/daintree-assistant/internal/storage"
)

// ConnectMcp connects the MCP transport, rolls up any tool drift to one log line,
// and refreshes the startup context cache. The runtime context is sent fresh as
// structured data on every backend round (built from PromptContext), so there is no
// session message to rewrite here anymore.
func (a *App) ConnectMcp(ctx context.Context) mcp.Status {
	a.mcpLifecycleMu.Lock()
	defer a.mcpLifecycleMu.Unlock()
	attemptCtx, cancelAttempt := boundedMCPConnectContext(ctx)
	defer cancelAttempt()

	// Kick off the docs-MCP handshake in PARALLEL (fire-and-forget) so it rides alongside
	// the Daintree connect during the boot splash WITHOUT sitting on the critical path —
	// a slow/unreachable public docs server must never add latency to boot, one-shot runs,
	// doctor, or /reconnect. The docs tools fail cleanly with MCP_UNAVAILABLE until it lands.
	a.connectDocsAsync(false)
	wasConnected := a.MCP.Status().Connected
	st := a.MCP.Connect(attemptCtx)
	a.logMcpConnectDiagnostics(st)
	a.warnOnDrift(st)
	if st.Connected && !wasConnected && a.Session != nil {
		// Warm the live-terminal inventory alongside the other splash reads so a fast
		// first user turn does not have to start from an empty roster.
		a.Session.WarmOpenTerminals()
	}
	a.ensureStartupContext(attemptCtx, st.Connected)
	final := a.MCP.Status()
	// Boot-only: reconcile the durable ledger against the live terminal inventory on
	// the first successful connect (idempotency-guarded inside).
	a.maybeReconcileLedger(attemptCtx, final.Connected)
	if ctx.Err() == nil {
		// A normal completed/timeout attempt fails open permanently for ordinary turns;
		// manual /reconnect owns later retries. An externally cancelled launch leaves this
		// false so a later live bootstrap/turn can still retry once.
		a.startupConnectAttempted = true
	}
	return final
}

// ReconnectMcp re-establishes the transport.
func (a *App) ReconnectMcp(ctx context.Context) mcp.Status {
	a.mcpLifecycleMu.Lock()
	defer a.mcpLifecycleMu.Unlock()
	attemptCtx, cancelAttempt := boundedMCPConnectContext(ctx)
	defer cancelAttempt()

	// Reconnect the docs MCP in parallel too (fire-and-forget), so /reconnect refreshes
	// BOTH transports without the docs link gating the primary status it returns.
	a.connectDocsAsync(true)
	st := a.MCP.Reconnect(attemptCtx)
	a.logMcpConnectDiagnostics(st)
	a.warnOnDrift(st)
	if st.Connected && a.Session != nil {
		a.Session.WarmOpenTerminals()
	}
	a.refreshStartupContext(attemptCtx, st.Connected)
	final := a.MCP.Status()
	// If the initial connect never succeeded, the boot reconcile runs on this first
	// successful (re)connect instead; the once-guard makes a later reconnect a no-op.
	a.maybeReconcileLedger(attemptCtx, final.Connected)
	if ctx.Err() == nil {
		a.startupConnectAttempted = true
	}
	return final
}

const primaryMCPConnectBudget = 8 * time.Second

// boundedMCPConnectContext uses cancellation rather than a deadline so the MCP client
// treats the budget as an intentional abort, not a connection-degrading tool failure.
// The budget spans handshake, stable discovery, and boot reconcile; an unreachable host
// can therefore never pin a user turn indefinitely.
func boundedMCPConnectContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	timer := time.AfterFunc(primaryMCPConnectBudget, cancel)
	return ctx, func() {
		timer.Stop()
		cancel()
	}
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

// logMcpConnectDiagnostics writes one connection-diagnostics line to the debug log on
// every (re)connect: the MCP URL host, whether the connect succeeded, the transport,
// and the bearer token's PRESENCE + LENGTH only. No token-derived material is logged —
// not even a truncated hash: a short fingerprint is an offline verification oracle for
// a low-entropy token (a log holder can hash candidate tokens and compare), while an
// integer length leaks nothing usable. Presence + length still answers the diagnostic
// questions ("is a token configured at all?", "did a rotation to a different-length
// credential land?"). The raw token (and the URL path/query, which can embed
// per-session secrets) must never be logged, even into the 0600 per-session debug log.
func (a *App) logMcpConnectDiagnostics(st mcp.Status) {
	rawURL := a.Config.McpURL
	if rawURL == "" {
		rawURL = st.URL // fall back to the resolved/connected URL
	}
	if rawURL == "" && a.Config.McpToken == "" {
		return // nothing configured — don't emit a useless line
	}
	host := ""
	if u, err := url.Parse(rawURL); err == nil {
		host = u.Host
	}
	debuglog.LogDebug(
		debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		"mcp.connect",
		map[string]any{
			"host":         host,
			"tokenPresent": a.Config.McpToken != "",
			"tokenLength":  len(a.Config.McpToken),
			"connected":    st.Connected,
			"transport":    st.Transport,
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
	// The async coordinator shares the scheduler's lifecycle in THIS process
	// (whichever owner is running — an open assistant or the supervisor daemon);
	// its Start adopts the persisted live invocations. Started AFTER a.scheduler
	// is set so the coordinator's Notify hook always sees it.
	if a.asyncCoordinator != nil {
		a.asyncCoordinator.Start(ctx)
	}
	return a.scheduler
}

// ClearWatchers tears down ALL live watchers in this session — revokes their
// grants, cancels them, and resolves their open attention events — for a completely
// clean slate. Used by /clear, which is now the ONLY wholesale watcher teardown
// (watchers are project-scoped and otherwise survive process boundaries). The
// actual Daintree terminals keep running; the assistant simply stops supervising
// them. Returns how
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

// ClearAsyncWork cancels every live async invocation for /clear's clean slate —
// a cleared conversation must never be woken by a completion for work only the
// old conversation understood. One bulk statement flips the DB rows (no partial
// mixed state), and the coordinator's CancelAll drops EVERY tracked entry AND
// bumps the clear generation — which also retracts the in-flight window where a
// pass finalized a group just before the clear but hasn't published yet (the
// pass re-checks the generation right before Queue.Publish). The watched
// terminals keep running (Daintree owns them); the assistant just stops
// following. Returns how many rows were cancelled. Best-effort: the error is
// returned for logging only.
func (a *App) ClearAsyncWork() (int, error) {
	ids, err := a.Store.CancelLiveAsyncInvocations(domain.NowMS(), storage.ReasonSessionCleared)
	if a.asyncCoordinator != nil {
		a.asyncCoordinator.CancelAll()
	}
	return len(ids), err
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
	if h.AskChoice != nil {
		a.hooks.AskChoice = h.AskChoice
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
	if a.asyncCoordinator != nil {
		a.asyncCoordinator.Stop()
		a.asyncCoordinator.Drain()
	}
	if a.scheduler != nil {
		a.scheduler.Stop()
		a.scheduler.Drain()
	}
	debuglog.BootTrace("shutdown.engines.stopped")
	// Join the session's detached distill goroutine BEFORE closing the Router/Store it
	// writes to. baseCancel above already cancelled its context, so this returns fast.
	if a.Session != nil {
		a.Session.DrainBackgroundWork()
	}
	debuglog.BootTrace("shutdown.session.drained")
	// Network teardown is DETACHED: both MCP closes are pure connection teardown with
	// zero store coupling, and by this point nothing uses either client (baseCancel +
	// the drains above joined every consumer). Blocking on them held the measured
	// ~250ms (worst-case 2s) docs-server round trip inside BOTH the one-shot exit AND
	// the daemon's attach handover — the lease release sat behind a public-internet
	// close. Only the store close below must stay synchronous (the next owner may
	// open the DB the instant the caller releases the flock). The goroutines are
	// self-terminating; a wedged endpoint leaks one goroutine bounded by process
	// exit / TCP teardown, the same abandonment policy closeMCPWithTimeout already
	// accepted.
	if a.MCP != nil {
		go func(c *mcp.Client) { _ = c.Close() }(a.MCP)
	}
	if a.DocsMCP != nil {
		// Join any in-flight docs (re)connect BEFORE closing — baseCancel above already
		// aborted it, so the join is fast — preserving the "never install a session
		// after Close" ordering, just off the caller's critical path.
		go func(c *mcp.Client) {
			a.docsConnectWG.Wait()
			closeMCPWithTimeout(c, docsCloseTimeout)
		}(a.DocsMCP)
	}
	debuglog.BootTrace("shutdown.mcp.detached")
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
func (a daemonQueueAdapter) MarkNotified(evs []domain.QueueEvent) error {
	return a.q.MarkNotified(context.Background(), evs)
}

// RearmAttention durably re-arms delivered attention events (nulls notifiedAt in
// the project store) so the next owner's notify pass re-digests and re-delivers
// them. The embedded host calls it when a shutdown/hibernate cancels a wake turn
// mid-flight — the burst was already marked notified when it was handed to the
// dying process, so without the re-arm it would be lost across the restart.
func (a *App) RearmAttention(ids []string) error {
	return a.Store.ClearNotified(ids)
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
