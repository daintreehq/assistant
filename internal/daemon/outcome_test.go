package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// A stored repeat interval that overflows must retire the timer, not resurrect it.
//
// Validation guards what is CREATED; rows already on disk were written under the old
// rules and no migration reaches a project that is offline. now+everyMs wrapping
// negative produces a fireAt every due check reads as permanently overdue, so a timer
// asking to run once in ten thousand years would instead run on every three-second
// tick for ever — the exact opposite of what it says.
func TestReschedulePatchRetiresAnOverflowingRepeat(t *testing.T) {
	every := int64(9223372036854775807)
	rec := domain.TimerRecord{ID: "tmr_1", RepeatEveryMs: &every}
	patch, terminal := rescheduleePatch(rec, domain.NowMS())
	if !terminal {
		t.Fatal("an overflowing repeat must be treated as terminal")
	}
	if patch["status"] != "done" {
		t.Fatalf("expected the row retired as done, got %v", patch["status"])
	}
	if _, resurrects := patch["fireAt"]; resurrects {
		t.Fatal("a retired row must not be given a next fire time")
	}
}

// A sane repeat still reschedules normally — the overflow guard must not retire
// everything that repeats.
func TestReschedulePatchStillRepeatsNormally(t *testing.T) {
	every := int64(3_600_000)
	rec := domain.TimerRecord{ID: "tmr_1", RepeatEveryMs: &every}
	now := domain.NowMS()
	patch, terminal := rescheduleePatch(rec, now)
	if terminal {
		t.Fatal("an ordinary repeat must keep going")
	}
	if patch["fireAt"] != now+every {
		t.Fatalf("next fire should be now+everyMs, got %v", patch["fireAt"])
	}
}

// A stored row that costs a model call and repeats faster than the scheduler ticks is
// retired at fire time.
//
// Schedule-time validation guards what is CREATED; this row was written under the older
// rules and no migration reaches a project that is offline. Left alone it dispatches on
// every three-second pass for the life of the project — the runaway the bounds exist to
// prevent, reached by the one route the bounds cannot see.
func TestReschedulePatchRetiresAnUnsafeMessageRepeat(t *testing.T) {
	fast, slow := int64(1), int64(3_600_000)
	runs := 5
	cases := map[string]domain.TimerRecord{
		"faster than the floor": {ID: "t1", RepeatEveryMs: &fast, PayloadType: "message", MaxRuns: &runs},
		"unbounded":             {ID: "t2", RepeatEveryMs: &slow, PayloadType: "message"},
	}
	for name, rec := range cases {
		patch, terminal := rescheduleePatch(rec, domain.NowMS())
		if !terminal || patch["status"] != "done" {
			t.Errorf("a %s message repeat must be retired, got terminal=%v patch=%v", name, terminal, patch)
		}
	}
	// A bounded, slow message repeat is exactly what the rules allow, and must run.
	rec := domain.TimerRecord{ID: "t3", RepeatEveryMs: &slow, PayloadType: "message", MaxRuns: &runs}
	if _, terminal := rescheduleePatch(rec, domain.NowMS()); terminal {
		t.Fatal("a bounded slow message repeat must keep running")
	}
}

// A reminder costs nothing per fire, so a fast one is harmless and must keep running —
// the guard is about spend, not about tidiness.
func TestReschedulePatchKeepsAFastReminder(t *testing.T) {
	every := int64(1)
	rec := domain.TimerRecord{ID: "tmr_1", RepeatEveryMs: &every, PayloadType: "enqueue"}
	if _, terminal := rescheduleePatch(rec, domain.NowMS()); terminal {
		t.Fatal("a fast enqueue repeat costs nothing and must not be retired")
	}
}

// An unbounded repeating TOOL CALL is long-standing behaviour with tests that encode it,
// and real schedules rely on it. Retiring those retroactively is a product decision
// about an existing feature, not one this feature gets to make on its way past.
func TestReschedulePatchKeepsAnUnboundedToolRepeat(t *testing.T) {
	every := int64(60_000)
	rec := domain.TimerRecord{ID: "tmr_1", RepeatEveryMs: &every, PayloadType: "call_safe_tool"}
	if _, terminal := rescheduleePatch(rec, domain.NowMS()); terminal {
		t.Fatal("a slow unbounded legacy repeat must keep running")
	}
}

