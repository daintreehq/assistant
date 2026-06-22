package storage

import (
	"math"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestUpsertEventNonPositiveTTLStaysVisible locks the ttlMs guard: a 0/negative ttlMs must
// be treated as "no expiry" (the event stays visible), never set expiresAt <= now (which
// would make it instantly invisible to ListEvents); a huge ttlMs must not overflow int64
// into a past timestamp.
func TestUpsertEventNonPositiveTTLStaysVisible(t *testing.T) {
	s := openTest(t, 1000)
	neg := int64(-100)
	zero := int64(0)
	huge := int64(math.MaxInt64)
	for _, ttl := range []*int64{&neg, &zero, &huge} {
		ev, err := s.UpsertEvent(domain.QueuePublishArgs{
			Source: domain.SourceSystem, Severity: domain.SeverityInfo,
			Title: "t", Summary: "s", TTLMs: ttl,
		})
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ListEvents(domain.QueueDigestOptions{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, e := range got {
			if e.ID == ev.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("event with ttlMs=%d must remain visible (treated as no-expiry), got %d events", *ttl, len(got))
		}
	}
}

// TestUpsertEventConcurrentDedupeNoDoubleInsert drives many goroutines publishing
// the SAME dedupeKey at once. Pre-fix, the lookup + insert were separate s.db calls
// so two first-publishers could both miss the (empty) dedupe lookup and double-
// insert. With the upsert wrapped in one transaction on the single connection, only
// ONE open row may exist for the key and its count must equal the publish count.
func TestUpsertEventConcurrentDedupeNoDoubleInsert(t *testing.T) {
	s := openTest(t, 1000)
	const n = 50

	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, errs[i] = s.UpsertEvent(domain.QueuePublishArgs{
				Source:    domain.SourceSystem,
				Severity:  domain.SeverityInfo,
				Title:     "t",
				Summary:   "s",
				DedupeKey: "k",
			})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}

	// Exactly one open row for the dedupe key.
	var rowCount int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM events WHERE dedupeKey = ? AND resolvedAt IS NULL", "k").
		Scan(&rowCount); err != nil {
		t.Fatal(err)
	}
	if rowCount != 1 {
		t.Fatalf("want exactly 1 open deduped row, got %d", rowCount)
	}
	// Its count reflects every publish (1 insert + n-1 dedupe bumps).
	var got int
	if err := s.db.QueryRow(
		"SELECT count FROM events WHERE dedupeKey = ? AND resolvedAt IS NULL", "k").
		Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != n {
		t.Fatalf("want count %d, got %d", n, got)
	}
}

// TestUpsertEventDoesNotReviveResolved guards the "newest OPEN row" lookup inside
// the tx: once an event is resolved, a same-key publish must INSERT a fresh row
// (count 1), never bump the just-resolved one.
func TestUpsertEventDoesNotReviveResolved(t *testing.T) {
	s := openTest(t, 1000)
	first, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceSystem, Severity: domain.SeverityInfo,
		Title: "t", Summary: "s", DedupeKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.ResolveEvent(first.ID); err != nil || !ok {
		t.Fatalf("resolve: ok=%v err=%v", ok, err)
	}
	second, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceSystem, Severity: domain.SeverityInfo,
		Title: "t2", Summary: "s2", DedupeKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID {
		t.Fatal("publish revived the resolved row instead of inserting a fresh one")
	}
	if second.Count != 1 {
		t.Fatalf("fresh row should have count 1, got %d", second.Count)
	}
}

// TestListEventsClampsMaxItems covers the LIMIT clamp: a negative MaxItems used to
// interpolate LIMIT -1 (unlimited); it must now clamp to a positive bound.
func TestListEventsClampsMaxItems(t *testing.T) {
	s := openTest(t, 1000)
	for i := 0; i < 3; i++ {
		if _, err := s.UpsertEvent(domain.QueuePublishArgs{
			Source: domain.SourceSystem, Severity: domain.SeverityInfo,
			Title: "t", Summary: "s",
		}); err != nil {
			t.Fatal(err)
		}
	}
	neg := -1
	got, err := s.ListEvents(domain.QueueDigestOptions{MaxItems: &neg})
	if err != nil {
		t.Fatal(err)
	}
	// Clamp floor is 1, so a negative request yields exactly one row, not all/none.
	if len(got) != 1 {
		t.Fatalf("negative MaxItems should clamp to LIMIT 1, got %d rows", len(got))
	}
}

// TestConsumeGrantSingleUseExhausts exercises the tx-contained consume: a 1-use
// grant must consume exactly once and then report nothing live.
func TestConsumeGrantSingleUseExhausts(t *testing.T) {
	s := openTest(t, 1000)
	g, err := s.InsertGrant(domain.AutomationGrantRecord{
		ActorID:                "w1",
		ActorType:              domain.AutomationGrantActorType("watcher"),
		AllowedRiskClassesJson: ptr(`["terminal"]`),
		ExpiresAt:              5000,
		MaxUses:                1,
		UsesRemaining:          1,
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.ConsumeGrant("w1", domain.AutomationGrantActorType("watcher"),
		"", domain.RiskClass("terminal"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || out.ID != g.ID || out.UsesRemaining != 0 {
		t.Fatalf("first consume should return the decremented grant, got %+v", out)
	}
	again, err := s.ConsumeGrant("w1", domain.AutomationGrantActorType("watcher"),
		"", domain.RiskClass("terminal"), 1000)
	if err != nil {
		t.Fatal(err)
	}
	if again != nil {
		t.Fatalf("exhausted grant should not consume again, got %+v", again)
	}
}

func ptr(s string) *string { return &s }

// TestClaimDueTimer_Guard locks the atomic claim used to fix the daemon/main-turn race: a
// claim succeeds only while the timer is STILL 'scheduled' at the read fireAt, and refuses to
// write back (and thus resurrect / double-fire) a cancelled or already-advanced row.
func TestClaimDueTimer_Guard(t *testing.T) {
	s := openTest(t, 1000)
	rec, err := s.InsertTimer(domain.TimerRecord{Title: "t", FireAt: 100, Status: "scheduled", PayloadType: "enqueue", PayloadJson: "{}"})
	if err != nil {
		t.Fatal(err)
	}
	// 1. A matching claim succeeds and advances the row out of 'scheduled'.
	if ok, err := s.ClaimDueTimer(rec.ID, 100, map[string]any{"status": "fired", "lastFiredAt": int64(1000)}); err != nil || !ok {
		t.Fatalf("matching claim should succeed: ok=%v err=%v", ok, err)
	}
	// 2. A second claim at the same (now stale) state fails — no double-fire.
	if ok, _ := s.ClaimDueTimer(rec.ID, 100, map[string]any{"status": "scheduled", "fireAt": int64(200)}); ok {
		t.Error("a claim against an already-advanced timer must fail (no double-fire)")
	}
	// 3. A timer cancelled after the due read can't be claimed — no resurrection.
	rec2, _ := s.InsertTimer(domain.TimerRecord{Title: "t2", FireAt: 100, Status: "scheduled", PayloadType: "enqueue", PayloadJson: "{}"})
	if err := s.UpdateTimer(rec2.ID, map[string]any{"status": "cancelled"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.ClaimDueTimer(rec2.ID, 100, map[string]any{"status": "scheduled", "fireAt": int64(200)}); ok {
		t.Error("a cancelled timer must not be claimable (no resurrection)")
	}
}
