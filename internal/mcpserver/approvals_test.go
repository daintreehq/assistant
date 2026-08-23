package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/redact"
)

// approvals_test.go pins the property that makes delegation safe to offer at all: a parked
// dispatch is ALWAYS bounded. Every exit — decision, timeout, cancellation, teardown —
// releases it, because a confirm hook that could block forever would wedge the turn and,
// through it, the session.

func TestApprovalDeclineRefusesWithoutParking(t *testing.T) {
	a := NewApprovals(ApprovalDecline, 0)
	done := make(chan bool, 1)
	go func() { done <- a.Confirm(context.Background(), ApprovalRequest{Tool: "git.push"}) }()

	select {
	case ok := <-done:
		if ok {
			t.Error("decline mode must refuse")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("decline mode must not park the dispatch — the turn would stall with nobody watching")
	}
	if len(a.Pending()) != 0 {
		t.Error("decline mode must not create a pending approval")
	}
}

func TestApprovalAutoAllowsWithoutParking(t *testing.T) {
	a := NewApprovals(ApprovalAuto, 0)
	done := make(chan bool, 1)
	go func() { done <- a.Confirm(context.Background(), ApprovalRequest{Tool: "git.push"}) }()
	select {
	case ok := <-done:
		if !ok {
			t.Error("auto mode must allow")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto mode must not park")
	}
}

func TestApprovalDelegateParksThenReleasesOnDecision(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 0)
	done := make(chan bool, 1)
	go func() {
		done <- a.Confirm(context.Background(), ApprovalRequest{
			Tool: "git.push", Risk: domain.RiskGit,
			Consequence: "pushes 3 commits to origin/main",
			RawArgs:     `{"remote":"origin"}`,
		})
	}()

	pa := waitForPending(t, a, 1)[0]
	if pa.Tool != "git.push" || pa.Risk != string(domain.RiskGit) {
		t.Errorf("pending = %+v", pa)
	}
	// The caller cannot judge "pushes to origin" without seeing the args.
	if !strings.Contains(pa.Args, "origin") {
		t.Errorf("args preview = %q, want it to carry the arguments", pa.Args)
	}
	if pa.Consequence == "" {
		t.Error("the consequence is the whole basis for a decision; it must be carried")
	}

	if !a.Resolve(pa.ID, DecisionApproved) {
		t.Fatal("Resolve reported nothing pending")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Error("an approved call must proceed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the dispatch was not released by the decision")
	}
	if len(a.Pending()) != 0 {
		t.Error("a decided approval must leave the pending list")
	}
}

// TestApprovalArgsAreRedacted: the preview crosses a process boundary into another
// agent's context. Raw tool args routinely carry tokens.
func TestApprovalArgsAreRedacted(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	a := NewApprovals(ApprovalDelegate, 0)
	go a.Confirm(context.Background(), ApprovalRequest{
		Tool:    "terminal.run",
		RawArgs: `{"cmd":"curl -H 'Authorization: Bearer sk-or-v1-fake-test-secret-value'"}`,
	})
	pa := waitForPending(t, a, 1)[0]
	if strings.Contains(pa.Args, "sk-or-v1-fake-test-secret-value") {
		t.Errorf("the args preview leaked a credential: %q", pa.Args)
	}
	a.RejectAll()
}

// TestUnansweredApprovalIsDeniedOnTheTimer: failing OPEN on silence would make waiting a
// way to get anything approved.
func TestUnansweredApprovalIsDeniedOnTheTimer(t *testing.T) {
	// 300ms, not 50: under -race on a loaded machine a 50ms timer can fire before the
	// polling goroutine observes the pending state, turning a real assertion into a
	// three-second flake.
	a := NewApprovals(ApprovalDelegate, 300*time.Millisecond)
	done := make(chan bool, 1)
	go func() { done <- a.Confirm(context.Background(), ApprovalRequest{Tool: "terminal.run"}) }()

	select {
	case ok := <-done:
		if ok {
			t.Fatal("an unanswered approval must be DENIED, not allowed")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the timer never fired; a forgotten approval would pin the turn forever")
	}
	if len(a.Pending()) != 0 {
		t.Error("a timed-out approval must leave the pending list")
	}
}

// TestCancellationUnparksTheDispatch: teardown waits for the turn goroutine, so an
// approval that survived cancellation would deadlock session close.
func TestCancellationUnparksTheDispatch(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 0)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- a.Confirm(ctx, ApprovalRequest{Tool: "terminal.run"}) }()
	waitForPending(t, a, 1)

	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Error("a cancelled approval must not allow the call")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not release the dispatch; session close would deadlock")
	}
}

