package extractionx

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func ptrState(s string) *domain.AgentState { v := domain.AgentState(s); return &v }
func ptrStr(s string) *string              { return &s }
func ptrI64(i int64) *int64                { return &i }

func TestEvaluateConditionLeaves(t *testing.T) {
	s := signals{AgentState: "waiting", RuntimeStatus: "running", Tail: "tests passed", MsSinceOutput: 5000}

	if !evaluateCondition(domain.WatchCondition{StateIs: ptrState("waiting")}, s) {
		t.Error("stateIs should match")
	}
	if !evaluateCondition(domain.WatchCondition{Contains: ptrStr("passed")}, s) {
		t.Error("contains should match")
	}
	if evaluateCondition(domain.WatchCondition{Contains: ptrStr("failed")}, s) {
		t.Error("contains should not match absent text")
	}
	if !evaluateCondition(domain.WatchCondition{Regex: ptrStr(`tests?\s+passed`)}, s) {
		t.Error("regex should match")
	}
	if !evaluateCondition(domain.WatchCondition{NoOutputForMs: ptrI64(3000)}, s) {
		t.Error("noOutputForMs should fire when idle >= threshold")
	}
	if evaluateCondition(domain.WatchCondition{NoOutputForMs: ptrI64(9000)}, s) {
		t.Error("noOutputForMs should not fire below threshold")
	}
	// modelJudge is unsupported here → always false.
	if evaluateCondition(domain.WatchCondition{ModelJudge: ptrStr("is it done?")}, s) {
		t.Error("modelJudge must evaluate false in extraction")
	}
}

func TestEvaluateConditionComposite(t *testing.T) {
	s := signals{AgentState: "waiting", Tail: "ready"}
	any := domain.WatchCondition{Any: []domain.WatchCondition{
		{StateIs: ptrState("exited")}, {Contains: ptrStr("ready")},
	}}
	if !evaluateCondition(any, s) {
		t.Error("any should match second branch")
	}
	all := domain.WatchCondition{All: []domain.WatchCondition{
		{StateIs: ptrState("waiting")}, {Contains: ptrStr("ready")},
	}}
	if !evaluateCondition(all, s) {
		t.Error("all should match both branches")
	}
	not := domain.WatchCondition{Not: &domain.WatchCondition{StateIs: ptrState("exited")}}
	if !evaluateCondition(not, s) {
		t.Error("not should invert a false leaf")
	}
}

func TestNextOutputStateTracksSilence(t *testing.T) {
	// First observation: ms is 0 (just seen now).
	st1, ms1 := nextOutputState(nil, "hello", 1000)
	if ms1 != 0 {
		t.Errorf("first ms = %d, want 0", ms1)
	}
	// Unchanged tail: outAt preserved, so elapsed grows.
	_, ms2 := nextOutputState(&st1, "hello", 4000)
	if ms2 != 3000 {
		t.Errorf("unchanged ms = %d, want 3000", ms2)
	}
	// Changed tail: outAt resets to now, elapsed back to 0.
	_, ms3 := nextOutputState(&st1, "world", 4000)
	if ms3 != 0 {
		t.Errorf("changed ms = %d, want 0", ms3)
	}
}

func TestResolveBaseWaitCoercion(t *testing.T) {
	// Empty wait object → SETTLED_WAIT (any of waiting/completed/exited).
	r, errMsg := resolveBase(baseArgs{TerminalIDs: []string{"t"}, Wait: []byte(`{}`)})
	if errMsg != "" {
		t.Fatalf("unexpected error: %s", errMsg)
	}
	if r.wait == nil || len(r.wait.Any) != 3 {
		t.Fatalf("empty wait not coerced to settled: %+v", r.wait)
	}
	// Defaults applied.
	if r.pollIntervalMs != 2000 || r.maxAttempts != 30 || r.tailBytes != 12_000 || r.maxTokens != 1024 {
		t.Errorf("defaults wrong: %+v", r)
	}
	// A real condition decodes through.
	r2, e2 := resolveBase(baseArgs{TerminalIDs: []string{"t"}, Wait: []byte(`{"stateIs":"waiting"}`)})
	if e2 != "" || r2.wait == nil || r2.wait.StateIs == nil || *r2.wait.StateIs != domain.AgentWaiting {
		t.Errorf("stateIs wait not decoded: %+v err=%s", r2.wait, e2)
	}
	// A degenerate condition (the strict union rejects {"foo":1}) fails.
	if _, e3 := resolveBase(baseArgs{TerminalIDs: []string{"t"}, Wait: []byte(`{"foo":1}`)}); e3 == "" {
		t.Error("expected error for invalid wait condition")
	}
}

func TestRejectModelJudge(t *testing.T) {
	wait := &domain.WatchCondition{ModelJudge: ptrStr("done?")}
	if res, ok := rejectModelJudge(wait); !ok || res.Ok || res.Error.Code != "UNSUPPORTED_CONDITION" {
		t.Errorf("modelJudge should be rejected: ok=%v res=%+v", ok, res)
	}
	if _, ok := rejectModelJudge(&domain.WatchCondition{StateIs: ptrState("waiting")}); ok {
		t.Error("clean wait should not be rejected")
	}
}
