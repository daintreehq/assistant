package extractionx

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// retire_test.go pins the in-turn consumption contract: once a wait DIRECTLY
// observes an agent terminal settle (awaitAll finished/failed, or an extract wait
// that resolved as completion), the spawn-attached supervisor watcher is retired
// through the SupervisorRetirer seam — so it can't later re-announce a completion
// the conversation already contains as a stale attention event.

// fakeRetirer records RetireForTerminal calls; ret is the per-call return.
type fakeRetirer struct {
	mu    sync.Mutex
	calls []string // "<terminalId>:<settledStatus>"
	ret   int
}

func (r *fakeRetirer) RetireForTerminal(_ context.Context, terminalID, settledStatus string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, terminalID+":"+settledStatus)
	return r.ret
}

func (r *fakeRetirer) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// awaitAll retires the supervisor of every terminal it settled as DONE — finished
// AND failed both count (the model holds the outcome either way) — but never of a
// question terminal (it still needs supervision if the orchestrator drops it).
// The result surfaces watchersRetired so the model knows those watchers are gone.
func TestAwaitAllTool_RetiresSupervisorsOnConsumedCompletion(t *testing.T) {
	code := 2
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("completed", "", "done"),
		"t2": {AgentState: "exited", ExitCode: &code, RecentOutput: strp("boom")},
		"t3": ent("waiting", "question", "which one?"),
	}}}
	ret := &fakeRetirer{ret: 1}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}, Supervisors: ret})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1","t2","t3"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("await should succeed, got %+v", res.Error)
	}

	calls := ret.snapshot()
	want := map[string]bool{"t1:finished": true, "t2:failed": true}
	if len(calls) != 2 || !want[calls[0]] || !want[calls[1]] || calls[0] == calls[1] {
		t.Fatalf("expected exactly t1:finished and t2:failed to retire, got %v", calls)
	}
	m := res.Result.(map[string]any)
	if got, _ := m["watchersRetired"].(int); got != 2 {
		t.Fatalf("watchersRetired = %v, want 2", m["watchersRetired"])
	}
}

// Zero retirements (or no retirer wired at all) omit the watchersRetired field —
// the model only hears about watchers when one actually went away.
func TestAwaitAllTool_NoRetirementOmitsField(t *testing.T) {
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": ent("completed", "", "done"),
	}}}
	tool := newAwaitAllTool(Deps{Reader: reader, Router: &safeRouter{}}) // nil Supervisors
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("await should succeed, got %+v", res.Error)
	}
	if _, present := res.Result.(map[string]any)["watchersRetired"]; present {
		t.Fatal("watchersRetired must be absent when nothing retired")
	}
}

// retireConsumedSupervisors only fires when a WAIT genuinely observed completion:
// the coerced wait:{} settle, or any wait that ended with every target exited. A
// read-once extract, an unmatched wait, or an explicit condition matching a
// still-running agent must leave the watchers alone.
func TestRetireConsumedSupervisors_Gating(t *testing.T) {
	ctx := context.Background()
	cond := &domain.WatchCondition{}

	cases := []struct {
		name string
		base resolvedBase
		poll pollResult
		want []string
	}{
		{"read-once never retires",
			resolvedBase{terminalIDs: []string{"t1"}},
			pollResult{matched: true, finished: true}, nil},
		{"unmatched wait never retires",
			resolvedBase{terminalIDs: []string{"t1"}, wait: cond},
			pollResult{matched: false, finished: true}, nil},
		{"explicit condition on a running agent never retires",
			resolvedBase{terminalIDs: []string{"t1"}, wait: cond},
			pollResult{matched: true, finished: false}, nil},
		{"explicit condition with every target exited retires",
			resolvedBase{terminalIDs: []string{"t1", "t2"}, wait: cond},
			pollResult{matched: true, finished: true},
			[]string{"t1:finished", "t2:finished"}},
		{"coerced settle wait retires without an exit",
			resolvedBase{terminalIDs: []string{"t1"}, wait: cond, isSettleWait: true},
			pollResult{matched: true, finished: false},
			[]string{"t1:finished"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ret := &fakeRetirer{ret: 1}
			n := retireConsumedSupervisors(ctx, Deps{Supervisors: ret}, tc.base, tc.poll)
			if n != len(tc.want) {
				t.Fatalf("retired = %d, want %d", n, len(tc.want))
			}
			got := ret.snapshot()
			if len(got) != len(tc.want) {
				t.Fatalf("calls = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("calls = %v, want %v", got, tc.want)
				}
			}
		})
	}

	// nil retirer is a no-op, never a panic.
	if n := retireConsumedSupervisors(ctx, Deps{},
		resolvedBase{terminalIDs: []string{"t1"}, wait: cond, isSettleWait: true},
		pollResult{matched: true}); n != 0 {
		t.Fatalf("nil retirer must retire nothing, got %d", n)
	}
}

// An extract wait that consumes a NONZERO exit reports the consumption as FAILED, so
// the linked workflow ledger closes honestly instead of as done (gate-only handler,
// end to end through the poll's exit-code plumbing).
func TestExtractGate_NonzeroExitRetiresAsFailed(t *testing.T) {
	code := 1
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": {AgentState: "exited", ExitCode: &code, RecentOutput: strp("boom")},
	}}}
	ret := &fakeRetirer{ret: 1}
	tool := newExtractTool(Deps{Reader: reader, Router: &safeRouter{}, Supervisors: ret})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"wait":{"stateIs":"exited"},"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if !res.Ok {
		t.Fatalf("gate should succeed, got %+v", res.Error)
	}
	if calls := ret.snapshot(); len(calls) != 1 || calls[0] != "t1:failed" {
		t.Fatalf("a nonzero exit must retire as failed, got %v", calls)
	}
	if got, _ := res.Result.(map[string]any)["watchersRetired"].(int); got != 1 {
		t.Fatalf("watchersRetired = %v, want 1", res.Result.(map[string]any)["watchersRetired"])
	}
}

// failExtractRouter fails the extraction model call while keeping the judge sane.
type failExtractRouter struct{ safeRouter }

func (r *failExtractRouter) ExtractText(_ context.Context, _ string, _ []string, _ string) (string, bool, error) {
	return "", false, context.DeadlineExceeded
}

// An instruction-bearing extract whose WAIT matched but whose EXTRACTION failed must
// NOT retire — the completion never reached the model, and the watcher has to survive
// for the retry.
func TestExtract_FailedExtractionDoesNotRetire(t *testing.T) {
	code := 0
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{{
		"t1": {AgentState: "exited", ExitCode: &code, RecentOutput: strp("all done")},
	}}}
	ret := &fakeRetirer{ret: 1}
	tool := newExtractTool(Deps{Reader: reader, Router: &failExtractRouter{}, Supervisors: ret})
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"terminalIds":["t1"],"instruction":"the answer","wait":{"stateIs":"exited"},"maxAttempts":2,"pollIntervalMs":0}`), nil)
	if res.Ok {
		t.Fatalf("extraction failure must fail the tool, got %+v", res.Result)
	}
	if calls := ret.snapshot(); len(calls) != 0 {
		t.Fatalf("no retirement on a failed extraction, got %v", calls)
	}
}
