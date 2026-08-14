package extractionx

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
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
	// live is the canonical roster ListTerminals returns (drives the handler's id-resolution
	// path). The direct awaitCohort tests bypass the handler, so they leave it nil.
	live []string
	// liveOK forces ListTerminals to report ok=true even with an EMPTY live slice, so a test
	// can exercise the "roster readable but empty" fail-open path distinctly from an
	// "unreadable roster" (ok=false). A non-empty live always reports ok=true.
	liveOK bool
}

func (r *cohortReader) Connected() bool { return true }
func (r *cohortReader) ListTerminals(_ context.Context) ([]string, bool) {
	return r.live, r.liveOK || len(r.live) > 0
}
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

func (r *safeRouter) ExtractText(_ context.Context, _ string, _ []string, _ string) (string, bool, error) {
	return "", false, nil
}
func (r *safeRouter) ExtractJSON(_ context.Context, _ string, _ []string, _ string, _ map[string]any) (any, error) {
	return nil, nil
}
func (r *safeRouter) Verdict(_ context.Context, _ string, _ string) (bool, string, error) {
	return false, "", nil
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
	res := buildAwaitResult(ids, out, 0, 0, false, 0, false)
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

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 10, 0,
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

// A terminal closed MID-WAIT comes back from Daintree as a PRESENT entry marked
// "terminal not found" (the batch never omits an unknown id), so the cohort wait
// must settle it as gone instead of stranding every live sibling behind it until
// the attempt cap — the exact failure when the user closes one agent of five
// while an await is running.
func TestAwaitCohort_DroppedTerminalSettlesAsGone(t *testing.T) {
	gone := TerminalStatusEntry{NotFound: true}
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1"), "t2": ent("working", "", "w2")},
		{"t1": gone, "t2": ent("working", "", "w2b")},
		{"t1": gone, "t2": ent("waiting", "", "done t2")},
	}}
	router := &safeRouter{}
	deps := Deps{Reader: reader, Router: router}
	ids := []string{"t1", "t2"}

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 10, 0,
		clockSeq(0, 2000, 4000, 6000, 8000))

	if attempts != 3 {
		t.Fatalf("wait should resolve on attempt 3 (t1 gone on 2, t2 finished on 3), got %d", attempts)
	}
	allFinished, per := awaitResult(t, out, ids)
	if !allFinished {
		t.Fatalf("the dropped terminal must settle, not strand the cohort: per=%+v", per)
	}
	if per["t1"]["status"] != "finished" || per["t1"]["reason"] != "terminal is gone (closed or exited)" {
		t.Fatalf("t1 should settle as gone with the explicit reason, got %v", per["t1"])
	}
	if per["t2"]["status"] != "finished" {
		t.Fatalf("t2 should finish normally, got %v", per["t2"])
	}
	if router.judgeCalls != 0 || reader.deepCalls != 0 {
		t.Fatalf("the gone settle must stay pure-FSM (judges=%d deepReads=%d)", router.judgeCalls, reader.deepCalls)
	}
}

// EVERY terminal dropped: not-found entries are PRESENT in the response, so the
// "total miss is a transport hiccup" guard must not apply — the whole cohort
// settles as gone on the first poll instead of grinding to the attempt cap.
func TestAwaitCohort_AllDroppedSettles(t *testing.T) {
	gone := TerminalStatusEntry{NotFound: true}
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": gone, "t2": gone},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1", "t2"}

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 10, 0, clockSeq(0, 2000))

	if attempts != 1 {
		t.Fatalf("an all-dropped cohort should settle on the first poll, got %d attempts", attempts)
	}
	_, per := awaitResult(t, out, ids)
	for _, id := range ids {
		if per[id]["status"] != "finished" || per[id]["reason"] != "terminal is gone (closed or exited)" {
			t.Fatalf("%s should settle as gone, got %v", id, per[id])
		}
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

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 6, 0,
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
	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 3, 0, clockSeq(0, 2000, 4000))
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
	out2, _, _ := awaitCohort(context.Background(), Deps{Reader: reader2, Router: &safeRouter{}}, ids, 0, 2, 0,
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

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 4, 0,
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

	out, _, _ := awaitCohort(context.Background(), deps, ids, 0, 4, 0, clockSeq(0, 2000))
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

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 30, 0, clockSeq(0, 2000))
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

