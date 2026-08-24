package domain

// Persisted DB-row records. All *At fields are epoch-ms
// int64. Optionals are pointers. `*Json` fields hold a JSON-serialized string of
// the named shape; the storage layer (de)serializes them.

// TimerRecord is a scheduled timer (persists across sessions).
type TimerRecord struct {
	ID            string  `json:"id"`
	Title         string  `json:"title"`
	FireAt        int64   `json:"fireAt"`
	RepeatEveryMs *int64  `json:"repeatEveryMs,omitempty"`
	RepeatUntil   *int64  `json:"repeatUntil,omitempty"`
	MaxRuns       *int    `json:"maxRuns,omitempty"`
	RunCount      int     `json:"runCount"`
	PayloadType   string  `json:"payloadType"` // enqueue | run_check | call_safe_tool
	PayloadJson   string  `json:"payloadJson"`
	TargetJson    *string `json:"targetJson,omitempty"`
	Status        string  `json:"status"` // scheduled | fired | cancelled | done
	CreatedAt     int64   `json:"createdAt"`
	LastFiredAt   *int64  `json:"lastFiredAt,omitempty"`
}

// WatcherEndedConsumedInTurn is the WatcherRecord.EndedReason stamped when the
// main turn directly observed a supervised terminal's completion (terminal.awaitAll
// or a settled terminal.extract wait) and retired the now-redundant supervisor
// watcher. Declared in domain (not storage) because the daemon also matches on it:
// a stop publish that loses its finalize claim to a consumed row must resolve the
// event it just emitted (daemon/watcher.go).
const WatcherEndedConsumedInTurn = "consumed_in_turn"

// WatcherRecord supervises a terminal or PR. Project-scoped: a non-terminal row
// survives process boundaries and is adopted by the next owner (attached session or
// supervisor daemon) at ownership boot; /clear is the only wholesale teardown.
// Unknown kind fails closed to "error".
type WatcherRecord struct {
	ID                 string         `json:"id"`
	Kind               string         `json:"kind"` // terminal | pr_state
	Title              string         `json:"title"`
	Goal               string         `json:"goal"`
	TargetsJson        string         `json:"targetsJson"` // JSON string[]
	CadenceMs          int            `json:"cadenceMs"`
	IsSupervisor       *bool          `json:"isSupervisor,omitempty"`
	ModelTier          ModelTier      `json:"modelTier"`
	StartAfterMs       *int64         `json:"startAfterMs,omitempty"`
	StopAfterMs        *int64         `json:"stopAfterMs,omitempty"`
	StopWhenJson       *string        `json:"stopWhenJson,omitempty"`
	AlertWhenJson      *string        `json:"alertWhenJson,omitempty"`
	OptionsJson        *string        `json:"optionsJson,omitempty"`
	Status             string         `json:"status"` // created|active|paused|condition_met|timeout|cancelled|error
	LastClassification *string        `json:"lastClassification,omitempty"`
	LastEpistemicKind  *EpistemicKind `json:"lastEpistemicKind,omitempty"`
	LastCheckedAt      *int64         `json:"lastCheckedAt,omitempty"`
	NextCheckAt        int64          `json:"nextCheckAt"` // required
	CreatedAt          int64          `json:"createdAt"`
	// EndedReason distinguishes a /clear teardown ("session_cleared", set by
	// CancelLiveWatchers), a deliberate user cancel ("user_cancelled", set by
	// watcher.cancel), and an in-turn consumption retirement ("consumed_in_turn",
	// set when the main turn directly observed the supervised completion via
	// terminal.awaitAll / a settled terminal.extract wait — that one lands with
	// status condition_met, since the supervised outcome WAS reached). nil on
	// active rows and on the daemon's own natural terminal states
	// (condition_met/timeout/error). EndedAt is when that end happened.
	EndedReason *string `json:"endedReason,omitempty"`
	EndedAt     *int64  `json:"endedAt,omitempty"`
	// WorkflowRunID back-links a supervisor watcher to the durable workflow ledger
	// row created when its work was spawned (nil for non-supervisor / manually-created
	// watchers). When set, the daemon advances that row's status as the watcher reaches
	// a terminal state (condition_met → done, timeout/error → failed).
	WorkflowRunID *string `json:"workflowRunId,omitempty"`
}

