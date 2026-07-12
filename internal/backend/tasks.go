package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Task IDs the backend exposes via /v1/daintree/tasks. Use these constants rather
// than string literals at call sites. (Confirm availability against
// Capabilities.Tasks at startup.)
const (
	TaskCheckpoint           = "checkpoint"
	TaskMemoryDistill        = "memory_distill"
	TaskWatcherClassify      = "watcher_classify"
	TaskTerminalJudge        = "terminal_judge"
	TaskTerminalSummarize    = "terminal_summarize"
	TaskTerminalExtractText  = "terminal_extract_text"
	TaskTerminalExtractJSON  = "terminal_extract_json"
	TaskExtractionVerdict    = "extraction_verdict"
	TaskSkillStepConsistency = "skill_step_consistency"
)

// TaskRunner is the narrow seam the typed task helpers depend on (satisfied by
// *Client and trivially by a fake). Keeping the helpers free functions over this
// interface keeps the app/daemon adapters testable without a live backend.
type TaskRunner interface {
	RunTask(ctx context.Context, req TaskRequest) (TaskResult, error)
}

// --------------------------------------------------------------------------
// Task input shapes (field names must match the backend pydantic input models).
// --------------------------------------------------------------------------

// TerminalState is the deterministic terminal snapshot shared by several tasks.
type TerminalState struct {
	AgentState    string `json:"agent_state,omitempty"`
	RuntimeStatus string `json:"runtime_status,omitempty"`
	WaitingReason string `json:"waiting_reason,omitempty"`
	LastOutputAt  string `json:"last_output_at,omitempty"`
}

// CheckpointInput compacts a transcript into a structured checkpoint.
type CheckpointInput struct {
	Transcript string `json:"transcript"`
}

// MemoryDistillInput distills durable facts from a discarded transcript.
type MemoryDistillInput struct {
	Transcript string `json:"transcript"`
}

// WatcherClassifyInput classifies a terminal's recent state for the watcher.
type WatcherClassifyInput struct {
	Goal                   string        `json:"goal,omitempty"`
	TerminalState          TerminalState `json:"terminal_state"`
	PreviousClassification string        `json:"previous_classification,omitempty"`
	Tail                   string        `json:"tail,omitempty"`
}

// TerminalJudgeInput answers one yes/no question about a terminal.
type TerminalJudgeInput struct {
	Goal          string        `json:"goal,omitempty"`
	Question      string        `json:"question"`
	TerminalState TerminalState `json:"terminal_state"`
	Tail          string        `json:"tail,omitempty"`
}

// TerminalSummarizeInput produces a terse factual summary of terminal output.
type TerminalSummarizeInput struct {
	Purpose string `json:"purpose,omitempty"`
	Tail    string `json:"tail,omitempty"`
}

// TerminalExtractTextInput extracts free text from terminal output.
type TerminalExtractTextInput struct {
	TerminalIDs []string `json:"terminal_ids,omitempty"`
	Instruction string   `json:"instruction"`
	Tail        string   `json:"tail,omitempty"`
}

// TerminalExtractJSONInput extracts a structured value from terminal output. The
// optional schema travels in TaskRequest.result_schema, not here.
type TerminalExtractJSONInput struct {
	TerminalIDs []string `json:"terminal_ids,omitempty"`
	Instruction string   `json:"instruction"`
	Tail        string   `json:"tail,omitempty"`
}

// ExtractionVerdictInput judges whether an extracted result satisfies a condition.
type ExtractionVerdictInput struct {
	Result    string `json:"result"`
	Condition string `json:"condition"`
}

// SkillStepConsistencyInput judges whether one skill-step advance is consistent.
type SkillStepConsistencyInput struct {
	SkillID           string           `json:"skill_id"`
	CompletedStep     int              `json:"completed_step"`
	Status            string           `json:"status,omitempty"`
	RequestedNext     string           `json:"requested_next,omitempty"`
	CurrentStepBefore int              `json:"current_step_before"`
	CurrentStepAfter  int              `json:"current_step_after"`
	RunStatusBefore   string           `json:"run_status_before,omitempty"`
	RunStatusAfter    string           `json:"run_status_after,omitempty"`
	Notes             string           `json:"notes,omitempty"`
	ProgressBefore    []map[string]any `json:"progress_before,omitempty"`
	ProgressAfter     []map[string]any `json:"progress_after,omitempty"`
}

