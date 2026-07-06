package agent

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
)

// Consumer-defined seams. The loop depends on these narrow interfaces, not the
// concrete provider packages, so it compiles, tests, and stays decoupled. Each is
// satisfied by the real provider (e.g. *backend.Client, *tools.Registry,
// *storage.Store) and trivially by a fake in tests.

// Router is the model-access seam (satisfied by *models.Router). The loop only
// streams the large model and chats the small model (auto-compact + skill
// selector). Stream's ChatResult/ChatOptions/ChatTool come from internal/models.
type Router interface {
	Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error)
	Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error)
	ModelFor(tier domain.ModelTier) string
	// FlushMeter drains the Router's accumulated per-tier usage since the last
	// flush. emitUsage calls it once per streamed round so the UsageEvent sums
	// EVERY model call in the turn (large stream + small-tier background work),
	// not just the large-thread stream result.
	FlushMeter() []models.TierUsage
}

// ToolRunner is the tool-registry seam (satisfied by an adapter over
// *tools.Registry). It projects tools to OpenAI specs, resolves wire→internal
// names, and dispatches a call for this turn (carrying the per-turn
// runId/signal/allowed-names through the registry's ToolContext).
type ToolRunner interface {
	// OpenAITools projects the (optionally filtered) tools to model-facing specs.
	// filterNames are INTERNAL dotted names; nil ⇒ the full registry.
	OpenAITools(filterNames []string) ([]models.ChatTool, error)
	// ResolveWireName maps a wire name back to its internal name (from the most
	// recent OpenAITools projection); "" when unknown.
	ResolveWireName(wireName string) string
	// Dispatch runs one tool call by internal name for the given turn. The runner
	// owns building the per-call ToolContext from runCtx (runId, signal, allowed
	// names). It NEVER returns an error — every failure is a domain.ToolResult.
	Dispatch(ctx context.Context, name string, argsJSON string, turn TurnContext) domain.ToolResult
}

// TurnContext carries the per-turn fields the ToolRunner stamps onto each call's
// ToolContext (runId, allowed-tool projection). The signal is the dispatch ctx.
//
// CallID + Progress are filled PER CALL (re-stamped before each Dispatch in the
// batch): the runner wires Progress into the registry's ReportProgress so an
// in-tool substep ("launching terminal") reaches the live footer tagged with the
// active call's id. Progress is "" on the no-op turns (tests); the runner is
// nil-safe.
type TurnContext struct {
	RunID           string
	ActiveToolNames []string
	// CallID identifies the call currently being dispatched (the live footer row).
	CallID string
	// Progress forwards an in-tool substep message for CallID. Nil-safe.
	Progress func(callID string, msg string)
}

// AssistantBackend is the native Daintree backend seam (satisfied by
// *backend.Client). The turn loop streams a respond round through RespondStream;
// utility/compaction work runs server-owned tasks through RunTask. The CLI sends
// only the visible conversation + structured context; the backend owns the system
// prompt, skill selection, and model routing. A narrow interface (no generic
// Chat/JSON) is deliberate: generic prompt calls are exactly what this migration
// removed.
type AssistantBackend interface {
	RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error)
	RunTask(ctx context.Context, req backend.TaskRequest) (backend.TaskResult, error)
}

// MessageStore is the persistence seam (satisfied by *storage.Store). All writes
// are best-effort — a DB failure must never break a live turn.
type MessageStore interface {
	InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error)
}

// MemoryStore is the distill-on-compact persistence seam (satisfied by
// *storage.Store). Just before auto-compact discards the working history, the session
// extracts durable facts and saves the novel ones via this seam. Optional: a nil
// MemoryStore disables distillation entirely (the default in tests, so the extra
// model call never fires). All writes are best-effort — a failure must never break
// compaction.
type MemoryStore interface {
	InsertMemory(rec domain.MemoryRecord) (domain.MemoryRecord, error)
	MemoryExists(content string) (bool, error)
}

// WorkflowRunLister is the read-only ledger seam for the turn footer: it returns
// the open (non-terminal) workflow runs so the model gets a compact, always-current
// view of its own active work and open branches, rebuilt every round at the uncached
// tail. Optional: a nil lister omits the workflow block entirely (the default in
// tests). The read is best-effort and synchronous (a sub-ms local SQLite query, not a
// network call) so — like MemoryStore/ArtifactPersister — it carries NO context and
// must never block or break the turn; the caller swallows any error to nil.
type WorkflowRunLister interface {
	ListNonTerminalWorkflowRuns(limit int) ([]domain.WorkflowRunRecord, error)
}

// AsyncInvocationLister is the read-only async-futures seam for the turn
// context: it returns the live (non-terminal) async invocations so the model
// sees its own in-flight async operations every round as inert data — it can
// reference them, cancel them, and (crucially) NOT re-issue them after a
// compaction or a long gap. Optional: a nil lister omits the block (the default
// in tests). Best-effort local read; never blocks or breaks the turn.
type AsyncInvocationLister interface {
	ListLiveAsyncInvocations() ([]domain.AsyncInvocationRecord, error)
}

