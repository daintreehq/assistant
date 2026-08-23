package domain

import "encoding/json"

// EventTarget scopes a queue event to a Daintree object. All fields optional.
type EventTarget struct {
	ProjectID     string `json:"projectId,omitempty"`
	WorktreeID    string `json:"worktreeId,omitempty"`
	TerminalID    string `json:"terminalId,omitempty"`
	WorkflowRunID string `json:"workflowRunId,omitempty"`
	// AsyncInvocationID links a completion event back to its async_invocations
	// ledger row (asy_…), so the wake reactor and async.list can cross-reference.
	AsyncInvocationID string `json:"asyncInvocationId,omitempty"`
}

// RecommendedAction is a suggested follow-up tool call surfaced on a queue event.
type RecommendedAction struct {
	Label                string    `json:"label"`
	ToolName             string    `json:"toolName"`
	Args                 any       `json:"args,omitempty"`
	Risk                 RiskClass `json:"risk,omitempty"`
	RequiresConfirmation bool      `json:"requiresConfirmation,omitempty"`
	// NodeID optionally binds the recommendation to one workflow-graph node (the
	// backend's next_action.node_id), disambiguating which ready node the action
	// advances when several share a tool. Advisory only; ingest paths drop a
	// binding that names a node the target graph doesn't have.
	NodeID string `json:"nodeId,omitempty"`
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
// agent terminal has finished THIS TURN. A bare agentState=="waiting" is an
// UNRELIABLE proxy: an agent parks at "waiting" before it picks up the prompt,
// stalls cut off mid-output, or (the real-world failure mode) gets flipped to
// "waiting" by Daintree when its window is backgrounded. So the watcher engine AND
// the terminal.extract settle gate both ask the small model THIS question against the
// terminal tail before concluding "finished". Shared by internal/daemon
// (judgeAgentFinished) and internal/tools/extractionx (confirmFinished) so the two
// consumers cannot drift — the small model is tuned against a single phrasing.
//
// CRITICAL — TURN completion, NOT task completion. The orchestration model relays
// between agents: the instant an agent produces its response and returns to an idle,
// ready prompt, the orchestrator should act (read its output, relay it, send the next
// round). So "finished" means "done generating FOR NOW and awaiting input" — TRUE even
// when the broader task / conversation / game has more rounds to come. The prior phrasing
// asked whether "the work is actually done", so a multi-round agent that posted its
// message and went idle was judged "still mid-task" and the wait burned its whole budget
// (an interactive Thieves'-game cohort hung terminal.awaitAll for ~3 minutes while all
// three agents sat idle at their prompts). An idle prompt AFTER produced output is the
// signal to act, not a non-finish.
const FinishedJudgeQuestion = "Has this agent finished generating its response for this turn and returned to an idle prompt that is ready for new input? Answer YES only when the tail shows SUBSTANTIVE output the agent itself produced this turn (a message, answer, result, or sign-off) AND the terminal is now sitting idle at a ready prompt. This counts as finished EVEN IF the overall task, conversation, or game has more rounds to come — an agent that posted its message and is back at a ready idle prompt has finished its turn; you are judging TURN completion, NOT whether the whole task is done. Answer NO if the agent is still actively working or generating, is cut off partway through its output (a live spinner, a tool call in flight, or an unfinished message), is stalled or silent without having produced a response, or has not produced anything yet. Do NOT count shell prompts, banners, menus, status lines, or other UI chrome as produced output — there must be a real response from the agent before an idle prompt means finished."

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
	Stats JsonRunStats `json:"stats"`
}

// JsonRunStats is the accounting block on the terminal `result` line — how much work
// the run did, so a consumer need not re-derive it by counting stream lines it may not
// have kept. The single declaration of these keys; the sink marshals this struct.
//
// Read the token counts as a LOWER BOUND on spend, not a bill. They are summed from the
// per-round usage the backend reports on a SUCCESSFUL response, so an attempt that was
// billed and then failed into a retry contributes nothing, and the separate model calls
// behind auto-compaction and the utility tasks are not counted at all.
type JsonRunStats struct {
	DurationMs int `json:"durationMs"`
	// Rounds counts rounds STARTED (one per assistant:start), not tool calls and not
	// completed backend requests: a run cancelled before its first request still
	// reports 1.
	Rounds     int `json:"rounds"`
	ToolCalls  int `json:"toolCalls"`
	ToolErrors int `json:"toolErrors"`
	// PromptTokens sums each round's prompt — every round re-sends the conversation, so
	// this is input-token VOLUME (what a provider charges for), not the size of the
	// context. ContextTokens is the latter: the last round's prompt size, which is the
	// figure that drives compaction.
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
	ContextTokens    int `json:"contextTokens"`
}

