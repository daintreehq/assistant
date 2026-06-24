package extractionx

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// cohortReader scripts a SEQUENCE of multi-terminal status snapshots, one per
// ReadStatuses call (clamping to the last once exhausted), so a test can drive
// several agents through working→waiting on staggered ticks. awaitCohort reads once
// per tick (serially), so call counting needs no lock.
type cohortReader struct {
	seq  []map[string]TerminalStatusEntry
	call int
}

func (r *cohortReader) Connected() bool { return true }
func (r *cohortReader) ReadStatuses(_ context.Context, _ []string, _ bool) StatusReadResult {
	i := r.call
	if i >= len(r.seq) {
		i = len(r.seq) - 1
	}
	r.call++
	return StatusReadResult{OK: true, ByID: r.seq[i]}
}
func (r *cohortReader) ReadOutput(_ context.Context, _ string, _ int) OutputReadResult {
	return OutputReadResult{OK: false} // short tails come from recentOutput
}

// safeRouter is a concurrency-safe finished judge (awaitCohort fans the per-terminal
// judges out across goroutines). It reports finished when the tail contains "done".
type safeRouter struct {
	mu         sync.Mutex
	judgeCalls int
}

func (r *safeRouter) Chat(_ context.Context, _ domain.ModelTier, _ []ChatMessage, _ int) (ChatResult, error) {
	return ChatResult{}, nil
}
func (r *safeRouter) JSON(_ context.Context, _ domain.ModelTier, _ []ChatMessage, _ int) (any, error) {
	return nil, nil
}
func (r *safeRouter) Judge(_ context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error) {
	r.mu.Lock()
	r.judgeCalls++
	r.mu.Unlock()
	return domain.ModelJudgeAnswer{Matched: strings.Contains(in.Tail, "done"), Confidence: 0.9}, nil
}

func ent(state, reason, out string) TerminalStatusEntry {
	return TerminalStatusEntry{AgentState: state, WaitingReason: reason, RecentOutput: strp(out)}
}
func entExit(code int, out string) TerminalStatusEntry {
	return TerminalStatusEntry{AgentState: "exited", ExitCode: &code, RecentOutput: strp(out)}
}

func awaitResult(t *testing.T, out map[string]*awaitOutcome, ids []string) (bool, map[string]map[string]any) {
	t.Helper()
	res := buildAwaitResult(ids, out, 0, 0)
	if !res.Ok {
		t.Fatalf("buildAwaitResult returned not-ok")
	}
	m, _ := res.Result.(map[string]any)
	allFinished, _ := m["allFinished"].(bool)
	per := map[string]map[string]any{}
	for _, e := range m["perTerminal"].([]map[string]any) {
		per[e["terminalId"].(string)] = e
	}
	return allFinished, per
}

// The cohort finishes on STAGGERED ticks: each agent is judged independently the
// round after its tail goes quiet, and the wait resolves once all three are done.
func TestAwaitCohort_AllFinishStaggered(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1"), "t2": ent("working", "", "w2"), "t3": ent("working", "", "w3")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "w2b"), "t3": ent("working", "", "w3b")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("waiting", "", "done t2"), "t3": ent("working", "", "w3c")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("waiting", "", "done t2"), "t3": ent("waiting", "", "done t3")},
	}}
	router := &safeRouter{}
	deps := Deps{Reader: reader, Router: router}
	ids := []string{"t1", "t2", "t3"}

	out, attempts := awaitCohort(context.Background(), deps, ids, 0, 10, 0,
		clockSeq(0, 2000, 4000, 6000, 8000, 10000, 12000))

	if attempts != 5 {
		t.Fatalf("staggered cohort should settle on attempt 5 (each judged the round after its tail quiets), got %d", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if !allFinished {
		t.Fatalf("all three should be finished, got per=%+v", per)
	}
	for _, id := range ids {
		if per[id]["status"] != "finished" {
			t.Fatalf("%s should be finished, got %v", id, per[id])
		}
	}
	if router.judgeCalls != 3 {
		t.Fatalf("each agent judged once on its quiet tail → 3 judge calls, got %d", router.judgeCalls)
	}
}

// One stuck agent must not pin the wait: it runs to the cap and reports the laggard as
// still working (allFinished=false), with the finished agents still resolved.
func TestAwaitCohort_TimesOutStillWorking(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1"), "t2": ent("working", "", "stuck")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "stuck")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "stuck")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1", "t2"}

	out, attempts := awaitCohort(context.Background(), deps, ids, 0, 4, 0,
		clockSeq(0, 2000, 4000, 6000, 8000))
	if attempts != 4 {
		t.Fatalf("should run to the cap (4) waiting on the stuck agent, got %d", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if allFinished {
		t.Fatal("a still-working agent must make allFinished=false")
	}
	if per["t1"]["status"] != "finished" {
		t.Fatalf("t1 should be finished, got %v", per["t1"])
	}
	if per["t2"]["status"] != "working" {
		t.Fatalf("t2 should report still-working, got %v", per["t2"])
	}
}

// A nonzero exit is "done but FAILED": the model needs to know not to relay it as a
// valid answer. It still counts as settled, so allFinished stays true.
func TestAwaitCohort_FailedExitNonzero(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": entExit(1, "panic: boom"), "t2": ent("completed", "", "ok done")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1", "t2"}

	out, _ := awaitCohort(context.Background(), deps, ids, 0, 4, 0, clockSeq(0, 2000))
	allFinished, per := awaitResult(t, out, ids)
	if !allFinished {
		t.Fatalf("a failed + a completed agent are both DONE → allFinished, got per=%+v", per)
	}
	if per["t1"]["status"] != "failed" {
		t.Fatalf("t1 (exit 1) should be failed, got %v", per["t1"])
	}
	if code, _ := per["t1"]["exitCode"].(int); code != 1 {
		t.Fatalf("t1 should carry exitCode 1, got %v", per["t1"]["exitCode"])
	}
	if per["t2"]["status"] != "finished" {
		t.Fatalf("t2 (completed) should be finished, got %v", per["t2"])
	}
}

// An agent waiting on a QUESTION settles immediately as needs-attention (never judged,
// never waited to the cap) so the orchestrator can answer it; allFinished=false.
func TestAwaitCohort_QuestionShortCircuits(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("waiting", "question", "Which file?"), "t2": ent("completed", "", "done")},
	}}
	router := &safeRouter{}
	deps := Deps{Reader: reader, Router: router}
	ids := []string{"t1", "t2"}

	out, attempts := awaitCohort(context.Background(), deps, ids, 0, 30, 0, clockSeq(0, 2000))
	if attempts != 1 {
		t.Fatalf("a question + a completed agent both settle on the first tick, got %d attempts", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if allFinished {
		t.Fatal("an agent on a question must make allFinished=false (needs the orchestrator)")
	}
	if per["t1"]["status"] != "question" {
		t.Fatalf("t1 should be flagged as asking a question, got %v", per["t1"])
	}
	if router.judgeCalls != 0 {
		t.Fatalf("a question is settled deterministically — never judged, got %d judge calls", router.judgeCalls)
	}
}
