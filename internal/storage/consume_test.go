package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// consume_test.go pins the in-turn consumption retirement surface: when the main
// turn directly observes a supervised terminal's completion (terminal.awaitAll /
// a settled terminal.extract wait), ConsumeSupervisorWatchersForTerminal retires
// exactly the live SINGLE-target supervisor watcher(s) on that terminal — never a
// user-created monitor, never a multi-target watcher, never an already-ended row —
// and ResolveOpenEventsByDedupeKey clears the near-miss inbox duplicate.

func supTrue() *bool { b := true; return &b }

func insertWatcher(t *testing.T, s *Store, title, targets string, isSup *bool) domain.WatcherRecord {
	t.Helper()
	w, err := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: title, Goal: "g", TargetsJson: targets,
		CadenceMs: 3000, IsSupervisor: isSup, ModelTier: domain.ModelSmall, NextCheckAt: 1,
	})
	if err != nil {
		t.Fatalf("insert watcher %s: %v", title, err)
	}
	return w
}

func TestConsumeSupervisorWatchersForTerminal(t *testing.T) {
	now := int64(50_000)
	s, err := Open(":memory:", &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	target := insertWatcher(t, s, "watch A", `["tA"]`, supTrue())        // retires
	other := insertWatcher(t, s, "watch B", `["tB"]`, supTrue())         // different terminal
	monitor := insertWatcher(t, s, "user monitor", `["tA"]`, nil)        // NOT a supervisor
	multi := insertWatcher(t, s, "watch pair", `["tA","tC"]`, supTrue()) // multi-target
	ended := insertWatcher(t, s, "already ended", `["tA"]`, supTrue())   // already terminal
	if err := s.UpdateWatcher(ended.ID, map[string]any{
		"status": "cancelled", "endedReason": "user_cancelled", "endedAt": now - 1,
	}); err != nil {
		t.Fatal(err)
	}

	consumed, err := s.ConsumeSupervisorWatchersForTerminal("tA")
	if err != nil {
		t.Fatal(err)
	}
	if len(consumed) != 1 || consumed[0].ID != target.ID {
		t.Fatalf("expected exactly the single-target supervisor on tA to retire, got %+v", consumed)
	}
	if consumed[0].Status != "condition_met" ||
		consumed[0].EndedReason == nil || *consumed[0].EndedReason != ReasonConsumedInTurn ||
		consumed[0].EndedAt == nil || *consumed[0].EndedAt != now {
		t.Fatalf("returned record must reflect the flip, got %+v", consumed[0])
	}

	// The flip is persisted, and the retired row leaves the daemon's due set.
	got, _ := s.GetWatcher(target.ID)
	if got.Status != "condition_met" || got.EndedReason == nil || *got.EndedReason != ReasonConsumedInTurn {
		t.Fatalf("persisted row must be condition_met/%s, got %+v", ReasonConsumedInTurn, got)
	}
	due, _ := s.DueWatchers(now + 1_000_000)
	for _, w := range due {
		if w.ID == target.ID {
			t.Fatal("a consumed watcher must never be due again")
		}
	}

	// The bystanders are untouched.
	for _, id := range []string{other.ID, monitor.ID, multi.ID} {
		w, _ := s.GetWatcher(id)
		if w.Status != "active" {
			t.Errorf("watcher %s should stay active, got %s", w.Title, w.Status)
		}
	}
	// The already-ended row keeps its original reason (never clobbered).
	w, _ := s.GetWatcher(ended.ID)
	if w.Status != "cancelled" || *w.EndedReason != "user_cancelled" {
		t.Fatalf("an already-ended watcher must keep its endedReason, got %+v", w)
	}

	// Idempotent: a second consumption finds nothing live.
	again, err := s.ConsumeSupervisorWatchersForTerminal("tA")
	if err != nil || len(again) != 0 {
		t.Fatalf("second consume should retire nothing, got %v / %v", again, err)
	}
}

func TestResolveOpenEventsByDedupeKey(t *testing.T) {
	now := int64(60_000)
	s, err := Open(":memory:", &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	key := "watcher:wch_1:tA"
	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "watch A: completed success", Summary: "done", DedupeKey: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	bystander, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "watch B: needs attention", Summary: "other", DedupeKey: "watcher:wch_2:tB",
	})
	if err != nil {
		t.Fatal(err)
	}

	if n, err := s.ResolveOpenEventsByDedupeKey(key); err != nil || n != 1 {
		t.Fatalf("expected 1 resolved, got %d / %v", n, err)
	}
	got, _ := s.GetEvent(ev.ID)
	if got.ResolvedAt == nil {
		t.Fatal("the keyed event must be resolved")
	}
	side, _ := s.GetEvent(bystander.ID)
	if side.ResolvedAt != nil {
		t.Fatal("a different dedupe key must stay open")
	}

	// Idempotent, and a blank key never mass-resolves.
	if n, _ := s.ResolveOpenEventsByDedupeKey(key); n != 0 {
		t.Fatalf("second resolve should be 0, got %d", n)
	}
	if n, _ := s.ResolveOpenEventsByDedupeKey(""); n != 0 {
		t.Fatalf("blank key must resolve nothing, got %d", n)
	}
}
