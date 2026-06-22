package daemon

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// The daemon talks to four other subsystems (store, queue, MCP transport, small
// model router) and a tool registry. Per the consumer-defined-interface idiom we
// declare SMALL local interfaces here for exactly the methods we call — the
// daemon never imports another Phase-C package, so it compiles in isolation. An
// integrator wires the concrete storage/queue/mcp/models/tools types to these.

// Store is the persistence surface the daemon uses. Methods mirror the SQLite
// contract in docs/port/daemon.md §4.4. patch maps are column→value; the storage
// layer allowlists columns (TIMER_UPDATE_COLS / WATCHER_UPDATE_COLS).
type Store interface {
	// DueTimers returns scheduled timers with fireAt<=now, ordered by fireAt,
	// each at most once per tick (key to sleep-catchup single-fire).
	DueTimers(now int64) ([]domain.TimerRecord, error)
	// UpdateTimer applies an allowlisted column patch to a timer row.
	UpdateTimer(id string, patch map[string]any) error
	// ClaimDueTimer atomically applies the patch ONLY while the timer is still the due row
	// the scheduler read (status 'scheduled' AND fireAt == expectFireAt); returns true iff a
	// row matched. A false return means it was cancelled/edited concurrently — skip firing.
	ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error)
	// DueWatchers returns active watchers with nextCheckAt<=now, ordered by
	// nextCheckAt.
	DueWatchers(now int64) ([]domain.WatcherRecord, error)
	// UpdateWatcher applies an allowlisted column patch to a watcher row.
	UpdateWatcher(id string, patch map[string]any) error
	// ClaimDueWatcher atomically applies the patch ONLY while the watcher is still 'active';
	// returns true iff a row matched. A false return means it was cancelled concurrently — the
	// daemon must not re-arm it (which would resurrect a cancelled watcher).
	ClaimDueWatcher(id string, patch map[string]any) (bool, error)
	// RevokeGrantsByActor revokes all live automation grants for an actor id
	// (called on every timer/watcher terminal state). Returns rows changed.
	RevokeGrantsByActor(actorID string, now int64) (int, error)
}

// Queue is the attention-queue surface the daemon publishes to and reads from in
// notify(). Publish dedupes by dedupeKey; Digest filters open events; MarkNotified
// stamps notifiedAt.
type Queue interface {
	Publish(args domain.QueuePublishArgs) error
	Digest(opts domain.QueueDigestOptions) ([]domain.QueueEvent, error)
	MarkNotified(ids []string) error
}

// MCPResult is the raw MCP tool-call result the watcher engines parse. Daintree
// returns the payload in BOTH structuredContent AND a JSON `text` body — the
// engines read both and merge (reading structuredContent alone silently saw zero
// terminals every tick). The concrete MCP client adapts its result onto this.
type MCPResult struct {
	// StructuredContent is the server's structuredContent object (may be nil).
	StructuredContent map[string]any
	// Text is the raw text content body (often the JSON Daintree actually
	// populated). Empty when absent.
	Text string
	// IsError reports a tool-level error result.
	IsError bool
}

// MCP is the read-only MCP transport the watcher engines call. Only idempotent
// reads pass through here (getStatus/getOutput/list/git pulse/forge.getPR) — never
// a mutation. CallRead bounds each attempt via ctx and retries a transient
// transport hiccup internally (the concrete client owns the retry/timeout policy;
// see McpReadMaxRetries / McpReadTimeoutMS).
type MCP interface {
	// CallRead invokes a read-only MCP tool by name with JSON-marshalable args.
	CallRead(ctx context.Context, name string, args map[string]any) (MCPResult, error)
	// Connected reports whether the client currently has a live session.
	Connected() bool
}

// WatcherModel is the small-model seam the terminal watcher uses. The prompt
// construction (WATCHER_SYSTEM_PROMPT / JUDGE_SYSTEM_PROMPT and the user-prompt
// builders) lives in the models subsystem; the daemon hands it the structured
// inputs and receives the decoded JSON verdict/answer. Both calls run at
// temperature 0 against rec.modelTier. On the concrete side a failure degrades to
// the documented fallback rather than propagating.
type WatcherModel interface {
	// Classify runs the terminal-state classification (WatcherVerdict schema).
	// previous is the prior classification (may be empty). On error the
	// implementation SHOULD return the documented fallback
	// (unknown / 0.3 / "Could not classify terminal output."), but the engine
	// also treats a returned error defensively.
	Classify(ctx context.Context, in ClassifyInput) (domain.WatcherVerdict, error)
	// Judge answers ONE yes/no question (ModelJudgeAnswer schema). Field order
	// reason→matched is load-bearing (implicit chain-of-thought).
	Judge(ctx context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error)
}

// ClassifyInput carries everything buildWatcherUserPrompt needs. LastOutputAt is
// the humanized "<N>s ago" string (empty when msSinceOutput was unobserved).
type ClassifyInput struct {
	Tier          domain.ModelTier
	Goal          string
	AgentState    string
	RuntimeStatus string
	LastOutputAt  string
	Previous      string
	Tail          string
}

// JudgeInput carries everything buildJudgeUserPrompt needs for one question.
type JudgeInput struct {
	Tier          domain.ModelTier
	Question      string
	Goal          string
	AgentState    string
	RuntimeStatus string
	WaitingReason string
	LastOutputAt  string
	Tail          string
}

// Registry runs a call_safe_tool timer payload through the tool registry as a
// non-interactive "timer" actor. Matches the dispatch surface in
// internal/ports.ToolRegistry but is declared locally to keep the daemon
// independent. actorID is the firing timer's id, threaded so a scoped automation
// grant (which keys on the actor id) can be matched and consumed — without it a
// confirm-required timer action is always denied despite a valid grant.
type Registry interface {
	Dispatch(ctx context.Context, actor domain.ToolActor, actorID, name, argsJson string) (domain.ToolResult, error)
}

// CheckContext bundles the per-actor dependencies a watcher/timer check needs.
// It is the Go analogue of the TS ToolContext, narrowed to what the daemon uses.
// ctxFor (SchedulerDeps.CtxFor) builds one for a given non-interactive actor.
type CheckContext struct {
	// Ctx carries cancellation (extraction polls supply a cancellable context;
	// watcher/timer ticks pass a background context).
	Ctx context.Context
	// Store is the persistence surface.
	Store Store
	// Queue is the attention queue.
	Queue Queue
	// MCP is the read-only MCP transport.
	MCP MCP
	// Model is the small-model classifier/judge.
	Model WatcherModel
	// ProjectPath is the bound project working directory (forge.getPR default cwd).
	ProjectPath string
}