// The result names the actionable non-finished sets at the TOP LEVEL: stillWorking (timed
// out at the cap) and askingQuestion (blocked on the orchestrator), as ID-only arrays in
// INPUT order — so the caller re-awaits the stragglers directly without scanning perTerminal.
// Settled terminals (finished/failed) appear in neither set.
func TestBuildAwaitResult_NamesStragglersAndQuestions(t *testing.T) {
	ids := []string{"t1", "t2", "t3", "t4"}
	outcomes := map[string]*awaitOutcome{
		"t1": {status: "finished", finished: true},
		"t2": nil, // timed out, still working at the cap
		"t3": {status: "question", finished: false, reason: "asking a question"},
		"t4": nil, // also timed out — proves multi-element ordering
	}
	res := buildAwaitResult(ids, outcomes, 5, 1234, false, 0, false)
	if !res.Ok {
		t.Fatal("buildAwaitResult returned not-ok")
	}
	m := res.Result.(map[string]any)

	if af, _ := m["allFinished"].(bool); af {
		t.Fatal("a still-working + a question agent must make allFinished=false")
	}
	sw, _ := m["stillWorking"].([]string)
	if len(sw) != 2 || sw[0] != "t2" || sw[1] != "t4" {
		t.Fatalf("stillWorking should be exactly [t2 t4] in input order, got %v", sw)
	}
	aq, _ := m["askingQuestion"].([]string)
	if len(aq) != 1 || aq[0] != "t3" {
		t.Fatalf("askingQuestion should be exactly [t3], got %v", aq)
	}
	// A settled (finished or failed) terminal must never leak into a straggler set.
	for _, id := range append(append([]string{}, sw...), aq...) {
		if id == "t1" {
			t.Fatal("a finished terminal must not appear in a straggler set")
		}
	}
}

// TestBuildAwaitResult_SummaryPluralizesAgents pins the human-readable summary count:
// it reads "of N agents" for a cohort and "of 1 agent" for a single terminal, never the
// clumsy "agent(s)" placeholder.
func TestBuildAwaitResult_SummaryPluralizesAgents(t *testing.T) {
	many := buildAwaitResult(
		[]string{"t1", "t2", "t3"},
		map[string]*awaitOutcome{
			"t1": {status: "finished", finished: true},
			"t2": {status: "finished", finished: true},
			"t3": {status: "finished", finished: true},
		}, 1, 10, false, 0, false)
	if got := many.Summary; !strings.Contains(got, "of 3 agents") || strings.Contains(got, "agent(s)") {
		t.Errorf("cohort summary = %q, want it to read \"of 3 agents\" and drop \"agent(s)\"", got)
	}
	one := buildAwaitResult(
		[]string{"t1"},
		map[string]*awaitOutcome{"t1": {status: "finished", finished: true}}, 1, 10, false, 0, false)
	if got := one.Summary; !strings.Contains(got, "of 1 agent)") || strings.Contains(got, "agents") {
		t.Errorf("single-agent summary = %q, want singular \"of 1 agent)\"", got)
	}
}

// When every terminal settled, stillWorking and askingQuestion are present as non-nil EMPTY
// slices so they serialize as JSON [] (never null) — a caller that iterates them
// unconditionally for a re-await never trips over a null.
func TestBuildAwaitResult_EmptyStragglerSetsSerializeAsArrays(t *testing.T) {
	ids := []string{"t1", "t2"}
	code := 2
	outcomes := map[string]*awaitOutcome{
		"t1": {status: "finished", finished: true},
		"t2": {status: "failed", finished: true, exitCode: &code, reason: "exited with code 2"},
	}
	res := buildAwaitResult(ids, outcomes, 1, 10, false, 0, false)
	m := res.Result.(map[string]any)

	sw, ok := m["stillWorking"].([]string)
	if !ok || sw == nil {
		t.Fatalf("stillWorking must be a non-nil []string, got %#v", m["stillWorking"])
	}
	aq, ok := m["askingQuestion"].([]string)
	if !ok || aq == nil {
		t.Fatalf("askingQuestion must be a non-nil []string, got %#v", m["askingQuestion"])
	}
	if len(sw) != 0 || len(aq) != 0 {
		t.Fatalf("all settled → both straggler sets empty, got stillWorking=%v askingQuestion=%v", sw, aq)
	}
	// Marshal the whole result and assert the literal [] — catches a nil/null regression a
	// type assertion alone would miss (the model relies on iterating these arrays).
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	if !strings.Contains(js, `"stillWorking":[]`) {
		t.Fatalf(`expected "stillWorking":[] in JSON, got %s`, js)
	}
	if !strings.Contains(js, `"askingQuestion":[]`) {
		t.Fatalf(`expected "askingQuestion":[] in JSON, got %s`, js)
	}
}

