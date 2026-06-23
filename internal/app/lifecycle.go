package app

import (
	"context"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/queue"
)

// ConnectMcp connects the MCP transport, rolls up any tool drift to one log line,
// and refreshes the session's runtime context.
func (a *App) ConnectMcp(ctx context.Context) mcp.Status {
	st := a.MCP.Connect(ctx)
	a.warnOnDrift(st)
	a.Session.RefreshRuntimeContext(a.PromptContext())
	return st
}

// ReconnectMcp re-establishes the transport.
func (a *App) ReconnectMcp(ctx context.Context) mcp.Status {
	st := a.MCP.Reconnect(ctx)
	a.warnOnDrift(st)
	a.Session.RefreshRuntimeContext(a.PromptContext())
	return st
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
		Store:       a.Store,
		Queue:       daemonQueueAdapter{q: a.Queue},
		Registry:    daemonRegistryAdapter{app: a},
		CtxFor:      a.daemonCtxFor,
		OnAttention: onAttention,
	})
	a.scheduler.Start(ctx)
	a.Session.RefreshRuntimeContext(a.PromptContext())
	return a.scheduler
}

// daemonCtxFor builds a per-actor daemon.CheckContext. The
// non-interactive actor reaches read-only MCP + the small-model classifier/judge.
func (a *App) daemonCtxFor(ctx context.Context, actor domain.ToolActor, actorID string) *daemon.CheckContext {
	return &daemon.CheckContext{
		Ctx:         ctx,
		Store:       a.Store,
		Queue:       daemonQueueAdapter{q: a.Queue},
		MCP:         daemonMcpAdapter{c: a.MCP},
		Model:       watcherModelAdapter{router: a.Router},
		ProjectPath: a.Config.ProjectPath,
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
	// (terminal.extract.async) stop touching MCP/Router/Store before we close them.
	if a.baseCancel != nil {
		a.baseCancel()
	}
	if a.scheduler != nil {
		a.scheduler.Stop()
		a.scheduler.Drain()
	}
	if a.MCP != nil {
		_ = a.MCP.Close()
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

// watcherModelAdapter satisfies daemon.WatcherModel using the small-model router +
// the watcher/judge prompt builders. Both calls run at temperature 0 against the
// watcher's tier. A model/decode failure degrades to the documented fallback
// rather than propagating (the engine also treats a returned error defensively).
type watcherModelAdapter struct{ router *models.Router }

func (w watcherModelAdapter) Classify(ctx context.Context, in daemon.ClassifyInput) (domain.WatcherVerdict, error) {
	fallback := domain.WatcherVerdict{
		Classification:    domain.ClassUnknown,
		Confidence:        0.3,
		Summary:           "Could not classify terminal output.",
		Evidence:          []string{},
		RecommendedAction: domain.ActionNone,
	}
	user := prompts.BuildWatcherUserPrompt(prompts.WatcherUserArgs{
		Goal:          in.Goal,
		AgentState:    in.AgentState,
		RuntimeStatus: in.RuntimeStatus,
		LastOutputAt:  in.LastOutputAt,
		Previous:      in.Previous,
		Tail:          in.Tail,
	})
	raw, err := w.callJSON(ctx, in.Tier, prompts.WatcherSystemPrompt, user)
	if err != nil {
		return fallback, nil
	}
	v, err := models.DecodeWatcherVerdict(raw)
	if err != nil {
		return fallback, nil
	}
	return v, nil
}

func (w watcherModelAdapter) Judge(ctx context.Context, in daemon.JudgeInput) (domain.ModelJudgeAnswer, error) {
	fallback := domain.ModelJudgeAnswer{
		Reason:     "Could not evaluate the question.",
		Confidence: 0.3,
		Matched:    false,
	}
	user := prompts.BuildJudgeUserPrompt(prompts.JudgeUserArgs{
		Question:      in.Question,
		Goal:          in.Goal,
		AgentState:    in.AgentState,
		RuntimeStatus: in.RuntimeStatus,
		WaitingReason: in.WaitingReason,
		LastOutputAt:  in.LastOutputAt,
		Tail:          in.Tail,
	})
	raw, err := w.callJSON(ctx, in.Tier, prompts.JudgeSystemPrompt, user)
	if err != nil {
		return fallback, nil
	}
	ans, err := models.DecodeModelJudgeAnswer(raw)
	if err != nil {
		return fallback, nil
	}
	return ans, nil
}

// callJSON runs one temperature-0 JSON request with a system+user prompt. These
// run on daemon goroutines, off any user turn — their usage accumulates in the
// SAME shared Router meter and is attributed to whichever turn's emitUsage drains
// it next (an idle session with no following turn simply discards it). That
// approximate per-turn attribution is the accepted trade-off for a running
// session-cost estimate (see internal/models/usage.go).
func (w watcherModelAdapter) callJSON(ctx context.Context, tier domain.ModelTier, system, user string) (string, error) {
	temp := 0.0
	return w.router.JSON(ctx, tier, models.ChatOptions{
		Messages: []models.ChatMessage{
			models.TextMessage("system", system),
			models.TextMessage("user", user),
		},
		Temperature: &temp,
	})
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
