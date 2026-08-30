package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

func memStoreForTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:", nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A scheduled message the previous owner never finished is delivered again.
//
// This is the durability floor for the one queue item that is a user INSTRUCTION rather
// than a report. The notifier hands a burst to its callback and marks it delivered
// immediately after, and the reactors hold it only in memory — so a crash between those
// two moments loses the instruction outright, with its timer already marked fired and
// nothing left to retry it. "Run the migration in an hour" silently never happening is
// the worst failure this feature can have.
func TestBeginOwnershipRearmsAnUnfinishedScheduledMessage(t *testing.T) {
	s := memStoreForTest(t)
	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "run the tests", Summary: "Run npm test and report the result",
		Target:    &domain.EventTarget{TimerID: "tmr_1", TimerMessage: true, TimerOccurrence: 1},
		DedupeKey: "timer:tmr_1:fire:1",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Delivered by the owner that then died.
	if err := s.MarkNotified([]domain.QueueEvent{ev}, domain.NowMS()); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RearmedMessageCount != 1 {
		t.Fatalf("the unfinished message should be re-armed, got %d", sum.RearmedMessageCount)
	}
	// Re-armed means the notifier will hand it over again.
	fresh, err := s.ListEvents(domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	var found bool
	for _, e := range fresh {
		if e.ID == ev.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("a re-armed message must be deliverable again")
	}
}

// A message that was CARRIED OUT is resolved, and must not come back.
func TestBeginOwnershipLeavesAResolvedMessageAlone(t *testing.T) {
	s := memStoreForTest(t)
	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "done one", Summary: "already carried out",
		Target:    &domain.EventTarget{TimerID: "tmr_2", TimerMessage: true, TimerOccurrence: 1},
		DedupeKey: "timer:tmr_2:fire:1",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, err := s.ResolveEvent(ev.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RearmedMessageCount != 0 {
		t.Fatalf("a carried-out message must not be re-delivered, got %d", sum.RearmedMessageCount)
	}
}

// An occurrence the previous owner claimed but never published is rebuilt.
//
// fireTimer claims a timer before it publishes — it must, or an overrunning tick would
// fire the same row twice. A kill in that window advances the schedule and produces
// nothing: the timer reads as fired and there is no event anywhere to notice. For a
// message that is the user's instruction silently never happening, which no amount of
// re-arming can recover, because there is nothing to re-arm.
func TestBeginOwnershipRecoversAClaimedButUnpublishedMessage(t *testing.T) {
	s := memStoreForTest(t)
	// A timer that has "fired" once, with no event to show for it.
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_lost", Title: "run the tests", Status: "fired", RunCount: 1,
		FireAt:      domain.NowMS() - 10_000,        // due ten seconds ago
		LastFiredAt: ptrI64(domain.NowMS() - 5_000), // claimed, then a crash
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"Run npm test and report the result"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}

	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 1 {
		t.Fatalf("the lost occurrence should be rebuilt, got %d", sum.RecoveredMessageCount)
	}

	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	var found bool
	for _, e := range open {
		if e.Target != nil && e.Target.TimerID == "tmr_lost" && e.Target.TimerMessage {
			found = true
			if e.Summary != "Run npm test and report the result" {
				t.Errorf("the recovered event must carry the instruction verbatim, got %q", e.Summary)
			}
			if e.Target.TimerOccurrence != 1 {
				t.Errorf("occurrence should be 1, got %d", e.Target.TimerOccurrence)
			}
		}
	}
	if !found {
		t.Fatal("no recovered message event was published")
	}
}

// An occurrence that DID publish is not published twice — the dedupe key is the record
// of what landed, so recovery is a lookup rather than a guess.
func TestBeginOwnershipDoesNotDuplicateAPublishedOccurrence(t *testing.T) {
	s := memStoreForTest(t)
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_ok", Title: "ok", Status: "fired", RunCount: 1,
		FireAt:      domain.NowMS() - 10_000,
		LastFiredAt: ptrI64(domain.NowMS() - 5_000),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"already delivered"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	if _, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "ok", Summary: "already delivered",
		Target:    &domain.EventTarget{TimerID: "tmr_ok", TimerMessage: true, TimerOccurrence: 1},
		DedupeKey: "timer:tmr_ok:fire:1",
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a published occurrence must not be rebuilt, got %d", sum.RecoveredMessageCount)
	}
}

