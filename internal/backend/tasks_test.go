package backend

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// scriptedRunner is a canned TaskRunner: it records the request and returns a fixed
// result, so the typed-helper validation paths are tested without HTTP.
type scriptedRunner struct {
	res    TaskResult
	err    error
	gotReq TaskRequest
}

func (s *scriptedRunner) RunTask(_ context.Context, req TaskRequest) (TaskResult, error) {
	s.gotReq = req
	return s.res, s.err
}

func withOutput(raw string) *scriptedRunner {
	return &scriptedRunner{res: TaskResult{Output: json.RawMessage(raw)}}
}

// TestCheckpointClampKeepsHeadAndTail pins the head+tail clamp: an over-budget
// checkpoint transcript keeps its HEAD (where a prior "[checkpoint | depth N]" note
// lives — the carry-forward rule needs it in hand) AND the freshest tail, joined by
// the elision marker, at exactly the task cap. A tail-only clamp silently cut the
// prior checkpoint out of every over-threshold auto-compaction.
func TestCheckpointClampKeepsHeadAndTail(t *testing.T) {
	head := "user: [checkpoint | depth 1] HEAD_DIRECTIVE_MARKER\n"
	filler := strings.Repeat("x", maxTaskTranscriptRunes*2)
	transcript := head + filler + "TAIL_MARKER_END"

	r := withOutput(`{"goal":"g"}`)
	if _, err := RunCheckpoint(context.Background(), r, CheckpointInput{Transcript: transcript}); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	got, _ := r.gotReq.Input["transcript"].(string)
	if n := len([]rune(got)); n != maxTaskTranscriptRunes {
		t.Fatalf("clamped transcript = %d runes, want exactly %d", n, maxTaskTranscriptRunes)
	}
	if !strings.HasPrefix(got, head) {
		t.Fatal("clamp dropped the head (prior checkpoint note)")
	}
	if !strings.Contains(got, transcriptElisionMarker) {
		t.Fatal("clamp missing the elision marker between head and tail")
	}
	if !strings.HasSuffix(got, "TAIL_MARKER_END") {
		t.Fatal("clamp dropped the freshest tail")
	}

	// Under budget → untouched (no marker injected).
	r2 := withOutput(`{"goal":"g"}`)
	if _, err := RunCheckpoint(context.Background(), r2, CheckpointInput{Transcript: "short"}); err != nil {
		t.Fatalf("RunCheckpoint short: %v", err)
	}
	if got, _ := r2.gotReq.Input["transcript"].(string); got != "short" {
		t.Fatalf("under-budget transcript must pass through unchanged, got %q", got)
	}
}

func assertTaskOutputError(t *testing.T, err error, task string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected *TaskOutputError, got nil", task)
	}
	var oe *TaskOutputError
	if !errors.As(err, &oe) {
		t.Fatalf("%s: expected *TaskOutputError, got %T: %v", task, err, err)
	}
	if oe.Task != task {
		t.Fatalf("TaskOutputError.Task = %q, want %q", oe.Task, task)
	}
}

// A wire-successful task round with NO output must be a typed error for every typed
// helper — a zero-valued struct is never a real answer.
func TestRunTypedEmptyOutputIsTypedError(t *testing.T) {
	r := &scriptedRunner{res: TaskResult{Output: nil}}
	_, err := RunCheckpoint(context.Background(), r, CheckpointInput{Transcript: "t"})
	assertTaskOutputError(t, err, TaskCheckpoint)

	_, err = RunTerminalSummarize(context.Background(), r, TerminalSummarizeInput{Tail: "x"})
	assertTaskOutputError(t, err, TaskTerminalSummarize)

	_, err = RunMemoryDistill(context.Background(), r, MemoryDistillInput{Transcript: "t"})
	assertTaskOutputError(t, err, TaskMemoryDistill)
}

// `{}` decodes cleanly but yields structurally empty required results; helpers whose
// output has an obviously-required field must reject it with a typed error. The
// judge/verdict tasks are deliberately NOT here: their backend schemas default
// reason to "" (see TestJudgeAndVerdictZeroValuesAreLegitimateNegatives).
func TestRunTypedValidateRejectsStructurallyEmptyResults(t *testing.T) {
	empty := `{}`

	_, err := RunWatcherClassify(context.Background(), withOutput(empty), WatcherClassifyInput{Tail: "x"})
	assertTaskOutputError(t, err, TaskWatcherClassify)

	_, err = RunTerminalExtractJSON(context.Background(), withOutput(empty), TerminalExtractJSONInput{Instruction: "x"}, nil)
	assertTaskOutputError(t, err, TaskTerminalExtractJSON)
}

