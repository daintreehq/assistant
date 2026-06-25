package skill

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// debugCtx builds a live-session ToolContext with debug logging routed at a temp dir,
// so the observability side-channels (issue #240) actually write where the test reads.
func debugCtx(dir string) *tools.ToolContext {
	return &tools.ToolContext{
		Config:    config.AppConfig{DebugLog: true, LogDir: dir},
		SessionID: "sess1",
		RunID:     "run-xyz",
	}
}

// readLogs concatenates every *.log written under dir (the debuglog filename carries a
// date + random id, so glob rather than guess the name).
func readLogs(t *testing.T, dir string) string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatalf("glob logs: %v", err)
	}
	var b strings.Builder
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			t.Fatalf("read %s: %v", m, err)
		}
		b.Write(data)
	}
	return b.String()
}

// okAnswer is a passing consistency verdict.
func okAnswer() domain.ModelJudgeAnswer {
	return domain.ModelJudgeAnswer{Reason: "normal advance", Confidence: 0.2, Matched: false}
}

// failInsertStore reports no existing run then fails to persist — exercises the path
// where the side-channel must NOT fire (a failed write is not an "advance occurred").
type failInsertStore struct{}

func (failInsertStore) GetSkillRunState(context.Context, string, string) (*domain.SkillRunStateRecord, error) {
	return nil, nil
}
func (failInsertStore) InsertSkillRunState(context.Context, domain.SkillRunStateRecord) (string, error) {
	return "", errors.New("db down")
}
func (failInsertStore) UpdateSkillRunState(context.Context, domain.SkillRunStateRecord) error {
	return errors.New("db down")
}

// With debug logging OFF, neither the delta log nor the consistency judge run — the
// feature is zero-cost in a normal session.
func TestStepAdvanceNoObservabilityWhenDebugOff(t *testing.T) {
	dir := t.TempDir()
	called := false
	deps := Deps{
		Store: &memStore{},
		CheckConsistency: func(context.Context, ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
			called = true
			return okAnswer(), nil
		},
	}
	tool := find(Tools(deps), "skill.step.advance")
	ctx := &tools.ToolContext{Config: config.AppConfig{DebugLog: false, LogDir: dir}, SessionID: "sess1", RunID: "r"}
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1,"nextStep":2}`), ctx)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if called {
		t.Fatal("CheckConsistency must not run when DebugLog is off")
	}
	if logs := readLogs(t, dir); logs != "" {
		t.Fatalf("expected no debug log when DebugLog off, got:\n%s", logs)
	}
}

// With debug logging ON and no judge wired, the free state-delta line fires with the
// transition facts and no consistency event is emitted.
func TestStepAdvanceDeltaLoggedWithoutChecker(t *testing.T) {
	dir := t.TempDir()
	tool := find(Tools(Deps{Store: &memStore{}}), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"mySkill","completedStep":2,"nextStep":3}`), debugCtx(dir))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	logs := readLogs(t, dir)
	for _, want := range []string{
		"skill.step.delta",
		"skillId=mySkill",
		"completedStep=2",
		"nextCurrentStep=3",
		"runId=run-xyz",
		"status=done",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("delta log missing %q\n---\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "skill.step.consistency") {
		t.Errorf("no consistency event expected without a checker\n---\n%s", logs)
	}
}

