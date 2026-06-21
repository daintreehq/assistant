// Package domain holds the pure, dependency-free domain vocabulary of the
// assistant: enums, persisted-record structs, the ToolResult envelope, ID and
// timestamp helpers, the event vocabulary, and JSON wire contracts.
//
// This package imports NO other internal package and nothing from the UI,
// network, or storage layers. It is the shared spine every other package
// compiles against. Port specs:
//   - docs/port/domain-config.md  (§2 schemas, §8 ids/timestamps)
//   - docs/port/_contracts.md     (tool/risk/severity inventory)
//   - docs/port/_interaction-ux.md (RunPhase / TurnStep model — shared agent+ui)
package domain

// RiskClass drives the confirmation matrix and tier gating.
// Order per schemas.ts RiskClass (NB: differs from CLAUDE.md prose).
type RiskClass string

const (
	RiskRead     RiskClass = "read"
	RiskLocal    RiskClass = "local"
	RiskUI       RiskClass = "ui"
	RiskTerminal RiskClass = "terminal"
	RiskProject  RiskClass = "project"
	RiskGit      RiskClass = "git"
	RiskExternal RiskClass = "external"
	RiskSystem   RiskClass = "system"
)

var validRiskClass = newEnumSet(
	RiskRead, RiskLocal, RiskUI, RiskTerminal,
	RiskProject, RiskGit, RiskExternal, RiskSystem,
)

// IsValid reports whether r is a known risk class.
func (r RiskClass) IsValid() bool { return validRiskClass[r] }

// Tier is a permission tier. supervisor < operator < system.
type Tier string

const (
	TierSupervisor Tier = "supervisor"
	TierOperator   Tier = "operator"
	TierSystem     Tier = "system"
)

var validTier = newEnumSet(TierSupervisor, TierOperator, TierSystem)

// IsValid reports whether t is a known tier.
func (t Tier) IsValid() bool { return validTier[t] }

// ModelTier selects a Fireworks model size. medium routes to large in v1.
type ModelTier string

const (
	ModelSmall  ModelTier = "small"
	ModelMedium ModelTier = "medium"
	ModelLarge  ModelTier = "large"
)

var validModelTier = newEnumSet(ModelSmall, ModelMedium, ModelLarge)

// IsValid reports whether m is a known model tier.
func (m ModelTier) IsValid() bool { return validModelTier[m] }

// AgentState mirrors Daintree's agent terminal state.
type AgentState string

const (
	AgentIdle      AgentState = "idle"
	AgentWorking   AgentState = "working"
	AgentWaiting   AgentState = "waiting"
	AgentDirecting AgentState = "directing"
	AgentCompleted AgentState = "completed"
	AgentExited    AgentState = "exited"
)

var validAgentState = newEnumSet(
	AgentIdle, AgentWorking, AgentWaiting,
	AgentDirecting, AgentCompleted, AgentExited,
)

// IsValid reports whether s is a known agent state.
func (s AgentState) IsValid() bool { return validAgentState[s] }

// Severity ranks a queue event. The DECLARATION order below intentionally
// differs from the numeric RANK order (see SeverityRank) — the rank is
// authoritative for sorting and the severityAtLeast filter.
type Severity string

const (
	SeverityDebug     Severity = "debug"
	SeverityInfo      Severity = "info"
	SeverityAttention Severity = "attention"
	SeverityUrgent    Severity = "urgent"
	SeverityBlocked   Severity = "blocked"
	SeverityDone      Severity = "done"
	SeverityError     Severity = "error"
)

var validSeverity = newEnumSet(
	SeverityDebug, SeverityInfo, SeverityAttention,
	SeverityUrgent, SeverityBlocked, SeverityDone, SeverityError,
)

// IsValid reports whether s is a known severity.
func (s Severity) IsValid() bool { return validSeverity[s] }

// SeverityRank is the numeric ordering used both for the severityAtLeast filter
// and ORDER BY ... DESC. CRITICAL: this is NOT the enum declaration order.
// The SQL CASE mirror must use ELSE 1 (unknown severity ⇒ treated as info).
var SeverityRank = map[Severity]int{
	SeverityDebug:     0,
	SeverityInfo:      1,
	SeverityDone:      2,
	SeverityAttention: 3,
	SeverityBlocked:   4,
	SeverityUrgent:    5,
	SeverityError:     6,
}

