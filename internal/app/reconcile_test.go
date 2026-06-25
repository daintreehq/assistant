package app

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/storage"
)

// fakeReconcileMCP is a scripted terminal.list source for the boot-reconcile tests.
type fakeReconcileMCP struct {
	result mcp.CallResult
	err    error
	calls  int
}

func (f *fakeReconcileMCP) CallTool(_ context.Context, _ string, _ map[string]any, _ mcp.CallOptions) (mcp.CallResult, error) {
	f.calls++
	return f.result, f.err
}

// terminalListText builds a terminal.list result with the given ids in its JSON text
// body (Daintree returns results in text).
func terminalListText(ids ...string) mcp.CallResult {
	body := `{"terminals":[`
	for i, id := range ids {
		if i > 0 {
			body += ","
		}
		body += `{"id":"` + id + `"}`
	}
	body += `]}`
	return mcp.CallResult{Text: body}
}

func reconcileTestStore(t *testing.T) *storage.Store {
	t.Helper()
	s, err := storage.Open(":memory:", &storage.Options{Now: func() int64 { return 1000 }})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustRunStatus(t *testing.T, s *storage.Store, id string) domain.WorkflowRunStatus {
	t.Helper()
	r, err := s.GetWorkflowRun(id)
	if err != nil || r == nil {
		t.Fatalf("get run %s: r=%v err=%v", id, r, err)
	}
	return r.Status
}

// TestReconcileLedgerCancelsStaleRun — an open run whose only terminal is gone from
// the live inventory is cancelled.
func TestReconcileLedgerCancelsStaleRun(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_gone"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live")}
	n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{})
	if n != 1 {
		t.Fatalf("want 1 cancellation, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowCancelled {
		t.Fatalf("run status want cancelled, got %s", got)
	}
}

// TestReconcileLedgerKeepsLiveRun — a run whose terminal is still live is untouched.
func TestReconcileLedgerKeepsLiveRun(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_live"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live", "term_other")}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("live run should stay active, got %s", got)
	}
}

// TestReconcileLedgerSkipsRunWithoutTerminals — a pending run that never bound a
// terminal (pre-spawn) is left alone.
func TestReconcileLedgerSkipsRunWithoutTerminals(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowPending})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText()}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowPending {
		t.Fatalf("terminal-less run should stay pending, got %s", got)
	}
}

// TestReconcileLedgerMCPErrorSkips — a failed terminal.list read must NOT mass-cancel.
func TestReconcileLedgerMCPErrorSkips(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_gone"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{err: context.Canceled}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations on read error, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("run must stay active on read error, got %s", got)
	}
}

// TestReconcileLedgerIsErrorSkips — an IsError result is treated like a failed read.
func TestReconcileLedgerIsErrorSkips(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_gone"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: mcp.CallResult{IsError: true, Text: `{"terminals":[]}`}}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations on IsError, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("run must stay active on IsError, got %s", got)
	}
}

// TestReconcileLedgerCancelsViaAgentLaunch — a confirmed launch whose terminal vanished
// cancels its backing ledger run (the boot-reconcile seam's first caller). The run
// itself binds no terminal, so only pass 2 can cancel it.
func TestReconcileLedgerCancelsViaAgentLaunch(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k1", AgentID: "claude", Mode: "edit", Title: "T", Name: "N",
		Stage: domain.LaunchConfirmed, TerminalID: strPtr("term_gone"),
		WorkflowRunID: strPtr(run.ID),
	}); err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live")}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 1 {
		t.Fatalf("want 1 cancellation via launch, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowCancelled {
		t.Fatalf("backing run want cancelled, got %s", got)
	}
}

// TestReconcileLedgerKeepsLiveAgentLaunch — a confirmed launch whose terminal is still
// live is a running-but-unsupervised orphan: its backing run is left active (the
// fresh-start invariant forbids re-arming watchers).
func TestReconcileLedgerKeepsLiveAgentLaunch(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k2", AgentID: "claude", Mode: "edit", Title: "T", Name: "N",
		Stage: domain.LaunchConfirmed, TerminalID: strPtr("term_live"),
		WorkflowRunID: strPtr(run.ID),
	}); err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live")}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations for a live orphan, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("live orphan's run should stay active, got %s", got)
	}
}