// End-to-end through the REAL poll loop: the existing awaitResult helper discards the new
// top-level arrays, so assert them straight off buildAwaitResult fed by awaitCohort's output.
// t1 finishes (working→waiting), t2 stays working to the cap → stillWorking names exactly t2.
func TestAwaitCohort_TopLevelArraysFromRealPollLoop(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1"), "t2": ent("working", "", "stuck")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "stuck")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "stuck")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1", "t2"}

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 3, 0, clockSeq(0, 2000, 4000, 6000))
	res := buildAwaitResult(ids, out, attempts, 0, false, 0, false)
	m := res.Result.(map[string]any)

	if af, _ := m["allFinished"].(bool); af {
		t.Fatal("a still-working agent must make allFinished=false")
	}
	if sw, _ := m["stillWorking"].([]string); len(sw) != 1 || sw[0] != "t2" {
		t.Fatalf("stillWorking should be exactly [t2] from the real loop, got %v", sw)
	}
	if aq, _ := m["askingQuestion"].([]string); len(aq) != 0 {
		t.Fatalf("no question agent → askingQuestion should be empty, got %v", aq)
	}
	if reader.deepCalls != 0 {
		t.Fatalf("pure-FSM awaitAll must make NO deep getOutput read, got %d", reader.deepCalls)
	}
}

// A question on one terminal must NOT short-circuit a still-working peer: the cohort gate is
// "all settled", so t2 keeps polling until it transitions even after t1 settles on a question.
func TestAwaitCohort_QuestionDoesNotAbortUnsettledPeer(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("waiting", "question", "Which file?"), "t2": ent("working", "", "w2")},
		{"t1": ent("waiting", "question", "Which file?"), "t2": ent("working", "", "w2b")},
		{"t1": ent("waiting", "question", "Which file?"), "t2": ent("waiting", "", "done t2")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}}
	ids := []string{"t1", "t2"}

	out, attempts, _ := awaitCohort(context.Background(), deps, ids, 0, 5, 0, clockSeq(0, 2000, 4000, 6000))
	if attempts != 3 {
		t.Fatalf("t2 should keep polling past t1's question until it finishes on tick 3, got %d", attempts)
	}
	res := buildAwaitResult(ids, out, attempts, 0, false, 0, false)
	m := res.Result.(map[string]any)
	if aq, _ := m["askingQuestion"].([]string); len(aq) != 1 || aq[0] != "t1" {
		t.Fatalf("askingQuestion should be exactly [t1], got %v", aq)
	}
	if sw, _ := m["stillWorking"].([]string); len(sw) != 0 {
		t.Fatalf("t2 finished before the cap → stillWorking should be empty, got %v", sw)
	}
	per := map[string]map[string]any{}
	for _, e := range m["perTerminal"].([]map[string]any) {
		per[e["terminalId"].(string)] = e
	}
	if per["t2"]["status"] != "finished" {
		t.Fatalf("t2 should finish despite t1's question, got %v", per["t2"])
	}
}

// A message typed mid-wait breaks the cohort wait EARLY: the loop stops the instant it
// sees a pending injection, hands back whatever has settled, and flags interruptedByUser
// so the orchestrator reads the user's message (already folded into the turn) before
// re-awaiting. This is what unblocks a multi-minute await the moment the human wants to
// redirect — e.g. "that agent errored, re-spawn it" — instead of forcing them to wait or
// hit Esc (which would nuke the whole turn).
func TestAwaitCohort_InterruptedByPendingInjection(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1"), "t2": ent("working", "", "w2")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "w2b")},
		{"t1": ent("waiting", "", "done t1"), "t2": ent("working", "", "w2c")},
	}}
	// Pending only from the SECOND check on: tick 1 settles nothing, tick 2 settles t1,
	// and the injection check then breaks the wait with t2 still working — proving partial
	// state survives the interrupt.
	calls := 0
	deps := Deps{
		Reader:            reader,
		Router:            &safeRouter{},
		InjectionsPending: func() bool { calls++; return calls >= 2 },
	}
	ids := []string{"t1", "t2"}

	out, attempts, interrupted := awaitCohort(context.Background(), deps, ids, 0, 30, 0,
		clockSeq(0, 2000, 4000, 6000))
	if !interrupted {
		t.Fatal("a pending injection mid-wait must interrupt the cohort wait")
	}
	if attempts != 2 {
		t.Fatalf("the wait should stop on tick 2 (when the injection appears), far short of the 30 cap, got %d", attempts)
	}
	res := buildAwaitResult(ids, out, attempts, 0, interrupted, 0, false)
	m := res.Result.(map[string]any)
	if iu, _ := m["interruptedByUser"].(bool); !iu {
		t.Fatalf("the result must carry interruptedByUser:true, got %+v", m)
	}
	if !strings.HasPrefix(res.Summary, "Paused — you sent a message.") {
		t.Fatalf("the summary should LEAD with the pause cue, got %q", res.Summary)
	}
	// Partial state is preserved across the interrupt.
	per := map[string]map[string]any{}
	for _, e := range m["perTerminal"].([]map[string]any) {
		per[e["terminalId"].(string)] = e
	}
	if per["t1"]["status"] != "finished" {
		t.Fatalf("t1 settled before the interrupt → finished, got %v", per["t1"])
	}
	if per["t2"]["status"] != "working" {
		t.Fatalf("t2 was still working at the interrupt → working, got %v", per["t2"])
	}
	if sw, _ := m["stillWorking"].([]string); len(sw) != 1 || sw[0] != "t2" {
		t.Fatalf("stillWorking should name the unsettled t2 for a later re-await, got %v", sw)
	}
}

