package storage

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// memContents collects the content of a memory slice into a set for membership
// assertions (order-independent).
func memContents(rows []domain.MemoryRecord) map[string]bool {
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.Content] = true
	}
	return out
}

// TestMemoryTTLHidesExpiredFromListAndRecall proves the expiry predicate
// (expiresAt IS NULL OR expiresAt > now) excludes elapsed rows from both list and
// recall, that the boundary is exclusive (expiresAt == now is hidden), and that
// advancing the clock retroactively hides a row whose deadline passes.
func TestMemoryTTLHidesExpiredFromListAndRecall(t *testing.T) {
	s := openTest(t, 1000)
	// openTest closes over a constant clock; s.now is settable so the test can move
	// time and exercise the boundary precisely.
	clock := int64(1000)
	s.now = func() int64 { return clock }

	ptr := func(v int64) *int64 { return &v }
	insert := func(content string, exp *int64) {
		t.Helper()
		if _, err := s.InsertMemory(domain.MemoryRecord{Content: content, ExpiresAt: exp}); err != nil {
			t.Fatalf("insert %q: %v", content, err)
		}
	}

	insert("ttlrow never", nil)        // visible always
	insert("ttlrow future", ptr(2000)) // visible while clock < 2000
	insert("ttlrow past", ptr(500))    // already expired → hidden
	insert("ttlrow now", ptr(1000))    // expiresAt == now → hidden (strict >)

	list, err := s.ListMemories(MemoryListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	got := memContents(list)
	if !got["ttlrow never"] || !got["ttlrow future"] {
		t.Fatalf("non-expired rows missing from list: %v", got)
	}
	if got["ttlrow past"] {
		t.Fatalf("expired-in-past row leaked into list")
	}
	if got["ttlrow now"] {
		t.Fatalf("expiresAt==now row leaked (boundary must be exclusive)")
	}

	// Recall (FTS MATCH on the shared "ttlrow" token) applies the same filter.
	rec, err := s.RecallMemories("ttlrow", MemoryRecallOptions{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	gotR := memContents(rec)
	if !gotR["ttlrow never"] || !gotR["ttlrow future"] {
		t.Fatalf("non-expired rows missing from recall: %v", gotR)
	}
	if gotR["ttlrow past"] || gotR["ttlrow now"] {
		t.Fatalf("expired rows leaked into recall: %v", gotR)
	}

	// Advance past the future row's deadline: it must now disappear too.
	clock = 2000
	list2, _ := s.ListMemories(MemoryListOptions{})
	if got2 := memContents(list2); got2["ttlrow future"] {
		t.Fatalf("future row should be hidden once clock reaches its expiresAt")
	}
}

// TestMemoryTTLExpiredStillVisibleToGetAndExists confirms the filter is scoped to
// list/recall only: GetMemory (id lookup) and MemoryExists (de-dup guard) still see
// an expired row, so direct fetches work and a forgotten/expired fact still blocks
// re-insertion during distillation.
func TestMemoryTTLExpiredStillVisibleToGetAndExists(t *testing.T) {
	s := openTest(t, 5000)
	s.now = func() int64 { return 5000 }

	past := int64(100)
	rec, err := s.InsertMemory(domain.MemoryRecord{Content: "expired but fetchable", ExpiresAt: &past})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := s.GetMemory(rec.ID, false)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("GetMemory should still return an expired (non-deleted) row")
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != past {
		t.Fatalf("expiresAt did not round-trip: %v", got.ExpiresAt)
	}

	exists, err := s.MemoryExists("expired but fetchable")
	if err != nil {
		t.Fatalf("exists: %v", err)
	}
	if !exists {
		t.Fatal("MemoryExists must still count an expired row (no silent resurrection)")
	}
}

// TestMemoryProvenanceRoundTrip checks the new columns round-trip through insert →
// scan, and that an omitted Kind defaults to semantic (mirroring the SQLite DEFAULT).
func TestMemoryProvenanceRoundTrip(t *testing.T) {
	s := openTest(t, 1000)

	run := "run_abc"
	sess := "ses_xyz"
	exp := int64(9_999_999)
	in := domain.MemoryRecord{
		Content:   "episodic provenance fact",
		Kind:      domain.MemoryKindEpisodic,
		ExpiresAt: &exp,
		RunID:     &run,
		SessionID: &sess,
	}
	saved, err := s.InsertMemory(in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.GetMemory(saved.ID, false)
	if err != nil || got == nil {
		t.Fatalf("get: %v (got=%v)", err, got)
	}
	if got.Kind != domain.MemoryKindEpisodic {
		t.Fatalf("kind = %q, want episodic", got.Kind)
	}
	if got.RunID == nil || *got.RunID != run {
		t.Fatalf("runId = %v, want %q", got.RunID, run)
	}
	if got.SessionID == nil || *got.SessionID != sess {
		t.Fatalf("sessionId = %v, want %q", got.SessionID, sess)
	}
	if got.ExpiresAt == nil || *got.ExpiresAt != exp {
		t.Fatalf("expiresAt = %v, want %d", got.ExpiresAt, exp)
	}

	// Omitted Kind defaults to semantic.
	def, err := s.InsertMemory(domain.MemoryRecord{Content: "kindless fact"})
	if err != nil {
		t.Fatalf("insert kindless: %v", err)
	}
	if def.Kind != domain.MemoryKindSemantic {
		t.Fatalf("returned record kind = %q, want semantic default", def.Kind)
	}
	back, _ := s.GetMemory(def.ID, false)
	if back == nil || back.Kind != domain.MemoryKindSemantic {
		t.Fatalf("stored kind not defaulted to semantic: %v", back)
	}
	if back.RunID != nil || back.SessionID != nil || back.ExpiresAt != nil {
		t.Fatalf("unset provenance should scan as nil, got %+v", back)
	}
}
