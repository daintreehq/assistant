package daemon

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// deliveredMessage returns the one scheduled-message event a fire published.
func deliveredMessage(t *testing.T, q *fakeQueue) domain.QueuePublishArgs {
	t.Helper()
	var out []domain.QueuePublishArgs
	for _, p := range q.published {
		if p.Target != nil && p.Target.TimerMessage {
			out = append(out, p)
		}
	}
	if len(out) != 1 {
		t.Fatalf("want exactly one delivered message, got %d (%+v)", len(out), q.published)
	}
	return out[0]
}

// A check-in tick carries where its loop stands, so the turn it starts can end or pace
// the loop honestly: cadence, cap, deadline, the linked ledger row, and whether this
// occurrence is the LAST (the claim's own terminal verdict).
func TestFireTimerStampsTheLoopPositionOnAMessage(t *testing.T) {
	now := domain.NowMS()
	every := int64(10 * 60 * 1000)
	maxRuns := 30
	until := now + 6*60*60*1000
	target := `{"workflowRunId":"wfr_queue"}`
	rec := domain.TimerRecord{
		ID: "tmr_loop", Title: "queue check", Status: "scheduled", FireAt: now - 1_000,
		PayloadType: "message", PayloadJson: `{"type":"message","message":"check the queue"}`,
		RepeatEveryMs: &every, MaxRuns: &maxRuns, RepeatUntil: &until, RunCount: 4, TargetJson: &target,
	}

	store := newFakeStore()
	store.timers = []domain.TimerRecord{rec}
	q := newFakeQueue()
	newScheduler(store, q, &fakeRegistry{result: domain.Ok("ok", nil)}, nil).fireTimer(context.Background(), rec, now)
	got := deliveredMessage(t, q).Target
	if got.TimerOccurrence != 5 || got.TimerEveryMs != every || got.TimerMaxRuns != maxRuns ||
		got.TimerRepeatUntil != until || got.WorkflowRunID != "wfr_queue" || got.TimerFinal {
		t.Fatalf("mid-loop tick target = %+v", got)
	}

	// The occurrence that exhausts maxRuns is final.
	last := rec
	last.RunCount = maxRuns - 1
	store = newFakeStore()
	store.timers = []domain.TimerRecord{last}
	q = newFakeQueue()
	newScheduler(store, q, &fakeRegistry{result: domain.Ok("ok", nil)}, nil).fireTimer(context.Background(), last, now)
	if got := deliveredMessage(t, q).Target; !got.TimerFinal || got.TimerOccurrence != maxRuns {
		t.Fatalf("last tick must be final, got %+v", got)
	}

	// A one-shot message is its own last occurrence.
	one := domain.TimerRecord{
		ID: "tmr_once", Title: "later", Status: "scheduled", FireAt: now - 1_000,
		PayloadType: "message", PayloadJson: `{"type":"message","message":"run the tests"}`,
	}
	store = newFakeStore()
	store.timers = []domain.TimerRecord{one}
	q = newFakeQueue()
	newScheduler(store, q, &fakeRegistry{result: domain.Ok("ok", nil)}, nil).fireTimer(context.Background(), one, now)
	if got := deliveredMessage(t, q).Target; !got.TimerFinal || got.TimerEveryMs != 0 || got.TimerMaxRuns != 0 {
		t.Fatalf("one-shot target = %+v", got)
	}
}

// failureTarget strips the loop metadata along with the marker: a failure published
// through a message's target is a report, never an instruction.
func TestFailureTargetClearsTheLoopPosition(t *testing.T) {
	in := &domain.EventTarget{TimerID: "tmr_1", TimerMessage: true, TimerOccurrence: 3,
		TimerEveryMs: 60_000, TimerMaxRuns: 5, TimerRepeatUntil: 9, TimerFinal: true}
	out := failureTarget(in)
	if out.TimerMessage || out.TimerOccurrence != 0 || out.TimerEveryMs != 0 || out.TimerMaxRuns != 0 ||
		out.TimerRepeatUntil != 0 || out.TimerFinal {
		t.Fatalf("failureTarget left provenance behind: %+v", out)
	}
	if out.TimerID != "tmr_1" || !in.TimerMessage {
		t.Fatalf("failureTarget must keep the timer link and not mutate its input: out=%+v in=%+v", out, in)
	}
}
