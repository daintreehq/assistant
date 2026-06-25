package storage

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestCheckpointRoundTrip — a single upsert is reloaded verbatim from the 'latest'
// slot with every promoted column intact.
func TestCheckpointRoundTrip(t *testing.T) {
	s := openTest(t, 1000)
	in := domain.ContextCheckpointRecord{
		CompactionDepth: 3,
		SummaryText:     "compacted state",
		LastRunID:       "run_abc",
		LastSeq:         42,
		PayloadJson:     `{"goal":"ship it"}`,
	}
	if err := s.UpsertContextCheckpoint(in); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.LoadLatestCheckpoint()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !found {
		t.Fatal("expected a checkpoint, got none")
	}
	if got.Slot != checkpointSlotLatest {
		t.Errorf("slot want %q got %q", checkpointSlotLatest, got.Slot)
	}
	if got.CompactionDepth != 3 || got.SummaryText != "compacted state" ||
		got.LastRunID != "run_abc" || got.LastSeq != 42 || got.PayloadJson != `{"goal":"ship it"}` {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	// CreatedAt defaults to the frozen clock when zero.
	if got.CreatedAt != 1000 {
		t.Errorf("createdAt want 1000 got %d", got.CreatedAt)
	}
}

// TestCheckpointEmptyReturnsNotFound — a fresh DB has no checkpoint.
func TestCheckpointEmptyReturnsNotFound(t *testing.T) {
	s := openTest(t, 1)
	_, found, err := s.LoadLatestCheckpoint()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if found {
		t.Fatal("expected no checkpoint on a fresh DB")
	}
}

// TestCheckpointEmptyPayloadIsValid — an empty payloadJson is a usable checkpoint
// (corruption is a PARSE failure, not an empty object), so it is returned, not skipped.
func TestCheckpointEmptyPayloadIsValid(t *testing.T) {
	s := openTest(t, 1)
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 1, SummaryText: "s", LastSeq: 5, PayloadJson: "",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, found, err := s.LoadLatestCheckpoint()
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.PayloadJson != "" || got.CompactionDepth != 1 {
		t.Errorf("unexpected record: %+v", got)
	}
}

// TestCheckpointRotation — a second upsert becomes 'latest'; the first survives in
// 'prev'. Load returns the newest.
func TestCheckpointRotation(t *testing.T) {
	s := openTest(t, 1)
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 1, SummaryText: "first", LastSeq: 10, PayloadJson: `{"v":1}`,
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 2, SummaryText: "second", LastSeq: 20, PayloadJson: `{"v":2}`,
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	got, found, err := s.LoadLatestCheckpoint()
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.CompactionDepth != 2 || got.SummaryText != "second" {
		t.Errorf("latest want second/depth2, got %+v", got)
	}
	// The prior latest is preserved in the prev slot.
	var prevSummary string
	if err := s.DB().QueryRow(
		"SELECT summaryText FROM context_checkpoints WHERE slot = ?", checkpointSlotPrev,
	).Scan(&prevSummary); err != nil {
		t.Fatalf("read prev slot: %v", err)
	}
	if prevSummary != "first" {
		t.Errorf("prev slot want 'first', got %q", prevSummary)
	}
}

// TestCheckpointCorruptLatestFallsBackToPrev — when the 'latest' payload is
// unparseable, Load returns the previous valid checkpoint instead.
func TestCheckpointCorruptLatestFallsBackToPrev(t *testing.T) {
	s := openTest(t, 1)
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 1, SummaryText: "good-prev", LastSeq: 10, PayloadJson: `{"v":1}`,
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 2, SummaryText: "to-corrupt", LastSeq: 20, PayloadJson: `{"v":2}`,
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	// Corrupt the latest slot's payload directly (simulating on-disk corruption).
	if _, err := s.DB().Exec(
		"UPDATE context_checkpoints SET payloadJson = ? WHERE slot = ?",
		`{"v":2`, checkpointSlotLatest,
	); err != nil {
		t.Fatalf("corrupt latest: %v", err)
	}
	got, found, err := s.LoadLatestCheckpoint()
	if err != nil || !found {
		t.Fatalf("load: found=%v err=%v", found, err)
	}
	if got.Slot != checkpointSlotPrev || got.SummaryText != "good-prev" {
		t.Errorf("expected fallback to prev/good-prev, got %+v", got)
	}
}

// TestCheckpointBothCorruptReturnsNotFound — when both slots are unparseable, Load
// reports no valid checkpoint rather than returning garbage.
func TestCheckpointBothCorruptReturnsNotFound(t *testing.T) {
	s := openTest(t, 1)
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 1, SummaryText: "a", LastSeq: 10, PayloadJson: `{"v":1}`,
	}); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	if err := s.UpsertContextCheckpoint(domain.ContextCheckpointRecord{
		CompactionDepth: 2, SummaryText: "b", LastSeq: 20, PayloadJson: `{"v":2}`,
	}); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}
	if _, err := s.DB().Exec(
		"UPDATE context_checkpoints SET payloadJson = '{bad'"); err != nil {
		t.Fatalf("corrupt both: %v", err)
	}
	_, found, err := s.LoadLatestCheckpoint()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if found {
		t.Fatal("expected no valid checkpoint when both slots are corrupt")
	}
}
