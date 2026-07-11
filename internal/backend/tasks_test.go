package backend

import (
	"context"
	"encoding/json"
	"errors"
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

// `{}` decodes cleanly but yields structurally empty required results; every helper
// with obviously-required output fields must reject it with a typed error.
func TestRunTypedValidateRejectsStructurallyEmptyResults(t *testing.T) {
	empty := `{}`

	_, err := RunWatcherClassify(context.Background(), withOutput(empty), WatcherClassifyInput{Tail: "x"})
	assertTaskOutputError(t, err, TaskWatcherClassify)

	_, err = RunTerminalJudge(context.Background(), withOutput(empty), TerminalJudgeInput{Question: "done?"})
	assertTaskOutputError(t, err, TaskTerminalJudge)

	_, err = RunSkillStepConsistency(context.Background(), withOutput(empty), SkillStepConsistencyInput{SkillID: "s"})
	assertTaskOutputError(t, err, TaskSkillStepConsistency)

	_, err = RunTerminalExtractJSON(context.Background(), withOutput(empty), TerminalExtractJSONInput{Instruction: "x"}, nil)
	assertTaskOutputError(t, err, TaskTerminalExtractJSON)

	_, err = RunExtractionVerdict(context.Background(), withOutput(empty), ExtractionVerdictInput{Result: "r", Condition: "c"})
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
