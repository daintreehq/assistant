package storage

import (
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

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