// A finished run (no nextStep) renders nextStep as the literal null in the delta line.
func TestStepAdvanceDeltaFinishRendersNull(t *testing.T) {
	dir := t.TempDir()
	tool := find(Tools(Deps{Store: &memStore{}}), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":4}`), debugCtx(dir))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	logs := readLogs(t, dir)
	if !strings.Contains(logs, "nextStep=null") {
		t.Errorf("finish delta should render nextStep=null\n---\n%s", logs)
	}
	if !strings.Contains(logs, "nextRunStatus=completed") {
		t.Errorf("finish delta should record nextRunStatus=completed\n---\n%s", logs)
	}
}

// With a judge wired, both the delta and the consistency verdict are logged, the judge
// receives the populated transition input, and a flag is surfaced as flagged=true.
func TestStepAdvanceConsistencyVerdictLogged(t *testing.T) {
	dir := t.TempDir()
	var got ConsistencyCheckInput
	deps := Deps{
		Store: &memStore{},
		CheckConsistency: func(_ context.Context, in ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
			got = in
			return domain.ModelJudgeAnswer{Reason: "step 5 jumped past 2-4", Confidence: 0.9, Matched: true}, nil
		},
	}
	tool := find(Tools(deps), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":5,"nextStep":6}`), debugCtx(dir))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if got.CompletedStep != 5 {
		t.Errorf("judge input completedStep = %d, want 5", got.CompletedStep)
	}
	if got.RunID != "run-xyz" {
		t.Errorf("judge input runId = %q, want run-xyz", got.RunID)
	}
	if got.NextStep == nil || *got.NextStep != 6 {
		t.Errorf("judge input nextStep = %v, want 6", got.NextStep)
	}
	logs := readLogs(t, dir)
	for _, want := range []string{
		"skill.step.delta",
		"skill.step.consistency",
		"checkOk=true",
		"flagged=true",
		"confidence=0.9",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("consistency log missing %q\n---\n%s", want, logs)
		}
	}
}

// A judge that returns an error logs checkOk=false with the error and never breaks the
// tool call; the failed check is distinguishable from a passing verdict (no flagged).
func TestStepAdvanceConsistencyErrorLoggedSafely(t *testing.T) {
	dir := t.TempDir()
	deps := Deps{
		Store: &memStore{},
		CheckConsistency: func(context.Context, ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
			return domain.ModelJudgeAnswer{}, errors.New("model timeout")
		},
	}
	tool := find(Tools(deps), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1,"nextStep":2}`), debugCtx(dir))
	if !res.Ok {
		t.Fatalf("a failed consistency check must not break the tool call, got %+v", res.Error)
	}
	logs := readLogs(t, dir)
	if !strings.Contains(logs, "skill.step.consistency") || !strings.Contains(logs, "checkOk=false") {
		t.Errorf("expected a checkOk=false consistency event\n---\n%s", logs)
	}
	if !strings.Contains(logs, "model timeout") {
		t.Errorf("consistency error event should carry the error\n---\n%s", logs)
	}
	if strings.Contains(logs, "flagged=") {
		t.Errorf("a failed check must not record a flagged verdict\n---\n%s", logs)
	}
}

// A panicking judge is contained by the recover guard — the tool still returns Ok and
// the delta (logged before the judge runs) is intact.
func TestStepAdvanceConsistencyPanicSafe(t *testing.T) {
	dir := t.TempDir()
	deps := Deps{
		Store: &memStore{},
		CheckConsistency: func(context.Context, ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
			panic("boom")
		},
	}
	tool := find(Tools(deps), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1,"nextStep":2}`), debugCtx(dir))
	if !res.Ok {
		t.Fatalf("a panicking consistency check must not break the tool call, got %+v", res.Error)
	}
	if logs := readLogs(t, dir); !strings.Contains(logs, "skill.step.delta") {
		t.Errorf("delta should be logged before the panicking check\n---\n%s", logs)
	}
}

// When persistence fails, the advance did not occur — no observability event fires and
// the judge never runs (the side-channel is gated on a successful write).
func TestStepAdvanceNoObservabilityOnPersistFailure(t *testing.T) {
	dir := t.TempDir()
	called := false
	deps := Deps{
		Store: failInsertStore{},
		CheckConsistency: func(context.Context, ConsistencyCheckInput) (domain.ModelJudgeAnswer, error) {
			called = true
			return okAnswer(), nil
		},
	}
	tool := find(Tools(deps), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1,"nextStep":2}`), debugCtx(dir))
	if res.Ok {
		t.Fatal("expected failure when persistence fails")
	}
	if called {
		t.Fatal("consistency check must not run when the advance did not persist")
	}
	if logs := readLogs(t, dir); logs != "" {
		t.Fatalf("expected no observability log on persist failure, got:\n%s", logs)
	}
}
