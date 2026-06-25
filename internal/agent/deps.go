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