// A message that fired long ago is NOT resurrected.
//
// Two reasons, and either alone would be enough. The dedupe row is not permanent —
// retention GC deletes resolved events after a week, so an unbounded scan would read a
// tidied-away success as a loss and republish it, every cycle, for ever. And an
// instruction is tied to a moment: "run the migration in an hour", delivered three days
// late, is not a late delivery but the wrong action.
func TestBeginOwnershipDoesNotResurrectAStaleMessage(t *testing.T) {
	s := memStoreForTest(t)
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_old", Title: "old", Status: "fired", RunCount: 1,
		FireAt:      domain.NowMS() - 8*24*60*60*1000, // due eight days ago
		LastFiredAt: ptrI64(domain.NowMS() - 8*24*60*60*1000),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"deploy to production"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a long-past occurrence must not be republished, got %d", sum.RecoveredMessageCount)
	}
}

// An unresolved message that has gone stale is not re-armed.
//
// This path had no age bound at all: an instruction could sit through days of downtime
// as an open inbox row and then execute on the next boot, which is the same staleness
// the fire path and the recovery both refuse. It is left OPEN rather than resolved —
// it did not happen, so the user should still see it waiting rather than have it
// quietly marked done.
func TestBeginOwnershipDoesNotRearmAStaleUnresolvedMessage(t *testing.T) {
	old := domain.NowMS() - 8*24*60*60*1000
	s, err := Open(":memory:", &Options{Now: func() int64 { return old }})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "deploy", Summary: "deploy to production",
		Target: &domain.EventTarget{
			TimerID: "tmr_1", TimerMessage: true, TimerOccurrence: 1,
			TimerDueAt: old, // due eight days ago, and recorded
		},
		DedupeKey: "timer:tmr_1:fire:1",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.MarkNotified([]domain.QueueEvent{ev}, old); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	// Boot NOW — eight days after the message was written.
	s.now = domain.NowMS
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RearmedMessageCount != 0 {
		t.Fatalf("a week-old instruction must not be armed to run now, got %d", sum.RearmedMessageCount)
	}
	// ...and it must still be visible, not quietly closed.
	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var stillOpen bool
	for _, e := range open {
		if e.ID == ev.ID {
			stillOpen = true
		}
	}
	if !stillOpen {
		t.Fatal("a stale message must stay visible to the user, not be silently resolved")
	}
}

// A skipped occurrence must not be rebuilt by recovery.
//
// The fire path records a too-stale occurrence by publishing a MISSED note under the
// per-fire key. Recovery uses that key to decide whether a fire went missing — so if the
// skip were recorded anywhere else, the very next boot would see the occurrence as lost,
// rebuild the original instruction, and run the message the fire path had just decided
// was too stale to run.
func TestBeginOwnershipDoesNotRebuildASkippedOccurrence(t *testing.T) {
	s := memStoreForTest(t)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_skipped", Title: "deploy", Status: "fired", RunCount: 1,
		FireAt:      now - 30_000,
		LastFiredAt: ptrI64(now - 20_000),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"deploy to production"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	// The skip's own record, under the per-fire key.
	if _, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "deploy", Summary: "came due 3 hours ago ... has NOT been carried out",
		Target:    &domain.EventTarget{TimerID: "tmr_skipped"},
		DedupeKey: "timer:tmr_skipped:fire:1",
	}); err != nil {
		t.Fatalf("publish skip note: %v", err)
	}

	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a skipped occurrence must not be rebuilt, got %d", sum.RecoveredMessageCount)
	}
	// And nothing deliverable was created from it.
	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range open {
		if e.Target != nil && e.Target.TimerMessage && e.Target.TimerID == "tmr_skipped" {
			t.Fatal("the skipped instruction must not become a deliverable message")
		}
	}
}