// JsonSessionPayload is the body of the one-time `session` header line: the facts a
// consumer needs to FIND the run it just started. It is the SINGLE declaration of those
// keys — the sink marshals this struct rather than hand-rolling a map, so the wire
// shape cannot drift from the contract silently.
//
// It names the endpoint and never the key. BackendURL arrives already stripped of
// userinfo and query (mcp.SanitizeURL); Project and LogPath are filesystem paths and
// are NOT sanitized, so the honest guarantee is "no field here is a credential", not
// "no field here can embarrass you".
type JsonSessionPayload struct {
	// SchemaVersion is repeated here, on the FIRST line, as well as on the terminal
	// `result`. Carrying it only on the last line meant a streaming consumer had to
	// parse the entire run before it could learn which schema it had been reading — so
	// a version it does not understand surfaced as mis-parsed events rather than as a
	// clean compatibility error at frame one. It is the same value in both places.
	SchemaVersion int    `json:"schemaVersion"`
	SessionID     string `json:"sessionId"`
	Project       string `json:"project"`
	Tier          string `json:"tier"`
	BackendURL    string `json:"backendUrl"`
	// LogPath is "" when debug logging is off — reported as empty rather than omitted,
	// so a consumer can tell "logging disabled" from "field absent in this version".
	LogPath string `json:"logPath"`
	Version string `json:"version"`
	// AutoApprove restates the warning line as a machine-readable fact.
	AutoApprove bool `json:"autoApprove"`
	// MCPConnected is the status at the moment the run STARTED, sampled once right
	// after connect. It separates a real answer from one produced in degraded local
	// mode, which is invisible in the content — but MCP can still degrade later in the
	// run, so it is a starting condition, not a whole-run guarantee.
	MCPConnected bool   `json:"mcpConnected"`
	MCPTransport string `json:"mcpTransport"`
}

// JsonTurnPromptPayload opens a multi-turn bracket: the prompt this turn was given and
// the turn's index. Turn is ZERO-based, like seq on every line of this stream.
//
// The prompt is ECHOED. Nothing else in the stream carries it (TurnPrompt is a no-op on
// this sink), and a transcript that cannot say which question produced which answer is
// not a transcript. It is the caller's own stdin, so this adds no source of secrets the
// caller did not already supply — but it does put that text on stdout, which is worth
// knowing before piping a prompt file into a CI log.
type JsonTurnPromptPayload struct {
	Turn   int    `json:"turn"`
	Prompt string `json:"prompt"`
}

// JsonTurnEndPayload closes a multi-turn bracket with THIS turn's outcome.
//
// Deliberately no exitCode: an exit code is a property of the process, and putting one
// on a turn invites the reading that some turn's code is the run's. The run's outcome is
// on the terminal `result` line and nowhere else. Status is the per-turn authority the
// same way `result` is the per-run one.
type JsonTurnEndPayload struct {
	Turn   int              `json:"turn"`
	Status JsonOutputStatus `json:"status"`
}

// JsonCommandResultPayload records one slash command run between turns.
//
// Title and Content come from the shared UI command handler, so a JSONL consumer sees
// exactly what the attached session would have shown — as DATA, never rendered, because stdout in
// --json mode carries only these lines.
type JsonCommandResultPayload struct {
	// Command is the line as read, leading slash included, with surrounding whitespace
	// trimmed (the same trim that decides it was a command in the first place).
	Command string `json:"command"`
	// Handled is false for a command the catalog does not know. It answers a DIFFERENT
	// question from the UI handler's own Handled bit, which is true even for an unknown
	// command because the handler still consumed the line and produced an "Unknown
	// command" card; this one is resolved against the command registry instead. The
	// distinction only matters here, where the reader is a script: a typo'd /claer is
	// otherwise indistinguishable on the wire from a command that ran and said nothing.
	Handled bool   `json:"handled"`
	Title   string `json:"title"`
	Content string `json:"content"`
	// Quit reports that the command ended input consumption (`/quit`).
	Quit bool `json:"quit"`
	// ConversationCleared marks the conversation-state boundary `/clear` creates. It
	// clears the CONVERSATION, never the transcript: already-emitted lines stand, seq
	// keeps climbing, stats keep accumulating, and an earlier failed turn stays failed.
	ConversationCleared bool `json:"conversationCleared"`
}

// JsonSessionEnvelope is the full `session` line: the standard framing plus the payload.
type JsonSessionEnvelope struct {
	Type string `json:"type"` // literal "session"
	Ts   int64  `json:"ts"`
	Seq  int    `json:"seq"`
	JsonSessionPayload
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