// RankOf returns the rank for a severity, defaulting to the info rank (1) for
// any unknown value — mirroring the SQL CASE ELSE 1 fallback.
func RankOf(s Severity) int {
	if r, ok := SeverityRank[s]; ok {
		return r
	}
	return 1
}

// EventSource identifies who published a queue event.
type EventSource string

const (
	SourceTimer           EventSource = "timer"
	SourceTerminalWatcher EventSource = "terminal_watcher"
	SourceWorktreeWatcher EventSource = "worktree_watcher"
	SourcePRWatcher       EventSource = "pr_watcher"
	SourceWorkflow        EventSource = "workflow"
	SourceModelWorker     EventSource = "model_worker"
	SourceSystem          EventSource = "system"
	SourceUser            EventSource = "user"
)

var validEventSource = newEnumSet(
	SourceTimer, SourceTerminalWatcher, SourceWorktreeWatcher, SourcePRWatcher,
	SourceWorkflow, SourceModelWorker, SourceSystem, SourceUser,
)

// IsValid reports whether s is a known event source.
func (s EventSource) IsValid() bool { return validEventSource[s] }

// EpistemicKind is the provenance of a fact (how it was established).
type EpistemicKind string

const (
	EpistemicObserved   EpistemicKind = "observed"
	EpistemicInferred   EpistemicKind = "inferred"
	EpistemicUnverified EpistemicKind = "unverified"
)

var validEpistemicKind = newEnumSet(EpistemicObserved, EpistemicInferred, EpistemicUnverified)

// IsValid reports whether k is a known epistemic kind.
func (k EpistemicKind) IsValid() bool { return validEpistemicKind[k] }

// WatcherClassification is the small-model / engine classification of a watched
// terminal. CompletedUnverified is set ONLY by the engine, never by the model
// (omit it from any model-facing prompt enum).
type WatcherClassification string

const (
	ClassNoChange            WatcherClassification = "no_change"
	ClassStillWorking        WatcherClassification = "still_working"
	ClassWaitingForInput     WatcherClassification = "waiting_for_input"
	ClassPermissionPrompt    WatcherClassification = "permission_prompt"
	ClassCommandFailed       WatcherClassification = "command_failed"
	ClassTestsFailed         WatcherClassification = "tests_failed"
	ClassTestsPassed         WatcherClassification = "tests_passed"
	ClassMergeConflict       WatcherClassification = "merge_conflict"
	ClassCompletedSuccess    WatcherClassification = "completed_success"
	ClassCompletedUnverified WatcherClassification = "completed_unverified"
	ClassCompletedUnknown    WatcherClassification = "completed_unknown"
	ClassTerminalExited      WatcherClassification = "terminal_exited"
	ClassRateLimited         WatcherClassification = "rate_limited"
	ClassNeedsLargeModel     WatcherClassification = "needs_large_model"
	ClassUnknown             WatcherClassification = "unknown"
)

var validWatcherClassification = newEnumSet(
	ClassNoChange, ClassStillWorking, ClassWaitingForInput, ClassPermissionPrompt,
	ClassCommandFailed, ClassTestsFailed, ClassTestsPassed, ClassMergeConflict,
	ClassCompletedSuccess, ClassCompletedUnverified, ClassCompletedUnknown,
	ClassTerminalExited, ClassRateLimited, ClassNeedsLargeModel, ClassUnknown,
)

// IsValid reports whether c is a known watcher classification.
func (c WatcherClassification) IsValid() bool { return validWatcherClassification[c] }

// VerificationVerdict is the read-only completion verdict.
type VerificationVerdict string

const (
	VerdictVerified VerificationVerdict = "verified"
	VerdictFailed   VerificationVerdict = "failed"
	VerdictUnknown  VerificationVerdict = "unknown"
)

var validVerificationVerdict = newEnumSet(VerdictVerified, VerdictFailed, VerdictUnknown)

// IsValid reports whether v is a known verdict.
func (v VerificationVerdict) IsValid() bool { return validVerificationVerdict[v] }

// JsonOutputStatus is the terminal status of a one-shot --json run.
type JsonOutputStatus string

const (
	JSONStatusSuccess   JsonOutputStatus = "success"
	JSONStatusError     JsonOutputStatus = "error"
	JSONStatusCancelled JsonOutputStatus = "cancelled"
)