// A row written before the due time was recorded is armed anyway.
//
// The compatibility rule has to mean the same thing everywhere: the delivery gate calls
// a missing due time FRESH, so a prefilter that quietly excluded those rows would strand
// exactly the instructions the gate was willing to run — the failure being prevented,
// arrived at from the other side.
func TestBeginOwnershipArmsALegacyMessageWithNoRecordedDueTime(t *testing.T) {
	s := memStoreForTest(t)
	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityAttention,
		Title: "legacy", Summary: "run the tests",
		Target:    &domain.EventTarget{TimerID: "tmr_legacy", TimerMessage: true, TimerOccurrence: 1},
		DedupeKey: "timer:tmr_legacy:fire:1",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := s.MarkNotified([]domain.QueueEvent{ev}, domain.NowMS()); err != nil {
		t.Fatalf("mark notified: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RearmedMessageCount != 1 {
		t.Fatalf("a legacy message with no recorded due time must still be armed, got %d", sum.RearmedMessageCount)
	}
}

// A CONTINUING repeat is never rebuilt, however recent it looks.
//
// Its fireAt was advanced relative to the CLAIM, so the original deadline is not
// recoverable from the row — and the fire path claims a stale occurrence before
// publishing its missed note, so a crash in that window leaves a row indistinguishable
// from a lost delivery. Rebuilding it would republish an instruction whose real deadline
// could be months ago, which is precisely what the freshness rule exists to prevent.
//
// The cost of skipping it is one occurrence of a job about to run again anyway. The cost
// of rebuilding it wrongly is executing an ancient instruction. Those are not close.
func TestBeginOwnershipDoesNotRebuildAContinuingRepeat(t *testing.T) {
	s := memStoreForTest(t)
	every := int64(24 * 60 * 60 * 1000)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_nightly", Title: "nightly", Status: "scheduled", RunCount: 3,
		FireAt:        now + every, // advanced relative to the claim
		RepeatEveryMs: &every,
		LastFiredAt:   ptrI64(now - 1_000),
		PayloadType:   "message",
		PayloadJson:   `{"type":"message","message":"run the nightly checks"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a continuing repeat must not be rebuilt, got %d", sum.RecoveredMessageCount)
	}
}

// A repeat that has REACHED its bound is recovered: it is terminal, so its fireAt still
// carries the due time of the occurrence that ended it, and that is the last chance the
// instruction will ever get.
func TestBeginOwnershipRecoversAFinalRepeatOccurrence(t *testing.T) {
	s := memStoreForTest(t)
	every := int64(24 * 60 * 60 * 1000)
	runs := 3
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_last", Title: "final", Status: "done", RunCount: 3,
		FireAt:        now - 30_000, // terminal rows never advanced it
		RepeatEveryMs: &every, MaxRuns: &runs,
		LastFiredAt: ptrI64(now - 20_000),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"the last nightly check"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 1 {
		t.Fatalf("a lost FINAL occurrence must be rebuilt, got %d", sum.RecoveredMessageCount)
	}
}

// A CANCELLED repeat must never be resurrected.
//
// Cancelling changes only `status` — the row keeps the advanced `fireAt` from its last
// fire, forever. An exclusion list that named only the continuing case let that through,
// and because the advanced value is in the FUTURE, a one-sided "is it too old" test read
// it as perfectly fresh. The result would be executing an instruction the user had
// explicitly stopped, which is worse than any delivery this recovery exists to save.
func TestBeginOwnershipNeverRebuildsACancelledRepeat(t *testing.T) {
	s := memStoreForTest(t)
	every := int64(24 * 60 * 60 * 1000)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_cancelled", Title: "nightly", Status: "cancelled", RunCount: 3,
		FireAt:        now + every, // advanced by its last fire, then cancelled
		RepeatEveryMs: &every,
		LastFiredAt:   ptrI64(now - 1_000),
		PayloadType:   "message",
		PayloadJson:   `{"type":"message","message":"deploy to production"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a cancelled repeat must never be rebuilt, got %d", sum.RecoveredMessageCount)
	}
	open, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, e := range open {
		if e.Target != nil && e.Target.TimerID == "tmr_cancelled" {
			t.Fatal("a cancelled instruction must not become a deliverable message")
		}
	}
}

// Nor an errored row, for the same reason: neither status is an outcome a CLAIM
// produces, so neither leaves fireAt describing the occurrence being recovered.
func TestBeginOwnershipOnlyRecoversClaimOutcomes(t *testing.T) {
	s := memStoreForTest(t)
	now := domain.NowMS()
	for _, status := range []string{"cancelled", "error", "scheduled"} {
		if _, err := s.InsertTimer(domain.TimerRecord{
			ID: "tmr_" + status, Title: status, Status: status, RunCount: 1,
			FireAt:      now - 30_000,
			LastFiredAt: ptrI64(now - 20_000),
			PayloadType: "message",
			PayloadJson: `{"type":"message","message":"do the thing"}`,
		}); err != nil {
			t.Fatalf("insert %s: %v", status, err)
		}
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("only fired/done rows may be rebuilt, got %d", sum.RecoveredMessageCount)
	}
}

// A backward clock adjustment must not destroy a real occurrence.
//
// Recovery runs once per ownership and the row is already terminal, so anything it
// skips is skipped for good — nothing retries when the clock catches up. An NTP
// correction, a reboot or a VM resume can leave a genuinely claimed occurrence sitting
// marginally in the future, and a strict future-rejection would discard the user's
// instruction permanently for a few seconds of clock drift.
func TestBeginOwnershipToleratesABackwardClockAdjustment(t *testing.T) {
	s := memStoreForTest(t)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_skew", Title: "run the tests", Status: "fired", RunCount: 1,
		FireAt:      now + 30_000, // the clock stepped back half a minute
		LastFiredAt: ptrI64(now),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"run the tests"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 1 {
		t.Fatalf("a real occurrence must survive a small clock rollback, got %d", sum.RecoveredMessageCount)
	}
}

// ...but the tolerance must not readmit the case it was carved out of. A stale fireAt
// sits a whole interval ahead, which is nothing like clock drift.
func TestBeginOwnershipToleranceDoesNotReadmitAStaleFutureDueTime(t *testing.T) {
	s := memStoreForTest(t)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_far", Title: "far", Status: "done", RunCount: 2,
		FireAt:      now + 24*60*60*1000, // a day ahead
		LastFiredAt: ptrI64(now),
		PayloadType: "message",
		PayloadJson: `{"type":"message","message":"deploy to production"}`,
	}); err != nil {
		t.Fatalf("insert timer: %v", err)
	}
	sum, err := s.BeginOwnership(domain.NowMS())
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatalf("a due time a day ahead is not clock drift, got %d", sum.RecoveredMessageCount)
	}
}