// --------------------------------------------------------------------------
// Task output shapes (decoded from TaskResult.Output).
// --------------------------------------------------------------------------

// CheckpointOutput is the structured compaction checkpoint.
type CheckpointOutput struct {
	Goal string `json:"goal"`
	// Standing user instructions/constraints stated mid-conversation ("never close
	// terminals", "always branch first") — preserved near-verbatim by the backend's
	// checkpoint task so an explicit instruction survives compaction.
	UserDirectives  []string `json:"user_directives"`
	ActiveTerminals []string `json:"active_terminals"`
	ActiveWatchers  []string `json:"active_watchers"`
	WorkflowRunIDs  []string `json:"workflow_run_ids"`
	Decisions       []string `json:"decisions"`
	// The loop-prevention register: approaches tried and failed (with why), so the
	// post-compaction assistant does not repeat them.
	FailedApproaches []string `json:"failed_approaches"`
	PendingToolState []string `json:"pending_tool_state"`
	NextActions      []string `json:"next_actions"`
	OpenQuestions    []string `json:"open_questions"`
	ApprovalsGrants  []string `json:"approvals_grants"`
	PreservedIDs     []string `json:"preserved_ids"`
}

// DistilledFact is one distilled memory with its kind.
type DistilledFact struct {
	Fact string `json:"fact"`
	Kind string `json:"kind"` // "semantic" | "episodic"
}

// MemoryDistillOutput is the distilled fact list.
type MemoryDistillOutput struct {
	Facts []DistilledFact `json:"facts"`
}

// WatcherClassifyOutput is the watcher classification verdict.
type WatcherClassifyOutput struct {
	Classification    string   `json:"classification"`
	Confidence        float64  `json:"confidence"`
	Summary           string   `json:"summary"`
	Evidence          []string `json:"evidence"`
	RecommendedAction string   `json:"recommendedAction"`
}

// JudgeOutput is the yes/no judge verdict (terminal_judge + skill consistency).
type JudgeOutput struct {
	Reason     string  `json:"reason"`
	Confidence float64 `json:"confidence"`
	Matched    bool    `json:"matched"`
}

// TextOutput is a plain-text task result (summarize / extract_text).
type TextOutput struct {
	Text string `json:"text"`
}

// ExtractJSONOutput wraps the extracted structured value.
type ExtractJSONOutput struct {
	Result json.RawMessage `json:"result"`
}

// ExtractionVerdictOutput is the settle/verdict judgement.
type ExtractionVerdictOutput struct {
	Passed bool   `json:"pass"`
	Reason string `json:"reason"`
}

// --------------------------------------------------------------------------
// Typed helpers.
// --------------------------------------------------------------------------

// Backend task input size caps (mirrors ../assistant-backend contracts/tasks.py
// max_length). Inputs are clamped client-side BEFORE the call so a large transcript /
// multi-terminal tail never trips the backend's strict validation (a 400) and degrades
// the task — a clamped input is strictly better than a hard failure. A small margin
// keeps us clear of any off-by-one / byte-vs-rune nuance at the boundary.
const (
	maxTaskTranscriptRunes = 399_000 // checkpoint / memory_distill transcript
	maxTaskTailRunes       = 119_000 // watcher/judge/summarize/extract tail
	maxTaskResultRunes     = 119_000 // extraction_verdict result
)

// clampTailRunes keeps the most-recent maxRunes (the tail) of s — recency matters most
// for terminal output and the about-to-be-discarded transcript.
func clampTailRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[len(r)-maxRunes:])
}

// checkpointHeadReserveRunes is the slice of the checkpoint transcript budget reserved
// for the HEAD when the whole transcript exceeds the cap. The head carries what a
// tail-only clamp silently destroyed: the prior "[checkpoint | depth N]" note (which
// the checkpoint prompt's carry-forward rule needs IN HAND to preserve directives,
// decisions, and failed approaches across repeated compactions) and, on a first
// compaction, the user's original request. A note is a few thousand runes, so 16k is
// generous; the remaining ~383k stays with the recent tail where the live state lives.
const checkpointHeadReserveRunes = 16_000

// transcriptElisionMarker separates head from tail in a clamped checkpoint transcript,
// so the model knows content was cut BETWEEN them (not that the head flows into the tail).
const transcriptElisionMarker = "\n[... middle of transcript elided for length ...]\n"