// JsonlEventType is the `type` string on each one-shot JSONL line. It reuses the
// durable RunEvent vocabulary so the live stream and DB replay agree, plus
// `result`.
type JsonlEventType string

const (
	JsonlAssistantStart     JsonlEventType = "assistant:start"
	JsonlAssistantContent   JsonlEventType = "assistant:content"
	JsonlAssistantEnd       JsonlEventType = "assistant:end"
	JsonlAssistantCancelled JsonlEventType = "assistant:cancelled"
	JsonlToolCall           JsonlEventType = "tool:call"
	JsonlToolResult         JsonlEventType = "tool:result"
	JsonlError              JsonlEventType = "error"
	JsonlInfo               JsonlEventType = "info"
	JsonlResult             JsonlEventType = "result"
)

// RecommendedActionVerb is the WatcherVerdict.recommendedAction inline enum.
type RecommendedActionVerb string

const (
	ActionNone          RecommendedActionVerb = "none"
	ActionFocusTerminal RecommendedActionVerb = "focus_terminal"
	ActionAskUser       RecommendedActionVerb = "ask_user"
	ActionSendInput     RecommendedActionVerb = "send_input"
	ActionSpawnHelper   RecommendedActionVerb = "spawn_helper"
	ActionOpenReview    RecommendedActionVerb = "open_review"
)

// --- plain string-union types (schemas.ts §2.5) ---

// AutomationGrantActorType is the actor an automation grant authorizes.
type AutomationGrantActorType string

const (
	GrantActorWatcher AutomationGrantActorType = "watcher"
	GrantActorTimer   AutomationGrantActorType = "timer"
)

// AutomationGrantSource records where a grant was minted (only local today).
type AutomationGrantSource string

const (
	GrantSourceLocal    AutomationGrantSource = "local"
	GrantSourceDaintree AutomationGrantSource = "daintree"
)

// WorkflowRunStatus tracks a workflow run. done/cancelled/failed are terminal.
type WorkflowRunStatus string

const (
	WorkflowPending   WorkflowRunStatus = "pending"
	WorkflowActive    WorkflowRunStatus = "active"
	WorkflowBlocked   WorkflowRunStatus = "blocked"
	WorkflowDone      WorkflowRunStatus = "done"
	WorkflowCancelled WorkflowRunStatus = "cancelled"
	WorkflowFailed    WorkflowRunStatus = "failed"
)

// MemorySource records who authored a memory.
type MemorySource string

const (
	MemoryUser      MemorySource = "user"
	MemoryAssistant MemorySource = "assistant"
	MemoryCompact   MemorySource = "compact"
)

// SkillRunStatus tracks a skill run.
type SkillRunStatus string

const (
	SkillRunActive    SkillRunStatus = "active"
	SkillRunCompleted SkillRunStatus = "completed"
	SkillRunAbandoned SkillRunStatus = "abandoned"
)

// SkillStepStatus marks how a skill step resolved.
type SkillStepStatus string

const (
	SkillStepDone    SkillStepStatus = "done"
	SkillStepSkipped SkillStepStatus = "skipped"
)

// AgentLaunchStage is a stage of the idempotent agent-spawn saga. Only
// confirmed/failed are terminal; ambiguous stays live for reconciliation.
type AgentLaunchStage string

const (
	LaunchRequested AgentLaunchStage = "launch_requested"
	AgentStarted    AgentLaunchStage = "agent_started"
	TerminalBound   AgentLaunchStage = "terminal_bound"
	WatcherAttached AgentLaunchStage = "watcher_attached"
	LaunchConfirmed AgentLaunchStage = "confirmed"
	LaunchFailed    AgentLaunchStage = "failed"
	LaunchAmbiguous AgentLaunchStage = "ambiguous"
)

// ToolActor identifies who initiated a tool dispatch.
type ToolActor string

const (
	ActorMain     ToolActor = "main"
	ActorWatcher  ToolActor = "watcher"
	ActorTimer    ToolActor = "timer"
	ActorWorkflow ToolActor = "workflow"
	ActorSystem   ToolActor = "system"
)

// newEnumSet builds a membership set for a comparable enum value type.
func newEnumSet[T comparable](vals ...T) map[T]bool {
	m := make(map[T]bool, len(vals))
	for _, v := range vals {
		m[v] = true
	}
	return m
}