// WorkflowDigestLister is the read-only workflow-intelligence seam for the
// turn context (satisfied by *workflowgraph.Service): it renders the open
// execution graphs as bounded, prompt-ready digests, re-read every round like
// the async ledger, so the model always sees what it already planned/did/is
// waiting on and never redoes completed work after a compaction or wake.
// Optional: nil (the default, and always when DAINTREE_WORKFLOW_INTELLIGENCE
// is off) omits the workflow_state block entirely — the wire stays
// byte-identical to before the feature. Best-effort local read; never blocks
// or breaks the turn.
type WorkflowDigestLister interface {
	WorkflowDigests(limit int) []backend.WorkflowDigest
}

// MemoryRecaller is the per-turn BM25 recall seam (satisfied by an adapter over
// *storage.Store). At the START of every turn the session runs ONE keyword recall
// seeded by the user's originating ask and injects the top hits into the merged
// `# Pinned and relevant memories` footer block (the `## Relevant` subblock) — so
// distilled, non-pinned facts resurface automatically without the model having to call
// the recall tool. The narrow (query, limit) signature deliberately keeps
// storage.MemoryRecallOptions out of the agent package. Optional: a nil MemoryRecaller
// omits the subblock (the default in tests). All recall is best-effort — a failure must
// never break a turn.
type MemoryRecaller interface {
	RecallMemories(query string, limit int) ([]domain.MemoryRecord, error)
}

// PinnedMemoryLister is the read-only pinned-memory seam for the turn footer (satisfied
// by an adapter over *storage.Store). The session re-reads the current pins EVERY round
// and renders them in the footer's merged `# Pinned and relevant memories` block, so a
// pin surfaces without the model calling a tool — and, because the read is per-round
// (not per-turn like recall), a memory.pin landing mid-turn shows on the very next round,
// which is why the migration out of message[1] needs no RefreshRuntimeContext on pin. The
// narrow (limit) signature keeps storage.MemoryListOptions out of the agent package.
// Optional: a nil lister omits the pinned subblock (the default in tests). All reads are
// best-effort — a failure must never break a turn.
type PinnedMemoryLister interface {
	ListPinnedMemories(limit int) ([]domain.MemoryRecord, error)
}

// ArtifactPersister is the durable-mirror seam for oversized tool-result overflow
// payloads (satisfied by *storage.Store). When a serialized result overflows the
// inline cap the session stashes the full envelope in its bounded in-memory
// ArtifactStore AND mirrors it here, so a later artifact.read resolves even after
// the id is evicted from the 64-entry hot cache or the process restarts. Optional:
// a nil persister keeps the store purely in-memory (the default in tests). All
// writes are best-effort — a failure must never break the turn.
type ArtifactPersister interface {
	InsertArtifact(rec domain.ArtifactRecord) (domain.ArtifactRecord, error)
}

// BackendStateStore is the durable mirror for the opaque backend state token
// (satisfied by *storage.Store). The token is server-signed skill-selection
// state, refreshed on every stream meta event; mirroring it lets a DIFFERENT
// process (the supervisor daemon after a cockpit detach, or vice versa) resume
// the session with the backend's selector cadence intact instead of forcing a
// from-scratch re-selection. Optional: nil keeps the token memory-only (the
// default in tests). All writes are best-effort — never break a stream.
type BackendStateStore interface {
	PutSessionBackendState(sessionID, token string) error
}

