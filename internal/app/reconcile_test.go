package app

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/storage"
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

func TestReconcileLedgerCompletionSignalOnlyCommitsParseableRead(t *testing.T) {
	s := reconcileTestStore(t)
	if _, completed := reconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText()}, debuglog.Config{}); !completed {
		t.Fatal("parseable empty inventory must count as a completed reconcile attempt")
	}
	if _, completed := reconcileLedger(context.Background(), s, &fakeReconcileMCP{err: context.Canceled}, debuglog.Config{}); completed {
		t.Fatal("canceled terminal read must remain retryable")
	}
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

// --- adopted watchers -------------------------------------------------------

func insertWatcher(t *testing.T, s *storage.Store, id, kind, targets string) {
	t.Helper()
	if _, err := s.InsertWatcher(domain.WatcherRecord{
		ID: id, Kind: kind, Title: "fix: minor audit issues",
		TargetsJson: targets, Status: "active", CadenceMs: 3000, NextCheckAt: 1,
	}); err != nil {
		t.Fatalf("insert watcher: %v", err)
	}
}

func watcherStatus(t *testing.T, s *storage.Store, id string) (string, string) {
	t.Helper()
	w, err := s.GetWatcher(id)
	if err != nil || w == nil {
		t.Fatalf("get watcher %s: w=%v err=%v", id, w, err)
	}
	reason := ""
	if w.EndedReason != nil {
		reason = *w.EndedReason
	}
	return w.Status, reason
}

// The restart case. A watcher adopted from a previous owner whose terminal died with
// the app is retired here — BEFORE the scheduler can tick it, classify the absence as
// `terminal_exited` at SeverityUrgent, and spend an autonomous model turn reporting an
// exit the restart itself caused.
func TestReconcileRetiresWatcherWhoseTerminalIsGone(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_gone", "terminal", `["term-dead"]`)
	mcpC := &fakeReconcileMCP{result: terminalListText("term-other")}

	ReconcileLedger(context.Background(), s, mcpC, debuglog.Config{})

	status, reason := watcherStatus(t, s, "wch_gone")
	if status != "cancelled" || reason != storage.ReasonWatcherTerminalGoneOnResume {
		t.Fatalf("expected a retired watcher, got status=%q reason=%q", status, reason)
	}
}

// ...and its authority goes with it. A grant scoped to a watcher that no longer exists
// is standing unattended authority nobody can see.
func TestReconcileRevokesTheRetiredWatchersGrants(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_gone", "terminal", `["term-dead"]`)
	if _, err := s.InsertGrant(domain.AutomationGrantRecord{
		ActorType: "watcher", ActorID: "wch_gone",
		MaxUses: 1, UsesRemaining: 1, CreatedAt: 1,
		ExpiresAt: domain.NowMS() + 3_600_000,
	}); err != nil {
		t.Fatalf("insert grant: %v", err)
	}

	ReconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText()}, debuglog.Config{})

	live, err := s.ListGrants("wch_gone", domain.NowMS())
	if err != nil {
		t.Fatalf("list grants: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("a retired watcher must hold no live grants, got %d", len(live))
	}
}

// A watcher whose terminal is still there is untouched — that is a live supervision
// the restart had no business ending.
func TestReconcileKeepsWatcherWhoseTerminalSurvived(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_live", "terminal", `["term-alive"]`)

	ReconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText("term-alive")}, debuglog.Config{})

	if status, _ := watcherStatus(t, s, "wch_live"); status != "active" {
		t.Fatalf("a live watcher must survive the boot reconcile, got %q", status)
	}
}

// A pr_state watcher observes a forge, not a process. It has no terminal to lose and
// must survive any number of restarts.
func TestReconcileLeavesNonTerminalWatchersAlone(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_pr", "pr_state", `["owner/repo#7"]`)

	ReconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText()}, debuglog.Config{})

	if status, _ := watcherStatus(t, s, "wch_pr"); status != "active" {
		t.Fatalf("a pr watcher must survive the boot reconcile, got %q", status)
	}
}