// clampHeadTailRunes keeps the first headRunes AND the freshest remainder of the
// budget, joined by the elision marker — total exactly maxRunes when clamped. Used by
// the checkpoint task only; recency-only inputs (memory_distill, terminal tails) keep
// the plain tail clamp.
func clampHeadTailRunes(s string, maxRunes, headRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	marker := []rune(transcriptElisionMarker)
	tailRunes := maxRunes - headRunes - len(marker)
	return string(r[:headRunes]) + transcriptElisionMarker + string(r[len(r)-tailRunes:])
}

// clampHeadRunes keeps the first maxRunes (the head) of s — an extracted result's
// answer is usually at the start.
func clampHeadRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// TaskOutputError reports a task round that "succeeded" on the wire but produced a
// result the typed caller cannot use: no output at all, or a decode that yielded a
// structurally empty required result. It is typed so callers can distinguish a bad
// task result from transport failures, and so a zero-valued struct is never silently
// treated as a real answer.
type TaskOutputError struct {
	Task   string
	Reason string
}

func (e *TaskOutputError) Error() string {
	return fmt.Sprintf("backend: task %s: %s", e.Task, e.Reason)
}

// runTyped marshals a typed input into the task envelope, runs it, and decodes the
// output into out. Every backend task returns an output object, so when a typed
// target is supplied an empty output is a *TaskOutputError, never a silent zero-value
// success. (No current typed caller legitimately expects empty output; if one ever
// does, give that call an explicit allowEmpty variant rather than weakening this.)
func runTyped(ctx context.Context, r TaskRunner, task string, input any, schema map[string]any, out any) error {
	return runTypedValidated(ctx, r, task, input, schema, out, nil)
}

// runTypedValidated is runTyped plus an optional post-decode validation hook: after a
// successful decode into out, validate (if non-nil) inspects the decoded value and
// returns a reason when it is structurally unusable (e.g. a required field decoded
// empty). A validation failure surfaces as *TaskOutputError. Typed helpers — the
// workflow ones included — wire their obviously-required-field checks through this.
func runTypedValidated(ctx context.Context, r TaskRunner, task string, input any, schema map[string]any, out any, validate func() error) error {
	m, err := structToMap(input)
	if err != nil {
		return fmt.Errorf("backend: encode %s input: %w", task, err)
	}
	res, err := r.RunTask(ctx, TaskRequest{Task: task, Input: m, ResultSchema: schema})
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(res.Output) == 0 {
		return &TaskOutputError{Task: task, Reason: "returned no output"}
	}
	if err := json.Unmarshal(res.Output, out); err != nil {
		return fmt.Errorf("backend: decode %s output: %w", task, err)
	}
	if validate != nil {
		if verr := validate(); verr != nil {
			return &TaskOutputError{Task: task, Reason: verr.Error()}
		}
	}
	return nil
}

// RunCheckpoint compacts a transcript into a structured checkpoint. Deliberately NO
// structurally-empty validate hook: the agent's compaction path (see
// agent/checkpoint.go) treats an all-empty checkpoint as a legitimate best-effort
// degradation — a prose/degenerate model reply must never block compaction, and its
// validateCheckpoint pass mines the load-bearing IDs into the empty object afterward.
// Only a wire round with NO output at all is rejected (by runTyped).
func RunCheckpoint(ctx context.Context, r TaskRunner, in CheckpointInput) (CheckpointOutput, error) {
	// Head+tail, not tail-only: at the auto-compact threshold the transcript is ~4×
	// this cap, and a tail-only clamp cut the prior checkpoint note (always at the
	// HEAD) out of the input — so the prompt's carry-forward rule could never fire and
	// directives/decisions decayed on every repeated compaction.
	in.Transcript = clampHeadTailRunes(in.Transcript, maxTaskTranscriptRunes, checkpointHeadReserveRunes)
	var out CheckpointOutput
	err := runTyped(ctx, r, TaskCheckpoint, in, nil, &out)
	return out, err
}

// RunMemoryDistill distills durable facts from a discarded transcript. No validate
// hook: an empty fact list is a legitimate result (nothing durable to keep).
func RunMemoryDistill(ctx context.Context, r TaskRunner, in MemoryDistillInput) (MemoryDistillOutput, error) {
	in.Transcript = clampTailRunes(in.Transcript, maxTaskTranscriptRunes)
	var out MemoryDistillOutput
	err := runTyped(ctx, r, TaskMemoryDistill, in, nil, &out)
	return out, err
}

