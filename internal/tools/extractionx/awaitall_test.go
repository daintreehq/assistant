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
	// deep, when set, serves a per-terminal deep terminal.getOutput value. Pure-FSM
	// awaitAll must NEVER read output, so deepCalls should stay 0 in await tests.
	deep      map[string]string
	deepCalls int
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
func (r *cohortReader) ReadOutput(_ context.Context, id string, _ int) OutputReadResult {
	r.deepCalls++
	if v, ok := r.deep[id]; ok {
		return OutputReadResult{OK: true, Value: v}
	}
	return OutputReadResult{OK: false}
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

// The cohort finishes on STAGGERED ticks: each agent settles the round it transitions
// working→waiting (pure FSM — no judge, no extra "quiet" round), and the wait resolves
// once all three are done.
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

	if attempts != 4 {
		t.Fatalf("each agent settles the round it goes working→waiting → done on attempt 4, got %d", attempts)
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
	if router.judgeCalls != 0 {
		t.Fatalf("pure-FSM awaitAll must make NO model judge call, got %d", router.judgeCalls)
	}
	if reader.deepCalls != 0 {
		t.Fatalf("pure-FSM awaitAll must make NO deep getOutput read, got %d", reader.deepCalls)
	}
}

// Pure-FSM guarantee: awaitAll settles on agentState ALONE — it never reads terminal
// output and never calls the model, regardless of what recentOutput holds (a blank,
// bottom-padded Codex viewport included). This is what removes the read burst that
// tripped Daintree's getOutput throttle and the per-tick judge latency.
func TestAwaitCohort_PureFSM_NoModelNoDeepRead(t *testing.T) {
	reader := &cohortReader{
		seq: []map[string]TerminalStatusEntry{
			{"t1": ent("working", "", "thinking…")},
			{"t1": ent("waiting", "prompt", "\r\n\r\n\r\n\r\n")}, // blank/padded tail — irrelevant now
		},
		deep: map[string]string{"t1": "should never be read"},
	}
	router := &safeRouter{}
	deps := Deps{Reader: reader, Router: router}
	ids := []string{"t1"}

	out, attempts := awaitCohort(context.Background(), deps, ids, 0, 6, 0,
		clockSeq(0, 2000, 4000, 6000, 8000, 10000, 12000))

	if attempts != 2 {
		t.Fatalf("working→waiting (seen working) settles on attempt 2, got %d", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if !allFinished || per["t1"]["status"] != "finished" {
		t.Fatalf("t1 should be finished on the working→waiting transition, got per=%+v", per)
	}
	if router.judgeCalls != 0 {
		t.Fatalf("no model call allowed, got %d judge calls", router.judgeCalls)
	}
	if reader.deepCalls != 0 {
		t.Fatalf("no deep getOutput read allowed, got %d", reader.deepCalls)
	}
}

// Pre-start guard: a terminal that reads "waiting" from the very first tick (never seen
// working) must NOT be declared finished while it is still under the spawn grace — that
// could be a pre-start prompt. With zero evidence it ever worked, it rides to the cap and
// is reported "still working" (allFinished=false), NOT a fabricated "finished" — so the
// caller reads the tail and self-heals rather than relaying a never-started agent.
func TestAwaitCohort_NeverWorkedWaitingHoldsForGrace(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("waiting", "", "$ ")}, // waiting from the start, never observed working
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1"}

	// Clock stays well under FinishSettleGraceMS (20s) for the whole bounded budget.
	out, attempts := awaitCohort(context.Background(), deps, ids, 0, 3, 0, clockSeq(0, 2000, 4000))
	if attempts != 3 {
		t.Fatalf("a never-worked 'waiting' must hold until the cap (grace not elapsed), got %d attempts", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if allFinished {
		t.Fatal("a never-worked, under-grace 'waiting' must NOT count as finished (allFinished=false)")
	}
	if per["t1"]["status"] != "working" {
		t.Fatalf("a never-worked 'waiting' under grace should report still-working, got %v", per["t1"])
	}

	// But once the grace elapses, the same state settles as finished (fast agent we never
	// caught mid-work).
	reader2 := &cohortReader{seq: []map[string]TerminalStatusEntry{{"t1": ent("waiting", "", "$ ")}}}
	out2, _ := awaitCohort(context.Background(), Deps{Reader: reader2, Router: &safeRouter{}}, ids, 0, 2, 0,
		clockSeq(0, domain.FinishSettleGraceMS+1))
	_, per2 := awaitResult(t, out2, ids)
	if per2["t1"]["status"] != "finished" {
		t.Fatalf("past the spawn grace a stable 'waiting' settles finished, got %v", per2["t1"])
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