// A failed terminal read is not evidence that every terminal is gone. It must not mass
// retire the supervision the user still has running.
func TestReconcileRetiresNothingOnAFailedRead(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_gone", "terminal", `["term-dead"]`)

	ReconcileLedger(context.Background(), s,
		&fakeReconcileMCP{err: context.DeadlineExceeded}, debuglog.Config{})

	if status, _ := watcherStatus(t, s, "wch_gone"); status != "active" {
		t.Fatalf("a failed read must change nothing, got %q", status)
	}
}

// A watcher carrying no targets is left alone: there is nothing to check, and an empty
// list is not the same claim as "its targets are gone".
func TestReconcileLeavesTargetlessWatchersAlone(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_bare", "terminal", `[]`)

	ReconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText()}, debuglog.Config{})

	if status, _ := watcherStatus(t, s, "wch_bare"); status != "active" {
		t.Fatalf("a targetless watcher must survive, got %q", status)
	}
}

// The half that decides whether the fix works at all.
//
// A watcher can publish an urgent `terminal_exited` and then die with its process
// before that event is ever delivered — the row outlives it, unresolved. Cancelling
// the watcher without resolving what it raised leaves the exact item the retire exists
// to prevent sitting in the queue, where the next tick digests it and spends the
// autonomous wake anyway.
func TestReconcileResolvesTheRetiredWatchersOwnEvents(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_gone", "terminal", `["term-dead"]`)
	if _, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source:    domain.SourceTerminalWatcher,
		Severity:  domain.SeverityUrgent,
		Title:     "fix: minor audit issues: terminal exited",
		Summary:   "Terminal exited.",
		Target:    &domain.EventTarget{TerminalID: "term-dead"},
		DedupeKey: "watcher:wch_gone:term-dead",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	ReconcileLedger(context.Background(), s, &fakeReconcileMCP{result: terminalListText()}, debuglog.Config{})

	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	for _, e := range open {
		if e.DedupeKey == "watcher:wch_gone:term-dead" {
			t.Fatal("the retired watcher's own event is still open — the wake it would " +
				"cause is exactly what retiring the watcher was meant to prevent")
		}
	}
}

// ...and only its own. A wholesale source-scoped resolve is /clear's semantics, and
// would silently clear supervision this reconcile has no quarrel with.
func TestReconcileLeavesOtherWatchersEventsAlone(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_gone", "terminal", `["term-dead"]`)
	insertWatcher(t, s, "wch_live", "terminal", `["term-alive"]`)
	for _, k := range []struct{ key, term string }{
		{"watcher:wch_gone:term-dead", "term-dead"},
		{"watcher:wch_live:term-alive", "term-alive"},
	} {
		if _, err := s.UpsertEvent(domain.QueuePublishArgs{
			Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
			Title: "waiting", Summary: "waiting on you",
			Target: &domain.EventTarget{TerminalID: k.term}, DedupeKey: k.key,
		}); err != nil {
			t.Fatalf("publish %s: %v", k.key, err)
		}
	}

	ReconcileLedger(context.Background(), s,
		&fakeReconcileMCP{result: terminalListText("term-alive")}, debuglog.Config{})

	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var kept bool
	for _, e := range open {
		if e.DedupeKey == "watcher:wch_gone:term-dead" {
			t.Fatal("the dead watcher's event should have been resolved")
		}
		if e.DedupeKey == "watcher:wch_live:term-alive" {
			kept = true
		}
	}
	if !kept {
		t.Fatal("a live watcher's open event must survive the boot reconcile")
	}
}

// The scheduler in this same process can be mid-check on the very row being retired,
// about to finalize it with a real verdict. Losing that race must mean leaving its
// answer alone, not overwriting a genuine outcome with `cancelled`.
func TestRetireWatcherOnResumeYieldsToASettledWatcher(t *testing.T) {
	s := reconcileTestStore(t)
	insertWatcher(t, s, "wch_done", "terminal", `["term-dead"]`)
	if err := s.UpdateWatcher("wch_done", map[string]any{"status": "condition_met"}); err != nil {
		t.Fatalf("settle watcher: %v", err)
	}

	won, err := s.RetireWatcherOnResume("wch_done", domain.NowMS())
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if won {
		t.Fatal("retiring an already-settled watcher must not win")
	}
	if status, _ := watcherStatus(t, s, "wch_done"); status != "condition_met" {
		t.Fatalf("the settled verdict must survive, got %q", status)
	}
}