// TestRejectAllReleasesEveryParkedDispatch is what interrupt and close rely on.
func TestRejectAllReleasesEveryParkedDispatch(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 0)
	var wg sync.WaitGroup
	results := make(chan bool, 3)
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- a.Confirm(context.Background(), ApprovalRequest{Tool: "terminal.run"})
		}()
	}
	waitForPending(t, a, 3)

	a.RejectAll()
	waited := make(chan struct{})
	go func() { wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(3 * time.Second):
		t.Fatal("RejectAll left a dispatch parked")
	}
	close(results)
	for ok := range results {
		if ok {
			t.Error("RejectAll must deny, never allow")
		}
	}
}

// TestSettledOutcomeIsRemembered: a caller that answers just after the timer fired must
// learn WHY its approval vanished, or "not found" reads as a bug in its own bookkeeping.
func TestSettledOutcomeIsRemembered(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 300*time.Millisecond)
	go a.Confirm(context.Background(), ApprovalRequest{Tool: "terminal.run"})
	pa := waitForPending(t, a, 1)[0]

	deadline := time.Now().Add(3 * time.Second)
	for len(a.Pending()) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got, ok := a.Outcome(pa.ID)
	if !ok {
		t.Fatal("the outcome of a settled approval must be recoverable")
	}
	if got != DecisionTimeout {
		t.Errorf("outcome = %q, want timeout", got)
	}
	// And resolving it again must report "nothing pending" rather than pretend it worked.
	if a.Resolve(pa.ID, DecisionApproved) {
		t.Error("resolving an already-settled approval must report false")
	}
}

// TestUnknownApprovalModeFallsBackToTheSafeOne.
func TestUnknownApprovalModeFallsBackToTheSafeOne(t *testing.T) {
	if got := NewApprovals(ApprovalMode("yolo"), 0).Mode(); got != ApprovalDecline {
		t.Errorf("mode = %q, want the safe default", got)
	}
}

// waitForPending blocks until n approvals are parked, or fails.
func waitForPending(t *testing.T, a *Approvals, n int) []PendingApproval {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if p := a.Pending(); len(p) >= n {
			return p
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d pending approval(s); got %d", n, len(a.Pending()))
	return nil
}

// TestApprovalTimeoutCannotBeDisabled: the timer is the ONLY bound on a parked dispatch
// when nobody answers, so no caller-supplied value may switch it off or stretch it past
// the point of being a bound.
func TestApprovalTimeoutCannotBeDisabled(t *testing.T) {
	for name, given := range map[string]time.Duration{
		"zero":           0,
		"negative":       -1,
		"absurdly large": 1000 * time.Hour,
		"minimum int64":  time.Duration(-1 << 62),
	} {
		t.Run(name, func(t *testing.T) {
			got := NewApprovals(ApprovalDelegate, given).Timeout()
			if got <= 0 {
				t.Fatalf("timeout = %v — an unanswered approval would park forever", got)
			}
			if got > MaxApprovalTimeout {
				t.Errorf("timeout = %v, want it clamped to at most %v", got, MaxApprovalTimeout)
			}
		})
	}
}

// TestCancellationDominatesADecision pins the rule directly. The race it guards — a
// decision and a cancellation both ready on Confirm's select — cannot be scheduled on
// demand: normally the cancellation wakes Confirm before the decision is even sent, so
// an end-to-end test would pass whether or not the rule existed. Asserting the rule
// itself is what actually bites.
func TestCancellationDominatesADecision(t *testing.T) {
	cases := []struct {
		decision  Decision
		cancelled bool
		want      bool
	}{
		{DecisionApproved, false, true},
		// The one that matters: approved, but the turn was cancelled underneath it.
		{DecisionApproved, true, false},
		{DecisionRejected, false, false},
		{DecisionRejected, true, false},
		{DecisionTimeout, false, false},
		{DecisionCancelled, false, false},
	}
	for _, tc := range cases {
		if got := settleDecision(tc.decision, tc.cancelled); got != tc.want {
			t.Errorf("settleDecision(%q, cancelled=%v) = %v, want %v",
				tc.decision, tc.cancelled, got, tc.want)
		}
	}
}

// TestApprovalAfterCancellationIsDenied is the end-to-end companion: whatever the
// scheduling, a call approved after the turn was cancelled must not run.
func TestApprovalAfterCancellationIsDenied(t *testing.T) {
	for i := 0; i < 50; i++ {
		a := NewApprovals(ApprovalDelegate, 0)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan bool, 1)
		go func() { done <- a.Confirm(ctx, ApprovalRequest{Tool: "git.push"}) }()
		pa := waitForPending(t, a, 1)[0]

		cancel()
		a.Resolve(pa.ID, DecisionApproved)

		select {
		case allowed := <-done:
			if allowed {
				t.Fatalf("iteration %d: a mutating call was ALLOWED after the turn was cancelled", i)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: the dispatch was never released", i)
		}
	}
}

// TestRejectRunLeavesOtherRunsAlone: a session is single-flight, but the run interrupt
// captured can finish and a new one start before the rejection lands.
func TestRejectRunLeavesOtherRunsAlone(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 0)
	mine := make(chan bool, 1)
	theirs := make(chan bool, 1)
	go func() { mine <- a.Confirm(context.Background(), ApprovalRequest{Tool: "git.push", RunID: "mrun_a"}) }()
	go func() {
		theirs <- a.Confirm(context.Background(), ApprovalRequest{Tool: "terminal.run", RunID: "mrun_b"})
	}()
	waitForPending(t, a, 2)

	a.RejectRun("mrun_a")
	select {
	case ok := <-mine:
		if ok {
			t.Error("the rejected run's approval must be denied")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RejectRun left its own approval parked")
	}
	select {
	case <-theirs:
		t.Error("RejectRun cancelled another run's approval — that aborts work nobody asked to stop")
	case <-time.After(150 * time.Millisecond):
		// Correct: still parked.
	}
	if len(a.Pending()) != 1 {
		t.Errorf("pending = %d, want the other run's approval still there", len(a.Pending()))
	}
	a.RejectAll()
}

// A decision written while watching one turn can arrive after that turn ended. For
// inject and interrupt the cost is steering or cancelling the wrong work; for an approval
// it is RELEASING A MUTATING CALL on the strength of a judgement made about something
// else. Correlation and settlement are one operation so there is no window between them.
func TestApprovalRunCorrelationRejectsAStaleDecision(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 2*time.Second)
	done := make(chan bool, 1)
	go func() {
		done <- a.Confirm(context.Background(), ApprovalRequest{
			Tool: "git.push", Risk: domain.RiskGit, RunID: "mrun_live",
		})
	}()
	id := awaitOnePending(t, a).ID

	// A decision aimed at a turn that is not the one this approval blocks. It must not
	// settle, and must say what this approval is actually waiting on.
	settled, mismatch := a.ResolveForRun(id, "mrun_previous", DecisionApproved)
	if settled {
		t.Fatal("a decision naming the wrong run settled the approval")
	}
	var mm *ApprovalRunMismatchError
	if !errors.As(mismatch, &mm) {
		t.Fatalf("wanted an ApprovalRunMismatchError, got %v", mismatch)
	}
	if mm.Actual != "mrun_live" {
		t.Errorf("the mismatch named %q as the blocked run, want mrun_live", mm.Actual)
	}
	// Still parked: a refused correlation must not have settled it either way.
	if len(a.Pending()) != 1 {
		t.Fatal("a mismatched decision removed the approval anyway")
	}

	// An omitted runId skips correlation, and the right one passes it.
	if _, err := a.ResolveForRun(id, "mrun_live", DecisionRejected); err != nil {
		t.Errorf("the correct run was rejected: %v", err)
	}
	if <-done {
		t.Error("a rejected approval let the dispatch through")
	}
}

