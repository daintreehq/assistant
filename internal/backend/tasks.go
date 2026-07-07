package backend

import (
	"context"
	"encoding/json"
	"fmt"
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
	Goal             string   `json:"goal"`
	ActiveTerminals  []string `json:"active_terminals"`
	ActiveWatchers   []string `json:"active_watchers"`
	WorkflowRunIDs   []string `json:"workflow_run_ids"`
	Decisions        []string `json:"decisions"`
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

// clampHeadRunes keeps the first maxRunes (the head) of s — an extracted result's
// answer is usually at the start.
func clampHeadRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// runTyped marshals a typed input into the task envelope, runs it, and decodes the
// output into out.
func runTyped(ctx context.Context, r TaskRunner, task string, input any, schema map[string]any, out any) error {
	m, err := structToMap(input)
	if err != nil {
		return fmt.Errorf("backend: encode %s input: %w", task, err)
	}
	res, err := r.RunTask(ctx, TaskRequest{Task: task, Input: m, ResultSchema: schema})
	if err != nil {
		return err
	}
	if out != nil && len(res.Output) > 0 {
		if err := json.Unmarshal(res.Output, out); err != nil {
			return fmt.Errorf("backend: decode %s output: %w", task, err)
		}
	}
	return nil
}

// RunCheckpoint compacts a transcript into a structured checkpoint.
func RunCheckpoint(ctx context.Context, r TaskRunner, in CheckpointInput) (CheckpointOutput, error) {
	in.Transcript = clampTailRunes(in.Transcript, maxTaskTranscriptRunes)
	var out CheckpointOutput
	err := runTyped(ctx, r, TaskCheckpoint, in, nil, &out)
	return out, err
}

// RunMemoryDistill distills durable facts from a discarded transcript.
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
	err := runTyped(ctx, r, TaskWatcherClassify, in, nil, &out)
	return out, err
}

// RunTerminalJudge answers one yes/no question about a terminal.
func RunTerminalJudge(ctx context.Context, r TaskRunner, in TerminalJudgeInput) (JudgeOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out JudgeOutput
	err := runTyped(ctx, r, TaskTerminalJudge, in, nil, &out)
	return out, err
}

// RunTerminalSummarize produces a terse summary of terminal output.
func RunTerminalSummarize(ctx context.Context, r TaskRunner, in TerminalSummarizeInput) (TextOutput, error) {
	in.Tail = clampTailRunes(in.Tail, maxTaskTailRunes)
	var out TextOutput
	err := runTyped(ctx, r, TaskTerminalSummarize, in, nil, &out)
	return out, err
}

// RunTerminalExtractText extracts free text from terminal output.
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
	err := runTyped(ctx, r, TaskTerminalExtractJSON, in, schema, &out)
	return out, err
}

// RunExtractionVerdict judges whether an extracted result satisfies a condition.
func RunExtractionVerdict(ctx context.Context, r TaskRunner, in ExtractionVerdictInput) (ExtractionVerdictOutput, error) {
	in.Result = clampHeadRunes(in.Result, maxTaskResultRunes)
	var out ExtractionVerdictOutput
	err := runTyped(ctx, r, TaskExtractionVerdict, in, nil, &out)
	return out, err
}

// RunSkillStepConsistency judges whether one skill-step advance is consistent.
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