// SessionDeps is the AgentSession constructor input. RestoredMessages != nil ⇒ a
// resumed session: the restored visible history (user/assistant/tool only) is the
// starting point and seq continues from InitialSeq. A nil RestoredMessages is a
// fresh session, which starts with an empty visible history (the backend owns all
// system text, so there is no client-side control prefix to persist).
type SessionDeps struct {
	// Backend is the native Daintree backend (assistant turns + utility tasks).
	Backend AssistantBackend
	// Router is the legacy model seam, retained ONLY for the compaction/distill
	// utility calls that have not yet moved to backend tasks; the main turn loop no
	// longer uses it. Optional once those move (a nil Router disables them).
	Router Router
	Tools  ToolRunner
	Store  MessageStore
	// MemoryStore enables distill-on-compact (optional; nil ⇒ disabled).
	MemoryStore MemoryStore
	// MemoryRecaller enables per-turn BM25 recall into the footer (optional; nil ⇒ the
	// recalled subblock of the merged memories section is omitted, the default in tests).
	MemoryRecaller MemoryRecaller
	// PinnedMemoryLister feeds the footer's merged memories block with the current pinned
	// project memories, re-read every round (optional; nil ⇒ the pinned subblock is
	// omitted, the default in tests). Best-effort, never breaks the turn.
	PinnedMemoryLister PinnedMemoryLister
	// ActiveWorktreeFunc returns the current active-worktree label for the footer's
	// `# Active worktree` section, called every round so a mid-turn worktree switch
	// surfaces next round (optional; nil ⇒ the section is omitted). It replaces the old
	// message[1] "Active worktree:" line so a worktree switch no longer rewrites the
	// cached runtime context. Must not block — it reads a cached label, not MCP.
	ActiveWorktreeFunc func() string
	// ResumedWatchers returns the titles of live watchers this process ADOPTED from a
	// prior owner at ownership boot (watchers are project-scoped and keep running
	// across restarts/detach). The footer surfaces them as a one-time `# Session note`
	// on the FIRST turn only, then never again. A provider func (not a slice) so the
	// app seam can scheduler-gate it dynamically: it returns nil on non-interactive
	// paths where the note is moot, even though the slice is known at construction.
	// Optional; nil ⇒ the note never appears (the default in tests).
	ResumedWatchers func() []string
	// ArtifactPersister mirrors overflow tool-result payloads to durable storage so
	// artifact.read survives cache eviction/restart (optional; nil ⇒ in-memory only).
	ArtifactPersister ArtifactPersister
	// BackendStateStore mirrors the opaque backend state token to durable storage on
	// every stream meta so a cross-process session handover (cockpit ↔ supervisor
	// daemon) replays it (optional; nil ⇒ memory-only, the default in tests).
	BackendStateStore BackendStateStore
	// InitialBackendState seeds the opaque token on construction — the persisted
	// value from the previous owner of this session ("" for a fresh session).
	InitialBackendState string
	// WorkflowRunLister feeds the turn footer's active-workflow-runs block (optional;
	// nil ⇒ the block is omitted). Read-only, best-effort, never breaks the turn.
	WorkflowRunLister WorkflowRunLister
	// AsyncInvocationLister feeds the turn context's active-async-operations block,
	// re-read every round like the workflow ledger (optional; nil ⇒ omitted).
	AsyncInvocationLister AsyncInvocationLister
	// WorkflowDigestLister feeds the turn context's workflow_state block with the
	// open execution-graph digests, re-read every round (optional; nil ⇒ omitted —
	// the default, and always when workflow intelligence is disabled).
	WorkflowDigestLister WorkflowDigestLister
	// OpenTerminalsFetcher returns a fresh, metadata-only snapshot of the open Daintree
	// terminals for the runtime block's open-terminal inventory. Called ONCE per turn
	// before the round loop (one terminal.list + one no-output terminal.getStatus) and
	// threaded through every round's buildRuntimeContext, so the model always sees the
	// live roster as inert data instead of tool-calling terminal.list to discover it.
	// Best-effort and bounded: nil ⇒ the inventory is omitted (the default in tests); a
	// slow/failed MCP read returns nil and never blocks the turn. The app wires this to
	// terminalReaderAdapter.FetchOpenTerminals.
	OpenTerminalsFetcher func(ctx context.Context) []backend.OpenTerminal
	// PromptContext is the seed/fallback runtime context (used when PromptContextFunc is
	// nil — the test default). The structured runtime block the backend receives each
	// round is built from this.
	PromptContext prompts.MainPromptContext
	// PromptContextFunc, when set, is read EVERY round to build the per-request runtime
	// context, so facts that populate after construction — a successful MCP connect, a
	// /permissions tier change, a started scheduler — reach the backend on the next round.
	// It replaces the old RefreshRuntimeContext push: the session now PULLS live context
	// instead of holding a boot snapshot. nil ⇒ the static PromptContext value is used
	// (tests). The app wires this to App.PromptContext.
	PromptContextFunc func() prompts.MainPromptContext
	SessionID         string

	// Resume discriminator: non-nil (even empty) ⇒ resumed; nil ⇒ fresh.
	RestoredMessages []models.ChatMessage
	InitialSeq       int
	// DirtyFreshStart ⇒ the resume was forced empty by a dup-seq tangle; NewSession
	// persists a clear breadcrumb at InitialSeq so the durable log records the reset.
	DirtyFreshStart bool
	// DroppedRehydrateRows is RehydrateResult.DroppedRows — how many corrupt/orphan
	// rows rehydration elided. When non-zero, the session emits one info event on the
	// first resumed turn so the silent drop becomes observable.
	DroppedRehydrateRows int

	Events EventSink // defaults to NoopEventSink
	RunRef *RunIDRef // stamped with the current run id per turn; defaults to a fresh ref

	// BackgroundCtx is the APP-SCOPED context for detached work that must OUTLIVE a
	// single turn (the post-compaction distill goroutine) but NOT outlive the app —
	// wire app.baseCtx so distill calls are cancelled on App.Shutdown rather than
	// touching a closed Router/Store. Optional; nil defaults to context.Background()
	// in NewSession (the test default, where no distill outlives the test).
	BackgroundCtx context.Context

	// Trace, when set, receives structured diagnostic events for the per-session debug
	// log: the turn lifecycle (turn.start/turn.end) and the backend respond round
	// (backend.respond.request/meta/done/error) — the trace gap the backend migration
	// left where the legacy router's model.request/model.response used to be. The app
	// wires it to debuglog.LogDebug, keeping the agent package free of any debuglog
	// dependency. Optional; nil ⇒ no tracing (the default in tests). It must never
	// block (the debug write is append-only and best-effort) and the session guards
	// every call against a panic — a logging failure can never break a live turn.
	Trace func(event string, fields map[string]any)
}