// AuditRecord is one row of the audit log.
type AuditRecord struct {
	ID          string                 `json:"id"`
	Ts          int64                  `json:"ts"`
	Actor       ToolActor              `json:"actor"`
	ToolName    string                 `json:"toolName"`
	ArgsJson    string                 `json:"argsJson"`
	Outcome     string                 `json:"outcome"` // ok|error|denied|dedup|grant_ok
	DurationMs  int64                  `json:"durationMs"`
	Summary     string                 `json:"summary"`
	ResultJson  *string                `json:"resultJson,omitempty"`
	GrantSource *AutomationGrantSource `json:"grantSource,omitempty"`
	GrantID     *string                `json:"grantId,omitempty"`
	RunID       *string                `json:"runId,omitempty"`
}

// RunEventRecord is a durable event in a turn's replay log.
type RunEventRecord struct {
	ID      string  `json:"id"` // rne_<uuid8>
	RunID   string  `json:"runId"`
	Seq     int     `json:"seq"` // from 0
	Ts      int64   `json:"ts"`
	Type    string  `json:"type"` // e.g. "assistant:start"
	Payload *string `json:"payload,omitempty"`
}

// RunSummaryRecord is computed (not persisted) over a run's events.
type RunSummaryRecord struct {
	RunID      string `json:"runId"`
	FirstTs    int64  `json:"firstTs"`
	LastTs     int64  `json:"lastTs"`
	EventCount int    `json:"eventCount"`
	// Label is the originating user prompt for the run, read back from its
	// turn:prompt event so /explain can show what prompted each run ("" when the
	// run has no turn:prompt event). Stored verbatim; the formatter truncates.
	Label string `json:"label,omitempty"`
}

// AutomationGrantRecord authorizes a non-interactive actor to run mutating tools.
// Authorization = tool-name in allowedToolNames OR risk in allowedRiskClasses
// (union); at least one list must be non-empty (enforced in code). revokedAt is
// an explicit revoke ONLY — use-exhaustion is usesRemaining==0 and does NOT
// stamp revokedAt.
type AutomationGrantRecord struct {
	ID                     string                   `json:"id"`
	ActorID                string                   `json:"actorId"` // wch_… | tmr_…
	ActorType              AutomationGrantActorType `json:"actorType"`
	AllowedRiskClassesJson *string                  `json:"allowedRiskClassesJson"` // string | null
	AllowedToolNamesJson   *string                  `json:"allowedToolNamesJson"`   // string | null
	ExpiresAt              int64                    `json:"expiresAt"`
	MaxUses                int                      `json:"maxUses"`
	UsesRemaining          int                      `json:"usesRemaining"`
	RevokedAt              *int64                   `json:"revokedAt"` // null until explicit revoke
	CreatedAt              int64                    `json:"createdAt"`
	Source                 AutomationGrantSource    `json:"source"`
}

// ConversationMessageRecord is one persisted conversation message.
type ConversationMessageRecord struct {
	ID        string `json:"id"` // msg_<uuid8>
	SessionID string `json:"sessionId"`
	Seq       int    `json:"seq"`
	Role      string `json:"role"` // system|user|assistant|tool
	Content   string `json:"content"`
	// Name is the message's wire `name`. Nil on every ordinary row; set only on the
	// server-delivered compacted context block, which carries the reserved
	// `daintree_compaction` — the marker the backend's span selector reads to find
	// where already-frozen history ends.
	Name *string `json:"name,omitempty"`
	// ReasoningContent persists an assistant turn's chain-of-thought so it survives
	// resume and replays correctly (DeepSeek 400s on a tool-call turn missing it).
	// Nil for non-assistant rows and for the default thinking-off posture.
	ReasoningContent *string `json:"reasoningContent,omitempty"`
	ToolCallsJson    *string `json:"toolCallsJson,omitempty"`
	ToolCallID       *string `json:"toolCallId,omitempty"`
	CreatedAt        int64   `json:"createdAt"`
}

// WorkflowRunRecord tracks an end-to-end workflow run.
type WorkflowRunRecord struct {
	ID                string            `json:"id"` // wfr_<uuid8>
	IssueNumber       *int              `json:"issueNumber,omitempty"`
	IssueURL          *string           `json:"issueUrl,omitempty"`
	IssueTitle        *string           `json:"issueTitle,omitempty"`
	Branch            *string           `json:"branch,omitempty"`
	WorktreeID        *string           `json:"worktreeId,omitempty"`
	PRNumber          *int              `json:"prNumber,omitempty"`
	PRURL             *string           `json:"prUrl,omitempty"`
	TerminalIdsJson   *string           `json:"terminalIdsJson,omitempty"`
	WatcherIdsJson    *string           `json:"watcherIdsJson,omitempty"`
	QueueEventIdsJson *string           `json:"queueEventIdsJson,omitempty"`
	Status            WorkflowRunStatus `json:"status"`
	NextActionJson    *string           `json:"nextActionJson,omitempty"` // serialized RecommendedAction
	NotesJson         *string           `json:"notesJson,omitempty"`      // JSON string[]
	CreatedAt         int64             `json:"createdAt"`
	UpdatedAt         int64             `json:"updatedAt"` // required
	CompletedAt       *int64            `json:"completedAt,omitempty"`
}

