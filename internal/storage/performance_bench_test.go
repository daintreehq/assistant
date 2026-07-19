package storage

import (
	"fmt"
	"path/filepath"
	"testing"
)

const benchmarkHistoryRows = 50_000

func openBenchmarkStore(b *testing.B) *Store {
	b.Helper()
	s, err := Open(filepath.Join(b.TempDir(), "state.db"), &Options{Now: func() int64 { return 1_000_000 }})
	if err != nil {
		b.Fatalf("Open: %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	return s
}

func seedTimerHistory(b *testing.B, s *Store, rows int) {
	b.Helper()
	_, err := s.db.Exec(`
		WITH RECURSIVE seq(x) AS (
			SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?
		)
		INSERT INTO timers
			(id,title,fireAt,runCount,payloadType,payloadJson,status,createdAt)
		SELECT printf('tmr_hist_%08d', x), 'historical timer', x, 1,
			'notify', '{}', 'fired', x
		FROM seq`, rows)
	if err != nil {
		b.Fatalf("seed timer history: %v", err)
	}
	for i := 0; i < 8; i++ {
		_, err = s.db.Exec(`INSERT INTO timers
			(id,title,fireAt,runCount,payloadType,payloadJson,status,createdAt)
			VALUES (?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("tmr_due_%02d", i), "due timer", 100+i, 0, "notify", "{}", "scheduled", int64(i))
		if err != nil {
			b.Fatalf("seed due timer: %v", err)
		}
	}
}

func seedWatcherHistory(b *testing.B, s *Store, rows int) {
	b.Helper()
	_, err := s.db.Exec(`
		WITH RECURSIVE seq(x) AS (
			SELECT 1 UNION ALL SELECT x + 1 FROM seq WHERE x < ?
		)
		INSERT INTO watchers
			(id,kind,title,goal,targetsJson,cadenceMs,isSupervisor,modelTier,
			 status,nextCheckAt,createdAt)
		SELECT printf('wch_hist_%08d', x), 'terminal', 'historical watcher', 'done',
			'["term_old"]', 3000, 0, 'fast', 'cancelled', x, x
		FROM seq`, rows)
	if err != nil {
		b.Fatalf("seed watcher history: %v", err)
	}
	statuses := []string{"active", "created", "paused", "active", "active", "active", "active", "active"}
	for i, status := range statuses {
		_, err = s.db.Exec(`INSERT INTO watchers
			(id,kind,title,goal,targetsJson,cadenceMs,isSupervisor,modelTier,
			 status,nextCheckAt,createdAt)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
			fmt.Sprintf("wch_live_%02d", i), "terminal", "live watcher", "working",
			fmt.Sprintf(`["term_%02d"]`, i), 3000, 0, "fast", status, 100, 1_000_000+int64(i))
		if err != nil {
			b.Fatalf("seed live watcher: %v", err)
		}
	}
}

func BenchmarkDueTimers50KHistory(b *testing.B) {
	s := openBenchmarkStore(b)
	seedTimerHistory(b, s, benchmarkHistoryRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.DueTimers(1_000_000)
		if err != nil || len(rows) != 8 {
			b.Fatalf("DueTimers: rows=%d err=%v", len(rows), err)
		}
	}
}

func BenchmarkDueWatchers50KHistory(b *testing.B) {
	s := openBenchmarkStore(b)
	seedWatcherHistory(b, s, benchmarkHistoryRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.DueWatchers(1_000_000)
		if err != nil || len(rows) != 6 {
			b.Fatalf("DueWatchers: rows=%d err=%v", len(rows), err)
		}
	}
}

// BenchmarkDashboardWatcherSnapshot50KHistory models the dashboard's once-per-second
// watcher snapshot. The dashboard only renders live rows, so terminal history is
// deliberately dominant in this fixture.
func BenchmarkDashboardWatcherSnapshot50KHistory(b *testing.B) {
	s := openBenchmarkStore(b)
	seedWatcherHistory(b, s, benchmarkHistoryRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := s.ListLiveWatchers()
		if err != nil || len(rows) != 8 {
			b.Fatalf("dashboard watcher snapshot: rows=%d err=%v", len(rows), err)
		}
	}
}

func BenchmarkBeginOwnership50KWatcherHistory(b *testing.B) {
	s := openBenchmarkStore(b)
	seedWatcherHistory(b, s, benchmarkHistoryRows)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		summary, err := s.BeginOwnership(1_000_000)
		if err != nil || len(summary.ResumedWatcherTitles) != 8 {
			b.Fatalf("BeginOwnership: resumed=%d err=%v", len(summary.ResumedWatcherTitles), err)
		}
	}
}