// TestReconcileLedgerMixedTerminalsKeepsRun — a run binding both a dead and a live
// terminal is still live (anyLive), so it is NOT cancelled.
func TestReconcileLedgerMixedTerminalsKeepsRun(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_dead","term_live"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live")}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations (one terminal still live), got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("partially-live run should stay active, got %s", got)
	}
}

// TestReconcileLedgerMalformedListSkips — a successful (non-error) terminal.list whose
// body has no parseable terminals array must NOT be read as "every terminal gone".
func TestReconcileLedgerMalformedListSkips(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_gone"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: mcp.CallResult{Text: "temporarily unavailable"}}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 0 {
		t.Fatalf("want 0 cancellations on an unparseable success, got %d", n)
	}
	if got := mustRunStatus(t, s, run.ID); got != domain.WorkflowActive {
		t.Fatalf("run must stay active on an unparseable read, got %s", got)
	}
}

// TestReconcileLedgerCancelStampsCompletedAt — a reconcile-cancelled run carries a
// non-NULL completedAt, matching the other cancellation paths.
func TestReconcileLedgerCancelStampsCompletedAt(t *testing.T) {
	s := reconcileTestStore(t)
	run, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status: domain.WorkflowActive, TerminalIdsJson: strPtr(`["term_gone"]`),
	})
	if err != nil {
		t.Fatal(err)
	}
	mcpC := &fakeReconcileMCP{result: terminalListText("term_live")}
	if n := ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{}); n != 1 {
		t.Fatalf("want 1 cancellation, got %d", n)
	}
	got, err := s.GetWorkflowRun(run.ID)
	if err != nil || got == nil {
		t.Fatalf("get run: %v", err)
	}
	if got.CompletedAt == nil {
		t.Fatal("reconcile-cancelled run must stamp completedAt, got nil")
	}
}

// TestParseTerminalIDsUnionsSources — ids are collected from both the structured
// payload and the text body, preferring "id" then "terminalId"; sawArray is true.
func TestParseTerminalIDsUnionsSources(t *testing.T) {
	structured := map[string]any{"terminals": []any{
		map[string]any{"id": "a"},
		map[string]any{"terminalId": "b"},
	}}
	got, saw := parseTerminalIDs(structured, `{"terminals":[{"id":"c"}]}`)
	if !saw {
		t.Error("expected sawArray=true for a parseable result")
	}
	for _, want := range []string{"a", "b", "c"} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing id %q in %v", want, got)
		}
	}
	if len(got) != 3 {
		t.Errorf("want 3 ids, got %d (%v)", len(got), got)
	}
}

// TestParseTerminalIDsEmptyArrayIsSeen — an explicit empty terminals array is a valid
// (seen) inventory: zero ids but sawArray=true.
func TestParseTerminalIDsEmptyArrayIsSeen(t *testing.T) {
	got, saw := parseTerminalIDs(nil, `{"terminals":[]}`)
	if !saw {
		t.Error("explicit empty terminals array should be seen (sawArray=true)")
	}
	if len(got) != 0 {
		t.Errorf("want 0 ids, got %d", len(got))
	}
}

// TestParseTerminalIDsUnparseableNotSeen — a non-JSON / missing-key body is NOT a seen
// inventory, so the caller skips reconciliation.
func TestParseTerminalIDsUnparseableNotSeen(t *testing.T) {
	if _, saw := parseTerminalIDs(nil, "temporarily unavailable"); saw {
		t.Error("plaintext body should not be seen as a terminals array")
	}
	if _, saw := parseTerminalIDs(nil, `{"other":[]}`); saw {
		t.Error("JSON without a terminals key should not be seen as a terminals array")
	}
}