// RunWatcherClassify classifies a terminal's recent state.
func RunWatcherClassify(ctx context.Context, r TaskRunner, in WatcherClassifyInput) (WatcherClassifyOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out WatcherClassifyOutput
	err := runTypedValidated(ctx, r, TaskWatcherClassify, in, nil, &out, func() error {
		// Classification is the verdict's required field; without it the watcher
		// state machine has nothing to act on (callers already fall back on error).
		if strings.TrimSpace(out.Classification) == "" {
			return errors.New("output missing classification")
		}
		return nil
	})
	return out, err
}

// RunTerminalJudge answers one yes/no question about a terminal. No validate hook:
// the AUTHORITATIVE backend schema (contracts/tasks.py JudgeOutput) defaults
// reason to "" and matched to false, so {matched:false, confidence:0, reason:""}
// is a schema-valid NEGATIVE verdict — rejecting it client-side would turn a
// legitimate "no" into a task error. Only the no-output-at-all wire round is
// rejected (by runTyped). Same posture for skill_step_consistency below.
func RunTerminalJudge(ctx context.Context, r TaskRunner, in TerminalJudgeInput) (JudgeOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out JudgeOutput
	err := runTyped(ctx, r, TaskTerminalJudge, in, nil, &out)
	return out, err
}

// RunTerminalSummarize produces a terse summary of terminal output. No validate
// hook: `{"text": ""}` is indistinguishable post-decode from an intentionally empty
// answer (e.g. extracting from a blank terminal), so only the missing-output case —
// covered by runTyped — is rejected.
func RunTerminalSummarize(ctx context.Context, r TaskRunner, in TerminalSummarizeInput) (TextOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out TextOutput
	err := runTyped(ctx, r, TaskTerminalSummarize, in, nil, &out)
	return out, err
}

// RunTerminalExtractText extracts free text from terminal output. No validate hook —
// same reasoning as RunTerminalSummarize (empty extracted text can be legitimate).
func RunTerminalExtractText(ctx context.Context, r TaskRunner, in TerminalExtractTextInput) (TextOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out TextOutput
	err := runTyped(ctx, r, TaskTerminalExtractText, in, nil, &out)
	return out, err
}

// RunTerminalExtractJSON extracts a structured value; an optional result schema
// constrains the extraction (the backend treats a non-conforming value as a parse
// failure and retries).
func RunTerminalExtractJSON(ctx context.Context, r TaskRunner, in TerminalExtractJSONInput, schema map[string]any) (ExtractJSONOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out ExtractJSONOutput
	err := runTypedValidated(ctx, r, TaskTerminalExtractJSON, in, schema, &out, func() error {
		// The result envelope is the whole point of this task; a missing/empty
		// `result` means the extraction produced nothing decodable.
		if len(out.Result) == 0 {
			return errors.New("output missing result")
		}
		return nil
	})
	return out, err
}

// RunExtractionVerdict judges whether an extracted result satisfies a condition.
// No validate hook: the backend ExtractionVerdictOutput defaults reason to "", so
// {pass:false, reason:""} is a schema-valid failing judgement (see RunTerminalJudge).
func RunExtractionVerdict(ctx context.Context, r TaskRunner, in ExtractionVerdictInput) (ExtractionVerdictOutput, error) {
	in.Result = clampHeadRunes(in.Result, maxTaskResultRunes)
	var out ExtractionVerdictOutput
	err := runTyped(ctx, r, TaskExtractionVerdict, in, nil, &out)
	return out, err
}

// RunSkillStepConsistency judges whether one skill-step advance is consistent.
// No validate hook — same reasoning as RunTerminalJudge (shared JudgeOutput schema).
func RunSkillStepConsistency(ctx context.Context, r TaskRunner, in SkillStepConsistencyInput) (JudgeOutput, error) {
	var out JudgeOutput
	err := runTyped(ctx, r, TaskSkillStepConsistency, in, nil, &out)
	return out, err
}

// structToMap converts a typed input struct to the map[string]any the task
// envelope carries (a JSON round-trip preserves the json tags / omitempty).
func structToMap(v any) (map[string]any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return m, nil
}