// WorkflowGraphRecord is the durable row of one workflow-intelligence execution
// graph (the DAG layer, distinct from the flat wfr_ workflow_runs ledger). The
// graph itself is stored as an opaque JSON snapshot (SnapshotJson, serialized by
// internal/workflowgraph) so the typed model can evolve without a schema change;
// the promoted columns (status/goal/revision) exist for cheap list/filter reads.
// Revision is the optimistic-concurrency counter: every snapshot write must name
// the revision it read, and a mismatch is a typed conflict — never a silent
// last-writer-wins over a backend-computed patch.
type WorkflowGraphRecord struct {
	ID            string `json:"id"` // wfg_<uuid8>
	Status        string `json:"status"`
	Goal          string `json:"goal"`
	SchemaVersion int    `json:"schemaVersion"`
	Revision      int64  `json:"revision"`
	SnapshotJson  string `json:"snapshotJson"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
	CompletedAt   *int64 `json:"completedAt,omitempty"`
}

// WorkflowGraphEventRecord is one append-only projection event over a workflow
// graph: plan created, patch applied, evidence recorded, resource linked, node
// transitioned, async settled. Revision is the graph revision AFTER the event's
// write (0 for events that did not bump the snapshot). PayloadHash is a stable
// content hash for dedupe/inspection when the payload is archived elsewhere.
type WorkflowGraphEventRecord struct {
	ID          string  `json:"id"` // wge_<uuid8>
	WorkflowID  string  `json:"workflowId"`
	Revision    int64   `json:"revision"`
	Kind        string  `json:"kind"`
	NodeID      *string `json:"nodeId,omitempty"`
	Summary     string  `json:"summary"`
	PayloadJson string  `json:"payloadJson"`
	PayloadHash string  `json:"payloadHash"`
	CreatedAt   int64   `json:"createdAt"`
}

// WorkflowResourceLinkRecord indexes one external resource (terminal, watcher,
// async handle, worktree, branch, PR, …) against a workflow graph (and
// optionally one node). It is the REVERSE index the snapshot alone can't serve
// cheaply: an async completion or queue event carrying only its own id maps
// back to the owning graph/node through this table. Natural key
// (workflowId, resourceType, resourceRef) — re-linking updates in place.
type WorkflowResourceLinkRecord struct {
	WorkflowID   string  `json:"workflowId"`
	ResourceType string  `json:"resourceType"` // terminal|agent|worktree|branch|pr|issue|watcher|timer|async|queue_event|artifact|memory|grant
	ResourceRef  string  `json:"resourceRef"`
	NodeID       *string `json:"nodeId,omitempty"`
	Label        *string `json:"label,omitempty"`
	Status       *string `json:"status,omitempty"`
	MetadataJson string  `json:"metadataJson"`
	CreatedAt    int64   `json:"createdAt"`
	UpdatedAt    int64   `json:"updatedAt"`
}

// WorkflowReconcileRunRecord is the forensic row of one backend reconcile call:
// what revision it read, whether/where its patch landed, and bounded hashes of
// the input/output for log correlation. Status: started|applied|rejected|
// conflict|failed|preview.
type WorkflowReconcileRunRecord struct {
	ID              string  `json:"id"` // wrc_<uuid8>
	WorkflowID      string  `json:"workflowId"`
	BaseRevision    int64   `json:"baseRevision"`
	AppliedRevision *int64  `json:"appliedRevision,omitempty"`
	Status          string  `json:"status"`
	InputHash       string  `json:"inputHash"`
	OutputHash      *string `json:"outputHash,omitempty"`
	Warning         *string `json:"warning,omitempty"`
	CreatedAt       int64   `json:"createdAt"`
}

// MemoryRecord is one stored memory. forget is a soft-delete (stamp DeletedAt);
// recall/list filter DeletedAt IS NULL. One DB == one project (no projectId).
type MemoryRecord struct {
	ID       string       `json:"id"` // mem_<uuid8>
	Content  string       `json:"content"`
	Category *string      `json:"category,omitempty"`
	Source   MemorySource `json:"source"`
	// Kind discriminates semantic (default) vs episodic memories. Empty on a
	// freshly-built record is treated as semantic by the storage layer (mirrors the
	// SQLite DEFAULT 'semantic'); a scanned row always carries the concrete value.
	Kind MemoryKind `json:"kind,omitempty"`
	// ExpiresAt is an optional epoch-ms TTL. list/recall exclude rows whose
	// ExpiresAt has passed; nil ⇒ never expires.
	ExpiresAt *int64 `json:"expiresAt,omitempty"`
	// RunID records which turn created the memory (provenance); nil when unknown.
	RunID *string `json:"runId,omitempty"`
	// SessionID namespaces episodic rows to the session that produced them; nil for
	// semantic memories.
	SessionID *string `json:"sessionId,omitempty"`
	PinnedAt  *int64  `json:"pinnedAt,omitempty"`
	DeletedAt *int64  `json:"deletedAt,omitempty"`
	CreatedAt int64   `json:"createdAt"`
	UpdatedAt int64   `json:"updatedAt"`
}

// ArtifactRecord is one durable tool-result overflow payload. When a serialized
// tool result overflows the inline cap the session stashes the full JSON envelope
// here so a later artifact.read survives the in-memory hot cache's eviction (>64)
// and a process restart. Looked up by opaque id; sessionId is provenance +
// retention scoping only (lookup is global — a rehydrated stub from a prior session
// must still resolve). TotalChars/TotalBytes mirror the stub's reported sizes.
type ArtifactRecord struct {
	ID         string `json:"id"` // artifact_<uuid8>
	SessionID  string `json:"sessionId"`
	Content    string `json:"content"`
	TotalChars int    `json:"totalChars"`
	TotalBytes int    `json:"totalBytes"`
	CreatedAt  int64  `json:"createdAt"`
}

// ContextCheckpointRecord is the durable, structured compaction checkpoint. At each
// compaction boundary the session persists the latest checkpoint here so a hard
// restart can reload the compacted operational state instead of losing it to
// working-history-only storage. Exactly two slots are kept — Slot "latest" and a
// "prev" fallback rotated in on each upsert — so a corrupt latest (unparseable
// PayloadJson) can fall back to the prior valid one. PayloadJson is the full
// structured checkpoint object verbatim (opaque to the storage layer, so a richer
// object can round-trip without a schema change); the promoted columns are the fields
// the resume path reads directly. One DB == one project (no projectId, and no
// sessionId — a stale checkpoint from a prior session is harmless: the conversation
// rows are the authoritative transcript). LastSeq is the conversation seq at the
// compaction boundary, so resume can validate the checkpoint against the replayed
// delta.
type ContextCheckpointRecord struct {
	Slot            string `json:"slot"` // "latest" | "prev" (set by the storage layer)
	CompactionDepth int    `json:"compactionDepth"`
	SummaryText     string `json:"summaryText"`
	LastRunID       string `json:"lastRunId,omitempty"`
	LastSeq         int    `json:"lastSeq"`
	PayloadJson     string `json:"payloadJson"`
	CreatedAt       int64  `json:"createdAt"`
}

// RunbookStepProgress is one step within a RunbookRunStateRecord (1-based index).
type RunbookStepProgress struct {
	Index  int               `json:"index"`
	Status RunbookStepStatus `json:"status"`
	Notes  *string           `json:"notes,omitempty"`
	Ts     int64             `json:"ts"`
}

// RunbookRunStateRecord tracks a runbook's stepwise progress. Natural key
// (sessionId, runbookId).
type RunbookRunStateRecord struct {
	ID          string           `json:"id"` // rrs_<uuid8>
	SessionID   string           `json:"sessionId"`
	RunbookID   string           `json:"runbookId"`
	CurrentStep int              `json:"currentStep"` // 0 = not started
	StepsJson   string           `json:"stepsJson"`   // JSON RunbookStepProgress[]
	Status      RunbookRunStatus `json:"status"`
	StartedAt   int64            `json:"startedAt"`
	UpdatedAt   int64            `json:"updatedAt"`
	CompletedAt *int64           `json:"completedAt,omitempty"`
}

// AgentLaunchRecord is the durable state of the idempotent agent-spawn saga.
// Session-scoped: cancelStaleAgentLaunches marks non-terminal rows failed on DB
// open.
type AgentLaunchRecord struct {
	ID             string           `json:"id"` // agt_<uuid8>
	IdempotencyKey string           `json:"idempotencyKey"`
	AgentID        string           `json:"agentId"`
	WorktreeID     *string          `json:"worktreeId,omitempty"`
	Mode           string           `json:"mode"` // edit | explore
	Title          string           `json:"title"`
	Name           string           `json:"name"`
	TerminalID     *string          `json:"terminalId,omitempty"`
	WatcherID      *string          `json:"watcherId,omitempty"`
	Stage          AgentLaunchStage `json:"stage"`
	ErrorCode      *string          `json:"errorCode,omitempty"`
	ErrorMessage   *string          `json:"errorMessage,omitempty"`
	CreatedAt      int64            `json:"createdAt"`
	UpdatedAt      int64            `json:"updatedAt"`
	// WorkflowRunID links a spawn saga to its durable workflow ledger row. Set once
	// the ledger row is created (best-effort, after a terminal binds) so an idempotent
	// retry re-uses the same row instead of inserting a duplicate. nil until then.
	WorkflowRunID *string `json:"workflowRunId,omitempty"`
}

// AsyncInvocationRecord is one runtime-owned async tool invocation: a durable
// future the coordinator polls to completion after the tool call already
// returned its immediate "accepted, running asynchronously" result. The model
// gets the completion later through the attention queue (an autonomous wake),
// NEVER as a late tool result for the original call. Project-scoped like
// watchers: a non-terminal row survives process boundaries and is adopted by
// the next owner's coordinator at Start; /clear cancels it.
type AsyncInvocationRecord struct {
	ID       string `json:"id"`       // asy_<uuid8>
	ToolName string `json:"toolName"` // terminal.run.async | terminal.await.async
	Title    string `json:"title"`    // short human label ("npm test", "wait for cohort")
	// GroupID groups invocations created in the same turn so completions landing
	// within the settle grace coalesce into ONE wake event. It is the creating
	// turn's run id VERBATIM (run_…, which doubles as provenance), or the
	// invocation's own id when no run id was available (self-grouped).
	GroupID         string `json:"groupId"`
	SessionID       string `json:"sessionId"`
	TerminalIdsJson string `json:"terminalIdsJson"` // JSON string[] — the watched terminals
	// Command is the command terminal.run.async sent before watching; nil for the
	// watch-only terminal.await.async.
	Command *string     `json:"command,omitempty"`
	Status  AsyncStatus `json:"status"`
	// OutcomesJson is the per-terminal settle ledger (JSON map terminalId →
	// {status, exitCode?, reason?}), written when the invocation settles/expires.
	OutcomesJson *string `json:"outcomesJson,omitempty"`
	LastError    *string `json:"lastError,omitempty"`
	QueueEventID *string `json:"queueEventId,omitempty"`
	// EndedReason distinguishes a /clear teardown ("session_cleared") from a
	// deliberate cancel ("user_cancelled") — mirrors WatcherRecord.EndedReason.
	EndedReason *string `json:"endedReason,omitempty"`
	CreatedAt   int64   `json:"createdAt"`
	// StartedAt is when the side effect was confirmed and polling began.
	StartedAt *int64 `json:"startedAt,omitempty"`
	// ExpiresAt is the hard deadline: past it the invocation expires with whatever
	// outcomes settled so far.
	ExpiresAt  int64  `json:"expiresAt"`
	FinishedAt *int64 `json:"finishedAt,omitempty"`
}

// QueueEvent is a live attention-queue event. updatedAt advances
// on each dedupe bump; createdAt stays fixed (recency).
type QueueEvent struct {
	ID                 string              `json:"id"`
	Source             EventSource         `json:"source"`
	Severity           Severity            `json:"severity"`
	Title              string              `json:"title"`
	Summary            string              `json:"summary"`
	Target             *EventTarget        `json:"target,omitempty"`
	Evidence           []string            `json:"evidence,omitempty"`
	RecommendedActions []RecommendedAction `json:"recommendedActions,omitempty"`
	DedupeKey          string              `json:"dedupeKey,omitempty"`
	EpistemicKind      EpistemicKind       `json:"epistemicKind,omitempty"`
	CreatedAt          int64               `json:"createdAt"`
	UpdatedAt          *int64              `json:"updatedAt,omitempty"`
	ExpiresAt          *int64              `json:"expiresAt,omitempty"`
	ResolvedAt         *int64              `json:"resolvedAt,omitempty"`
	Count              int                 `json:"count"`
}

// QueueDigestOptions filters a queue digest.
type QueueDigestOptions struct {
	SeverityAtLeast *Severity
	MaxItems        *int
	IncludeResolved bool
	NotifiedIsNull  bool
}