// The backend JudgeOutput / ExtractionVerdictOutput schemas accept an empty reason
// (it defaults to ""), so an all-zero verdict is a legitimate NEGATIVE answer the
// client must accept — while a wire round with no output at all stays rejected.
func TestJudgeAndVerdictZeroValuesAreLegitimateNegatives(t *testing.T) {
	zeroJudge := `{"reason":"","confidence":0,"matched":false}`

	judge, err := RunTerminalJudge(context.Background(), withOutput(zeroJudge), TerminalJudgeInput{Question: "done?"})
	if err != nil || judge.Matched || judge.Reason != "" {
		t.Fatalf("terminal judge zero verdict: out=%+v err=%v, want accepted negative", judge, err)
	}

	skill, err := RunSkillStepConsistency(context.Background(), withOutput(zeroJudge), SkillStepConsistencyInput{SkillID: "s"})
	if err != nil || skill.Matched {
		t.Fatalf("skill consistency zero verdict: out=%+v err=%v, want accepted negative", skill, err)
	}

	verdict, err := RunExtractionVerdict(context.Background(), withOutput(`{"pass":false,"reason":""}`),
		ExtractionVerdictInput{Result: "r", Condition: "c"})
	if err != nil || verdict.Passed {
		t.Fatalf("extraction verdict zero verdict: out=%+v err=%v, want accepted negative", verdict, err)
	}

	// No output at all is still a typed error for all three.
	r := &scriptedRunner{res: TaskResult{Output: nil}}
	if _, err := RunTerminalJudge(context.Background(), r, TerminalJudgeInput{Question: "q"}); err == nil {
		t.Fatal("terminal judge with no output must stay rejected")
	}
	if _, err := RunSkillStepConsistency(context.Background(), r, SkillStepConsistencyInput{SkillID: "s"}); err == nil {
		t.Fatal("skill consistency with no output must stay rejected")
	}
	_, err = RunExtractionVerdict(context.Background(), r, ExtractionVerdictInput{Result: "r", Condition: "c"})
	assertTaskOutputError(t, err, TaskExtractionVerdict)
}

// Structurally sound results pass validation, and legitimately-sparse outputs
// (nothing to distill, an empty extracted text) are NOT rejected.
func TestRunTypedValidateAcceptsRealAndLegitimatelySparseResults(t *testing.T) {
	cls, err := RunWatcherClassify(context.Background(),
		withOutput(`{"classification":"working","confidence":0.9,"summary":"s","evidence":[],"recommendedAction":"none"}`),
		WatcherClassifyInput{Tail: "x"})
	if err != nil || cls.Classification != "working" {
		t.Fatalf("watcher classify: out=%+v err=%v", cls, err)
	}

	judge, err := RunTerminalJudge(context.Background(),
		withOutput(`{"reason":"tests finished","confidence":0.8,"matched":true}`),
		TerminalJudgeInput{Question: "done?"})
	if err != nil || !judge.Matched {
		t.Fatalf("terminal judge: out=%+v err=%v", judge, err)
	}

	// A REAL negative verdict (reason present) must not be confused with `{}`.
	verdict, err := RunExtractionVerdict(context.Background(),
		withOutput(`{"pass":false,"reason":"value not found"}`),
		ExtractionVerdictInput{Result: "r", Condition: "c"})
	if err != nil || verdict.Passed {
		t.Fatalf("extraction verdict: out=%+v err=%v", verdict, err)
	}

	// Nothing durable to distill is a legitimate empty result.
	distill, err := RunMemoryDistill(context.Background(), withOutput(`{"facts":[]}`),
		MemoryDistillInput{Transcript: "t"})
	if err != nil || len(distill.Facts) != 0 {
		t.Fatalf("memory distill: out=%+v err=%v", distill, err)
	}

	// An empty extracted text is representable and allowed.
	text, err := RunTerminalExtractText(context.Background(), withOutput(`{"text":""}`),
		TerminalExtractTextInput{Instruction: "x"})
	if err != nil || text.Text != "" {
		t.Fatalf("extract text: out=%+v err=%v", text, err)
	}

	// An all-empty checkpoint is a documented best-effort degradation (the agent's
	// compaction path mines IDs into it) and must NOT be rejected.
	cp, err := RunCheckpoint(context.Background(), withOutput(`{}`), CheckpointInput{Transcript: "t"})
	if err != nil || cp.Goal != "" {
		t.Fatalf("checkpoint degradation: out=%+v err=%v, want empty checkpoint with nil error", cp, err)
	}

	extract, err := RunTerminalExtractJSON(context.Background(),
		withOutput(`{"result":{"port":8080}}`), TerminalExtractJSONInput{Instruction: "x"}, nil)
	if err != nil || len(extract.Result) == 0 {
		t.Fatalf("extract json: out=%+v err=%v", extract, err)
	}
}

// A transport error still surfaces as-is (validation never masks it), and a nil out
// target skips the output requirement entirely.
func TestRunTypedTransportErrorAndNilTarget(t *testing.T) {
	wantErr := &Error{HTTPStatus: 502, Code: "http_error", Message: "bad gateway"}
	r := &scriptedRunner{err: wantErr}
	if _, err := RunTerminalJudge(context.Background(), r, TerminalJudgeInput{Question: "q"}); err != wantErr { //nolint:errorlint // identity check
		t.Fatalf("transport error = %v, want the runner's error unchanged", err)
	}

	if err := runTyped(context.Background(), &scriptedRunner{res: TaskResult{}}, "any_task", struct{}{}, nil, nil); err != nil {
		t.Fatalf("nil out with empty output = %v, want nil (nothing to decode)", err)
	}
}