// With NO injector wired (deps.InjectionsPending nil — tests, watcher/timer/workflow
// actors), the wait can NEVER be interrupted: it behaves exactly as before and the FSM
// runs to settle. This guards the gate so a non-interactive await is never cut short.
func TestAwaitCohort_NoInjectorNeverInterrupts(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "w1")},
		{"t1": ent("waiting", "", "done")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}} // InjectionsPending nil
	_, _, interrupted := awaitCohort(context.Background(), deps, []string{"t1"}, 0, 6, 0,
		clockSeq(0, 2000, 4000))
	if interrupted {
		t.Fatal("with no injector wired the wait must never report interrupted")
	}
}

// Issue #239: the tool surface itself must carry the re-await bound, so the ceiling is
// visible at point-of-use even when no orchestration skill is loaded.
//
// Locked on the tool DESCRIPTION only. It was previously repeated verbatim in the
// terminalIds schema too, and a rule stated twice is paid for on every model round —
// what matters is that it is somewhere the model reads before calling, not that it
// appears in both places.
func TestAwaitAllTool_DocumentsReawaitBound(t *testing.T) {
	tool := newAwaitAllTool(Deps{})
	if !strings.Contains(tool.Description, "at most twice") {
		t.Errorf("awaitAll Description should document the re-await bound (\"at most twice\")")
	}
	if !strings.Contains(tool.Description, "queue.publish") {
		t.Errorf("awaitAll Description should name the queue.publish escalation path")
	}
	// The schema's job is the SHAPE: full ids, never invented or abbreviated. No negative
	// assertion on the bound — forbidding an exact phrase tests wording placement rather
	// than whether the rule survives, and would fail a legitimate short reminder while a
	// paraphrased duplicate slipped through.
	if !strings.Contains(string(tool.Schema), "terminal-<uuid>") {
		t.Errorf("awaitAll terminalIds schema should still state the id SHAPE")
	}
}

// maxAttempts is validated against the RAISED opt-in ceiling (240): a known-slow cohort can
// pass up to 240, but 241+ and non-positive values are still rejected, and omitting it
// (handler defaults to 30) stays valid.
func TestAwaitArgsValidate_MaxAttemptsCeiling(t *testing.T) {
	mk := func(n int) *awaitArgs {
		return &awaitArgs{TerminalIDs: []string{"t1"}, MaxAttempts: &n}
	}
	for _, n := range []int{1, 30, 60, 120, 240} {
		if err := mk(n).Validate(); err != nil {
			t.Fatalf("maxAttempts=%d should be accepted, got %v", n, err)
		}
	}
	for _, n := range []int{0, -1, 241, 1000} {
		if err := mk(n).Validate(); err == nil {
			t.Fatalf("maxAttempts=%d should be rejected", n)
		}
	}
	if err := (&awaitArgs{TerminalIDs: []string{"t1"}}).Validate(); err != nil {
		t.Fatalf("omitted maxAttempts should be valid (defaults to 30), got %v", err)
	}
}