// A pending approval with no recorded run FAILS a correlation rather than passing it. The
// caller asked for a check; answering "sure" when the provenance is simply missing is the
// fail-open answer.
func TestApprovalCorrelationFailsClosedWhenTheRunIsUnknown(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, 2*time.Second)
	done := make(chan bool, 1)
	go func() {
		done <- a.Confirm(context.Background(), ApprovalRequest{Tool: "git.push", Risk: domain.RiskGit})
	}()
	id := awaitOnePending(t, a).ID

	settled, mismatch := a.ResolveForRun(id, "mrun_anything", DecisionApproved)
	if settled || mismatch == nil {
		t.Fatalf("an approval with no recorded run passed correlation (settled=%v, err=%v)", settled, mismatch)
	}
	// Dropping the runId still works — the caller is then making the claim itself.
	if ok, err := a.ResolveForRun(id, "", DecisionRejected); !ok || err != nil {
		t.Errorf("an uncorrelated decision was refused: ok=%v err=%v", ok, err)
	}
	<-done
}

// An approval that is no longer pending has nothing to correlate: the caller's own
// not-found handling (already settled versus never real) is more informative than a
// correlation error about a ghost.
func TestApprovalCorrelationIsSilentOnAnUnknownApproval(t *testing.T) {
	a := NewApprovals(ApprovalDelegate, time.Second)
	settled, mismatch := a.ResolveForRun("apr_gone", "mrun_x", DecisionApproved)
	if settled {
		t.Error("resolving an unknown approval reported success")
	}
	if mismatch != nil {
		t.Errorf("resolving an unknown approval produced a correlation error: %v", mismatch)
	}
}

// awaitOnePending waits for exactly one parked approval and returns it.
func awaitOnePending(t *testing.T, a *Approvals) PendingApproval {
	t.Helper()
	for i := 0; i < 200; i++ {
		if p := a.Pending(); len(p) == 1 {
			return p[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no approval parked")
	return PendingApproval{}
}
