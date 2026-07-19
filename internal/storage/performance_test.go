package storage

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestListLiveWatchersExcludesTerminalHistory(t *testing.T) {
	s := openTest(t, 1000)
	statuses := []string{"active", "created", "paused", "cancelled", "condition_met", "timeout", "error"}
	for i, status := range statuses {
		_, err := s.InsertWatcher(domain.WatcherRecord{
			ID:          "wch_live_filter_" + status,
			Kind:        "terminal",
			Title:       status,
			Goal:        "test",
			TargetsJson: `["term_1"]`,
			CadenceMs:   3000,
			ModelTier:   domain.ModelSmall,
			Status:      status,
			NextCheckAt: int64(100 + i),
			CreatedAt:   int64(100 + i),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListLiveWatchers()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("live rows = %d, want active/created/paused only", len(got))
	}
	for i, want := range statuses[:3] {
		if got[i].Status != want {
			t.Errorf("row %d status = %q, want %q", i, got[i].Status, want)
		}
	}
}

func TestSchedulerAndWatcherIndexesPresent(t *testing.T) {
	s := openTest(t, 1)
	for _, name := range []string{"idx_timers_due", "idx_watchers_due", "idx_watchers_status_created"} {
		var got string
		if err := s.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&got); err != nil {
			t.Errorf("index %s missing: %v", name, err)
		}
	}
}