// The HANDLER canonicalizes a truncated/prefix id against the live roster before polling:
// the model's "terminal-5284bfef" prefix resolves to the full id and the cohort settles —
// instead of grinding to the cap reporting "still working" (the ses_f3fdeb08 bug).
func TestAwaitAllTool_ResolvesTruncatedPrefix(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	reader := &cohortReader{
		live: []string{full},
		seq:  []map[string]TerminalStatusEntry{{full: ent("completed", "", "done")}},
	}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["terminal-5284bfef"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("a resolvable prefix should succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if af, _ := m["allFinished"].(bool); !af {
		t.Fatalf("the prefix should resolve to %q and settle finished, got %+v", full, m)
	}
	// The result reports the CANONICAL id, not the truncated input.
	per := m["perTerminal"].([]map[string]any)
	if len(per) != 1 || per[0]["terminalId"] != full {
		t.Fatalf("perTerminal should carry the canonical id %q, got %+v", full, per)
	}
}

// An id that matches no live terminal fails LOUD and FAST (UNKNOWN_TERMINALS) instead of
// polling the whole budget and reporting "still working".
func TestAwaitAllTool_UnknownIDFailsFast(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	reader := &cohortReader{live: []string{full}} // roster live & non-empty, but no match
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["terminal-deadbeef"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if res.Ok {
		t.Fatalf("an unknown id must fail, got ok result %+v", res.Result)
	}
	if res.Error == nil || res.Error.Code != codeUnknownTerminals {
		t.Fatalf("want %s, got %+v", codeUnknownTerminals, res.Error)
	}
	if !strings.Contains(res.Error.Message, full) {
		t.Fatalf("the failure should name the live terminal id to steer the model, got %q", res.Error.Message)
	}
}

// Fail OPEN on an UNREADABLE roster (ListTerminals ok=false): resolution must never turn a
// transport hiccup into a hard failure — the ids pass through unchanged and the poll (with
// its own empty-read guard) proceeds.
func TestAwaitAllTool_FailsOpenOnUnreadableRoster(t *testing.T) {
	reader := &cohortReader{ // live nil + liveOK false ⇒ ListTerminals ok=false ⇒ fail open
		seq: []map[string]TerminalStatusEntry{{"t1": ent("completed", "", "done")}},
	}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("an unreadable roster must fail open and proceed, got %+v", res.Error)
	}
	if af, _ := res.Result.(map[string]any)["allFinished"].(bool); !af {
		t.Fatalf("with resolution skipped, the original id should poll and settle, got %+v", res.Result)
	}
}

// Fail OPEN on a READABLE-BUT-EMPTY roster (ListTerminals ok=true, zero terminals): also
// the #108 transport-hiccup symptom, so resolution must pass ids through, NOT reject them.
func TestAwaitAllTool_FailsOpenOnEmptyReadableRoster(t *testing.T) {
	reader := &cohortReader{ // liveOK=true, live empty ⇒ ok=true with an empty roster
		liveOK: true,
		seq:    []map[string]TerminalStatusEntry{{"t1": ent("completed", "", "done")}},
	}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("an empty readable roster must fail open and proceed, got %+v", res.Error)
	}
	if af, _ := res.Result.(map[string]any)["allFinished"].(bool); !af {
		t.Fatalf("with resolution skipped, the original id should poll and settle, got %+v", res.Result)
	}
}

// The extract handlers canonicalize ids the same way: a truncated prefix resolves through
// terminal.extract (read-once), and the result reports the canonical id.
func TestExtractTool_ResolvesTruncatedPrefix(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	reader := &cohortReader{
		live: []string{full},
		seq:  []map[string]TerminalStatusEntry{{full: ent("waiting", "", "the answer")}},
	}
	tool := newExtractTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["terminal-5284bfef"],"instruction":"get it"}`), nil)
	if !res.Ok {
		t.Fatalf("a resolvable prefix should succeed, got %+v", res.Error)
	}
	ids, _ := res.Result.(map[string]any)["terminalIds"].([]string)
	if len(ids) != 1 || ids[0] != full {
		t.Fatalf("extract should use the canonical id %q, got %v", full, ids)
	}
}

// terminal.extract.json fails fast on an unknown id (UNKNOWN_TERMINALS), same as awaitAll.
func TestExtractJSONTool_UnknownIDFailsFast(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	reader := &cohortReader{live: []string{full}}
	tool := newExtractJSONTool(Deps{Reader: reader, Router: &safeRouter{}})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["terminal-nope"],"instruction":"x","jsonSchema":"{\"type\":\"object\"}"}`), nil)
	if res.Ok {
		t.Fatalf("an unknown id must fail fast, got %+v", res.Result)
	}
	if res.Error == nil || res.Error.Code != codeUnknownTerminals {
		t.Fatalf("want %s, got %+v", codeUnknownTerminals, res.Error)
	}
}
