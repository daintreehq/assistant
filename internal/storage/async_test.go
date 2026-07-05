package storage

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func openAsyncTest(t *testing.T, now int64) *Store {
	t.Helper()
	s, err := Open(":memory:", &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestAsyncInvocationCRUD(t *testing.T) {
	s := openAsyncTest(t, 1_000)

	rec, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "npm test", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.ID == "" || rec.ID[:4] != "asy_" {
		t.Errorf("id = %q, want asy_ prefix", rec.ID)
	}
	if rec.Status != domain.AsyncStarting {
		t.Errorf("default status = %q, want starting", rec.Status)
	}
	if rec.GroupID != rec.ID {
		t.Errorf("empty group must self-group: got %q", rec.GroupID)
	}

	got, err := s.GetAsyncInvocation(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Title != "npm test" || got.TerminalIdsJson != `["term-1"]` {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if missing, err := s.GetAsyncInvocation("asy_nope"); err != nil || missing != nil {
		t.Errorf("missing id should be (nil, nil), got %v %v", missing, err)
	}

	live, err := s.ListLiveAsyncInvocations()
	if err != nil || len(live) != 1 {
		t.Fatalf("live list = %d, %v; want 1", len(live), err)
	}
	if n, _ := s.CountLiveAsyncInvocations(); n != 1 {
		t.Errorf("live count = %d, want 1", n)
	}
}

func TestClaimLiveAsyncInvocationGuardsTerminalRows(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.await.async", Title: "wait", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, Status: domain.AsyncRunning, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	ok, err := s.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
		"status": string(domain.AsyncCancelled), "endedReason": "user_cancelled", "finishedAt": int64(2_000),
	})
	if err != nil || !ok {
		t.Fatalf("first claim: ok=%v err=%v", ok, err)
	}
	// A second claim must LOSE: the row is terminal now (the cancel/finalize race
	// discipline — whoever claims first wins).
	ok, err = s.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
		"status": string(domain.AsyncSucceeded), "finishedAt": int64(3_000),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("claim on a terminal row must fail")
	}
	got, _ := s.GetAsyncInvocation(rec.ID)
	if got.Status != domain.AsyncCancelled {
		t.Errorf("status = %q, want the first claim's cancelled", got.Status)
	}
	if n, _ := s.CountLiveAsyncInvocations(); n != 0 {
		t.Errorf("live count = %d, want 0", n)
	}
}

func TestCancelStaleAsyncInvocationsOnOpen(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	live, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "left running", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, Status: domain.AsyncRunning, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "already done", SessionID: "ses_1",
		TerminalIdsJson: `["term-2"]`, Status: domain.AsyncSucceeded, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The session boundary: every non-terminal row is abandoned; terminal rows
	// stay untouched (history).
	if err := s.cancelStaleAsyncInvocations(5_000); err != nil {
		t.Fatal(err)
	}
	gotLive, _ := s.GetAsyncInvocation(live.ID)
	if gotLive.Status != domain.AsyncAbandoned {
		t.Errorf("live row after boundary = %q, want abandoned", gotLive.Status)
	}
	if gotLive.EndedReason == nil || *gotLive.EndedReason != "session_ended" {
		t.Errorf("endedReason = %v, want session_ended", gotLive.EndedReason)
	}
	if gotLive.FinishedAt == nil || *gotLive.FinishedAt != 5_000 {
		t.Errorf("finishedAt = %v, want 5000", gotLive.FinishedAt)
	}
	gotDone, _ := s.GetAsyncInvocation(done.ID)
	if gotDone.Status != domain.AsyncSucceeded {
		t.Errorf("terminal row must be untouched, got %q", gotDone.Status)
	}
}

func TestAsyncRetentionSweepsOldTerminalRows(t *testing.T) {
	now := int64(1_000)
	s := openAsyncTest(t, now)
	finished := now - DefaultRetention.EventsTerminalAge.Milliseconds() - 1
	old, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "ancient", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, Status: domain.AsyncSucceeded,
		CreatedAt: finished - 10, ExpiresAt: finished, FinishedAt: &finished,
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "live", SessionID: "ses_1",
		TerminalIdsJson: `["term-2"]`, Status: domain.AsyncRunning, ExpiresAt: now + 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.GCRetentionSweep(now); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetAsyncInvocation(old.ID); got != nil {
		t.Error("aged-out terminal row should be swept")
	}
	if got, _ := s.GetAsyncInvocation(fresh.ID); got == nil {
		t.Error("live row must survive the sweep")
	}
}
