package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

func insertStarting(t *testing.T, s *Store) domain.AsyncInvocationRecord {
	t.Helper()
	rec, err := s.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "npm test", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Status != domain.AsyncStarting {
		t.Fatalf("status = %q, want starting", rec.Status)
	}
	return rec
}

// Activation is the one write that turns a send into supervised work, so the
// receipt and the running status must land together — a row that went live
// without its receipt would fall back to the legacy completion heuristic for
// the rest of its life.
func TestActivateAsyncSubmissionIsAtomicAndRoundTrips(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec := insertStarting(t, s)
	receipt := domain.SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "tok-1", Phase: "queued"}

	ok, err := s.ActivateAsyncSubmission(rec.ID, receipt, 2_000)
	if err != nil || !ok {
		t.Fatalf("activate: ok=%v err=%v", ok, err)
	}
	got, err := s.GetAsyncInvocation(rec.ID)
	if err != nil || got == nil {
		t.Fatalf("get: %v %v", got, err)
	}
	if got.Status != domain.AsyncRunning {
		t.Errorf("status = %q, want running", got.Status)
	}
	if got.StartedAt == nil || *got.StartedAt != 2_000 {
		t.Errorf("startedAt = %v, want 2000", got.StartedAt)
	}
	// The receipt rides runtime_state, not a column — the round trip back
	// through scanAsyncInvocation is what the coordinator actually reads.
	if got.Submission == nil || *got.Submission != receipt {
		t.Fatalf("submission = %+v, want %+v", got.Submission, receipt)
	}

	// Only a STARTING row activates: a second call cannot re-run the transition.
	if ok, err := s.ActivateAsyncSubmission(rec.ID, receipt, 3_000); err != nil || ok {
		t.Errorf("second activate: ok=%v err=%v, want false", ok, err)
	}
}

// Checkpointing preserves a decisive observation BEFORE the owner acts on it,
// so a crash between the read and the latch cannot lose it.
func TestCheckpointAsyncSubmissionPreservesDecisiveObservations(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec := insertStarting(t, s)
	queued := domain.SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "tok-1", Phase: "queued"}
	if ok, err := s.ActivateAsyncSubmission(rec.ID, queued, 2_000); err != nil || !ok {
		t.Fatalf("activate: ok=%v err=%v", ok, err)
	}

	// A checkpoint on a STARTING row is refused — activation owns that edge.
	other := insertStarting(t, s)
	if ok, err := s.CheckpointAsyncSubmission(other.ID, queued, 2_500); err != nil || ok {
		t.Errorf("checkpoint on a starting row: ok=%v err=%v, want false", ok, err)
	}

	written := queued
	written.Phase = "pty_written"
	written.ObservedAt = 3_000
	if ok, err := s.CheckpointAsyncSubmission(rec.ID, written, 3_000); err != nil || !ok {
		t.Fatalf("checkpoint: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetAsyncInvocation(rec.ID)
	if got == nil || got.Submission == nil || *got.Submission != written {
		t.Fatalf("submission = %+v, want %+v", got.Submission, written)
	}
	if got.Status != domain.AsyncRunning {
		t.Errorf("a checkpoint must not move the status: %q", got.Status)
	}

	// Cancellation wins the live claim, and losing it loses NOTHING: the last
	// decisive observation stays exactly where the previous checkpoint put it.
	if ok, err := s.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
		"status": string(domain.AsyncCancelled), "endedReason": "user_cancelled", "finishedAt": int64(4_000),
	}); err != nil || !ok {
		t.Fatalf("cancel: ok=%v err=%v", ok, err)
	}
	later := written
	later.Phase = "failed"
	later.ObservedAt = 5_000
	if ok, err := s.CheckpointAsyncSubmission(rec.ID, later, 5_000); err != nil || ok {
		t.Errorf("checkpoint after cancel: ok=%v err=%v, want false", ok, err)
	}
	got, _ = s.GetAsyncInvocation(rec.ID)
	if got == nil || got.Submission == nil || *got.Submission != written {
		t.Fatalf("cancelled row lost its receipt: %+v", got.Submission)
	}
}

// A receipt that would not survive its own Valid() gate must never reach the
// database — the reader has no way to tell a corrupt write from host truth.
func TestWriteAsyncSubmissionRejectsInvalidReceipts(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec := insertStarting(t, s)

	if ok, err := s.ActivateAsyncSubmission(rec.ID, domain.SubmissionReceipt{Version: 1, Acceptance: "tracked"}, 2_000); err == nil || ok {
		t.Fatalf("invalid receipt: ok=%v err=%v, want an error", ok, err)
	}
	got, _ := s.GetAsyncInvocation(rec.ID)
	if got == nil || got.Status != domain.AsyncStarting || got.Submission != nil {
		t.Fatalf("a rejected write must leave the row untouched: %+v", got)
	}
}

// A stored value the current build cannot decode is reported as "unreadable",
// not dropped: falling back to nil would silently re-enable the legacy
// completion heuristic on a row that was being supervised with a receipt.
func TestScanAsyncInvocationMarksCorruptReceiptsUnreadable(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec := insertStarting(t, s)

	for _, value := range []string{`{`, `{"version":2,"acceptance":"tracked"}`, `{"version":1,"acceptance":"tracked"}`} {
		if err := s.PutRuntimeState("async_submission:"+rec.ID, value); err != nil {
			t.Fatal(err)
		}
		got, err := s.GetAsyncInvocation(rec.ID)
		if err != nil || got == nil || got.Submission == nil {
			t.Fatalf("get for %q: %+v %v", value, got, err)
		}
		if got.Submission.Acceptance != "unreadable" || !got.Submission.Valid() {
			t.Fatalf("receipt for %q = %+v, want a valid unreadable record", value, got.Submission)
		}
	}
}

// The sidecar row's lifetime follows its invocation: retention GC must not
// leave orphaned receipts behind once the invocation itself is swept.
func TestGCRetentionSweepDropsOrphanedSubmissions(t *testing.T) {
	s := openAsyncTest(t, 1_000)
	rec := insertStarting(t, s)
	receipt := domain.SubmissionReceipt{Version: 1, Acceptance: "tracked", Token: "tok-1", Phase: "queued"}
	if ok, err := s.ActivateAsyncSubmission(rec.ID, receipt, 2_000); err != nil || !ok {
		t.Fatalf("activate: ok=%v err=%v", ok, err)
	}
	// An invocation that no longer exists (a prior sweep, a /clear) must not
	// keep its receipt alive.
	if _, err := s.db.Exec(`DELETE FROM async_invocations WHERE id = ?`, rec.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.GCRetentionSweep(10_000); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetRuntimeState("async_submission:" + rec.ID); err != nil || v != "" {
		t.Errorf("orphaned receipt survived the sweep: %q %v", v, err)
	}
}