// The tolerance boundary, exactly.
//
// Named because an off-by-one here is invisible in normal use and only shows up as a
// real instruction lost to a clock step — and because the boundary is the one place a
// tolerance either works or does not.
func TestBeginOwnershipToleranceBoundaryIsExact(t *testing.T) {
	for name, tc := range map[string]struct {
		ahead     int64
		recovered int
	}{
		"exactly at the tolerance": {clockRollbackToleranceMs, 1},
		"one ms past it":           {clockRollbackToleranceMs + 1, 0},
	} {
		// A FROZEN clock. The boundary is the whole point of this test, so it must not
		// depend on how long the test itself takes: with the real clock, a slow run
		// advances s.now() past the captured `now` and flips the exactly-at case.
		now := domain.NowMS()
		s, err := Open(":memory:", &Options{Now: func() int64 { return now }})
		if err != nil {
			t.Fatalf("%s: open: %v", name, err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if _, err := s.InsertTimer(domain.TimerRecord{
			ID: "tmr_edge", Title: "edge", Status: "fired", RunCount: 1,
			FireAt:      now + tc.ahead,
			LastFiredAt: ptrI64(now),
			PayloadType: "message",
			PayloadJson: `{"type":"message","message":"run the tests"}`,
		}); err != nil {
			t.Fatalf("%s: insert: %v", name, err)
		}
		sum, err := s.BeginOwnership(now)
		if err != nil {
			t.Fatalf("%s: begin ownership: %v", name, err)
		}
		if sum.RecoveredMessageCount != tc.recovered {
			t.Errorf("%s: recovered %d, want %d", name, sum.RecoveredMessageCount, tc.recovered)
		}
	}
}

// A cancelled one-minute repeat sits INSIDE the clock tolerance, which is exactly why
// the tolerance must not be what protects against it. The status allow-list is.
func TestBeginOwnershipRejectsACancelledFastRepeatInsideTheTolerance(t *testing.T) {
	s := memStoreForTest(t)
	every := int64(60_000)
	now := domain.NowMS()
	if _, err := s.InsertTimer(domain.TimerRecord{
		ID: "tmr_fast", Title: "fast", Status: "cancelled", RunCount: 5,
		FireAt:        now + every, // one minute ahead: well within the tolerance
		RepeatEveryMs: &every,
		LastFiredAt:   ptrI64(now),
		PayloadType:   "message",
		PayloadJson:   `{"type":"message","message":"deploy to production"}`,
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	sum, err := s.BeginOwnership(now)
	if err != nil {
		t.Fatalf("begin ownership: %v", err)
	}
	if sum.RecoveredMessageCount != 0 {
		t.Fatal("a cancelled repeat must be refused by the status allow-list, not by the clock tolerance")
	}
}