// The freshness rule must apply on BOTH sides of the claim.
//
// Without it, an identical outage produced opposite outcomes purely by where the crash
// landed: a machine off for three days silently EXECUTED the instruction if it died
// before the claim (the row stayed scheduled and caught up), and silently DROPPED it if
// it died just after (recovery refuses a stale occurrence). Same situation, two
// answers, neither one the user could predict.
func TestFireTimerDropsALongOverdueMessage(t *testing.T) {
	threeDays := int64(3 * 24 * 60 * 60 * 1000)
	now := domain.NowMS()
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_stale", Title: "deploy", Status: "scheduled",
		FireAt:      now - threeDays,
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"deploy to production"}`,
	}}
	queue := newFakeQueue()
	reg := &fakeRegistry{result: domain.Ok("ok", nil)}
	s := newScheduler(store, queue, reg, nil)
	s.fireTimer(context.Background(), store.timers[0], now)

	// It must NOT be delivered as an instruction...
	for _, p := range queue.published {
		if p.Target != nil && p.Target.TimerMessage {
			t.Fatal("a long-overdue message must not be delivered as an instruction")
		}
	}
	// ...but the user must be told it was missed, rather than it vanishing.
	var told bool
	for _, p := range queue.published {
		if strings.Contains(p.Summary, "NOT been carried out") {
			told = true
		}
	}
	if !told {
		t.Fatal("a dropped message must be reported as missed, not silently discarded")
	}
}

// A message that is merely a little late is still delivered — the rule is about a stale
// world, not about punctuality.
func TestFireTimerStillDeliversASlightlyLateMessage(t *testing.T) {
	now := domain.NowMS()
	store := newFakeStore()
	store.timers = []domain.TimerRecord{{
		ID: "tmr_ok", Title: "tests", Status: "scheduled",
		FireAt:      now - 30_000, // half a minute late
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"run the tests"}`,
	}}
	queue := newFakeQueue()
	reg := &fakeRegistry{result: domain.Ok("ok", nil)}
	s := newScheduler(store, queue, reg, nil)
	s.fireTimer(context.Background(), store.timers[0], now)

	var delivered bool
	for _, p := range queue.published {
		if p.Target != nil && p.Target.TimerMessage && strings.Contains(p.Summary, "run the tests") {
			delivered = true
		}
	}
	if !delivered {
		t.Fatal("a slightly late message must still be delivered")
	}
}

// Skipping a stale occurrence must not cancel a standing schedule.
//
// A nightly message whose machine was off for one night has to run tomorrow. Retiring
// the row because a single delivery was missed destroys the whole instruction — a much
// larger loss than the stale delivery being avoided.
func TestFireTimerSkipsAStaleOccurrenceWithoutKillingTheRepeat(t *testing.T) {
	every := int64(24 * 60 * 60 * 1000)
	runs := 30
	now := domain.NowMS()
	store := newFakeStore()
	rec := domain.TimerRecord{
		ID: "tmr_nightly", Title: "nightly", Status: "scheduled",
		FireAt:        now - 3*60*60*1000, // three hours late: past the window
		RepeatEveryMs: &every, MaxRuns: &runs,
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"run the nightly checks"}`,
	}
	store.timers = []domain.TimerRecord{rec}
	queue := newFakeQueue()
	s := newScheduler(store, queue, &fakeRegistry{result: domain.Ok("ok", nil)}, nil)
	s.fireTimer(context.Background(), rec, now)

	patch := store.timerPatches["tmr_nightly"]
	if patch == nil {
		t.Fatal("the occurrence should have been claimed and skipped")
	}
	if patch["status"] != "scheduled" {
		t.Fatalf("a repeating message must stay scheduled after a skipped occurrence, got %v", patch["status"])
	}
	if patch["fireAt"] == nil {
		t.Fatal("the next occurrence must be scheduled")
	}
	// Its authority must survive too — the fires ahead of it still need it.
	if store.revoked["tmr_nightly"] != 0 {
		t.Fatalf("a continuing repeat must keep its grants, revoked %v", store.revoked)
	}
}
