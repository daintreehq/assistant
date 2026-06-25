package agent

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// Consumer-defined seams. The loop depends on these narrow interfaces, not the
// concrete provider packages, so it compiles, tests, and stays decoupled. Each is
// satisfied by the real provider (e.g. *models.Router, *tools.Registry,
// *skills.SkillRegistry, *storage.Store) and trivially by a fake in tests.

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

// SkillSelector is the skill.find engine's selector seam (satisfied by a thin
// adapter over skills.SelectSkills). Returns a validated SkillSelection or an
// error (incl. cancellation) so findSkills can leave the loaded set unchanged.
type SkillSelector interface {
	Select(ctx context.Context, candidates []skills.SkillMetadata, query string) (skills.SkillSelection, error)
}

// SkillCatalog is the registry seam for skill metadata/bodies (satisfied by
// *skills.SkillRegistry).
type SkillCatalog interface {
	MetadataForSelection() []skills.SkillMetadata
	GetMany(ids []string) []skills.Skill
	Has(id string) bool
}

// MessageStore is the persistence seam (satisfied by *storage.Store). All writes
// are best-effort — a DB failure must never break a live turn.
type MessageStore interface {
	InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error)
	InsertSkillSelection(rec domain.SkillSelectionLogRecord) (domain.SkillSelectionLogRecord, error)
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

// SessionDeps is the AgentSession constructor input. restoredMessages != nil ⇒ a
// resumed session: the three control messages are rebuilt fresh (so the cached
// prefix stays byte-stable) but NOT re-persisted (they already exist in the DB);
// seq continues from InitialSeq. A nil RestoredMessages is a fresh session
// (controls persisted).
type SessionDeps struct {
	Router        Router
	Tools         ToolRunner
	SkillSelector SkillSelector
	SkillCatalog  SkillCatalog
	Store         MessageStore
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
	// SessionEndedWatchers returns the titles of watchers a prior session left running
	// that this session's store-open had to cancel (watchers are session-scoped). The
	// footer surfaces them as a one-time `# Session note` on the FIRST turn only, then
	// never again — replacing the old message[1] one-time NOTE without a
	// RefreshRuntimeContext consume. A provider func (not a slice) so the app seam can
	// scheduler-gate it dynamically: it returns nil on non-interactive paths where
	// re-creating watchers is moot, even though the slice is known at construction.
	// Optional; nil ⇒ the note never appears (the default in tests).
	SessionEndedWatchers func() []string
	// ArtifactPersister mirrors overflow tool-result payloads to durable storage so
	// artifact.read survives cache eviction/restart (optional; nil ⇒ in-memory only).
	ArtifactPersister ArtifactPersister
	// WorkflowRunLister feeds the turn footer's active-workflow-runs block (optional;
	// nil ⇒ the block is omitted). Read-only, best-effort, never breaks the turn.
	WorkflowRunLister WorkflowRunLister
	PromptContext     prompts.MainPromptContext
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
}
