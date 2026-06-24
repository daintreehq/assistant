package domain

import "encoding/json"

// EventTarget scopes a queue event to a Daintree object. All fields optional.
type EventTarget struct {
	ProjectID     string `json:"projectId,omitempty"`
	WorktreeID    string `json:"worktreeId,omitempty"`
	TerminalID    string `json:"terminalId,omitempty"`
	WorkflowRunID string `json:"workflowRunId,omitempty"`
}

// RecommendedAction is a suggested follow-up tool call surfaced on a queue event.
type RecommendedAction struct {
	Label                string    `json:"label"`
	ToolName             string    `json:"toolName"`
	Args                 any       `json:"args,omitempty"`
	Risk                 RiskClass `json:"risk,omitempty"`
	RequiresConfirmation bool      `json:"requiresConfirmation,omitempty"`
}

// QueuePublishArgs is the input to Queue.publish. epistemicKind MUST be declared
// (a strict decode would otherwise strip it before it reaches the DB).
type QueuePublishArgs struct {
	Source             EventSource         `json:"source"`
	Severity           Severity            `json:"severity"`
	Title              string              `json:"title"`
	Summary            string              `json:"summary"`
	Target             *EventTarget        `json:"target,omitempty"`
	Evidence           []string            `json:"evidence,omitempty"`
	RecommendedActions []RecommendedAction `json:"recommendedActions,omitempty"`
	DedupeKey          string              `json:"dedupeKey,omitempty"`
	TTLMs              *int64              `json:"ttlMs,omitempty"`
	EpistemicKind      EpistemicKind       `json:"epistemicKind,omitempty"`
}

// WatcherVerdict is the small-model output contract.
// Defaults applied after decode: Evidence []; RecommendedAction "none".
type WatcherVerdict struct {
	Classification    WatcherClassification `json:"classification"`
	Confidence        float64               `json:"confidence"` // [0,1]
	Summary           string                `json:"summary"`
	Evidence          []string              `json:"evidence"`
	RecommendedAction RecommendedActionVerb `json:"recommendedAction"`
}

// ModelJudgeAnswer is a small-model yes/no judgement. FIELD ORDER IS LOAD-BEARING:
// Reason FIRST (implicit chain-of-thought), then Confidence, then Matched.
// encoding/json emits in declaration order — do not reorder these fields.
type ModelJudgeAnswer struct {
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"` // [0,1]
	Matched    bool    `json:"matched"`
}

// FinishedJudgeQuestion is the ONE byte-stable yes/no question that confirms an
// agent terminal has genuinely finished its turn. A bare agentState=="waiting" is
// an UNRELIABLE proxy: an agent parks at "waiting" before it picks up the prompt,
// pauses mid-task, or (the real-world failure mode) gets flipped to "waiting" by
// Daintree when its window is backgrounded. So the watcher engine AND the
// terminal.extract settle gate both ask the small model THIS question against the
// terminal tail before concluding "finished". Shared by internal/daemon
// (judgeAgentFinished) and internal/tools/extractionx (confirmFinished) so the two
// consumers cannot drift — the small model is tuned against a single phrasing.
const FinishedJudgeQuestion = "Has this agent finished its turn and stopped, returning to an idle prompt AFTER doing work? Answer YES only if the most recent output shows the work is complete or the agent has clearly stopped. Answer NO if it is still working, mid-task, paused, or merely parked at a prompt before it has started the work."

// VerificationResult is the read-only completion verification. A legacy blob
// with old enum values (clean/dirty) must deserialize safely to "unknown" —
// callers should normalize an unrecognized verdict to VerdictUnknown after
// decode.
type VerificationResult struct {
	Verdict            VerificationVerdict `json:"verdict"`
	HasGitChanges      bool                `json:"hasGitChanges"`
	ChangedFiles       int                 `json:"changedFiles"`
	ChangedFileList    []string            `json:"changedFileList"`
	GitSummary         string              `json:"gitSummary"`
	AcceptanceCriteria string              `json:"acceptanceCriteria,omitempty"`
	CriteriaMetSummary string              `json:"criteriaMetSummary,omitempty"`
	UnresolvedWarnings []string            `json:"unresolvedWarnings"`
}

// JsonlEvent is a one-shot --json stream line. Extra per-type fields are
// preserved in Extra.
type JsonlEvent struct {
	Type  JsonlEventType             `json:"type"`
	Ts    int64                      `json:"ts"`
	Seq   int                        `json:"seq"`
	Extra map[string]json.RawMessage `json:"-"`
}

// JsonResultEnvelope is the terminal `result` line of a --json run (.strict()).
// ExitCode is constrained to 0|1|2 (3 is reserved, never valid on this line).
type JsonResultEnvelope struct {
	Type          string           `json:"type"` // literal "result"
	Ts            int64            `json:"ts"`
	Seq           int              `json:"seq"`
	SchemaVersion int              `json:"schemaVersion"` // literal 1
	Status        JsonOutputStatus `json:"status"`
	ExitCode      int              `json:"exitCode"` // 0|1|2
	Content       string           `json:"content"`
	Error         *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// classEpistemicKind maps a classification to its provenance when no model was
// used.
var classObservedWithoutModel = newEnumSet(
	ClassTerminalExited, ClassWaitingForInput, ClassRateLimited, ClassStillWorking,
)

var classAlwaysInferred = newEnumSet(
	ClassPermissionPrompt, ClassTestsFailed, ClassTestsPassed,
	ClassCommandFailed, ClassMergeConflict, ClassCompletedSuccess,
	ClassCompletedUnverified, ClassCompletedUnknown,
)

// ClassificationEpistemicKind maps a watcher classification to the provenance of
// the resulting fact. usedModel reflects whether the small model produced the
// classification (vs. a deterministic signal).
//
//   - terminal_exited / waiting_for_input / rate_limited / still_working → inferred
//     iff usedModel else observed (a working agent is observed model-free; a model-
//     judged still_working stays inferred).
//   - the "always inferred" set → inferred.
//   - everything else (no_change / unknown / needs_large_model / unrecognized) →
//     unverified.
func ClassificationEpistemicKind(c WatcherClassification, usedModel bool) EpistemicKind {
	if classObservedWithoutModel[c] {
		if usedModel {
			return EpistemicInferred
		}
		return EpistemicObserved
	}
	if classAlwaysInferred[c] {
		return EpistemicInferred
	}
	return EpistemicUnverified
}
