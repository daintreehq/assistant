package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func decodeOpts(t *testing.T, rec WatcherRecord) map[string]any {
	t.Helper()
	if rec.OptionsJson == nil {
		t.Fatalf("OptionsJson must not be nil")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*rec.OptionsJson), &m); err != nil {
		t.Fatalf("OptionsJson is not valid JSON: %v", err)
	}
	return m
}

func decodeTargets(t *testing.T, rec WatcherRecord) []string {
	t.Helper()
	var ts []string
	if err := json.Unmarshal([]byte(rec.TargetsJson), &ts); err != nil {
		t.Fatalf("TargetsJson is not a JSON []string: %v", err)
	}
	return ts
}

// TestBuildSupervisorWatcherRecordFixedFields pins the invariant supervisor shape:
// every attach path must produce a terminal-kind, supervisor, small-model watcher
// targeting exactly the one terminal, active and immediately due.
func TestBuildSupervisorWatcherRecordFixedFields(t *testing.T) {
	rec := BuildSupervisorWatcherRecord(SupervisorWatcherSpec{
		TerminalID: "term_1",
		Title:      "watch Fix OAuth",
		Goal:       "supervise it",
		CadenceMs:  3000,
		SpawnMode:  "edit",
	})
	if !strings.HasPrefix(rec.ID, PrefixWatcher) {
		t.Errorf("ID should carry the watcher prefix, got %q", rec.ID)
	}
	if rec.Kind != "terminal" {
		t.Errorf("Kind = %q, want terminal", rec.Kind)
	}
	if rec.IsSupervisor == nil || !*rec.IsSupervisor {
		t.Errorf("IsSupervisor must be true")
	}
	if rec.ModelTier != ModelSmall {
		t.Errorf("ModelTier = %q, want small", rec.ModelTier)
	}
	if rec.Status != "active" {
		t.Errorf("Status = %q, want active", rec.Status)
	}
	if rec.Title != "watch Fix OAuth" || rec.Goal != "supervise it" || rec.CadenceMs != 3000 {
		t.Errorf("title/goal/cadence not passed through: %+v", rec)
	}
	if rec.NextCheckAt <= 0 || rec.CreatedAt <= 0 {
		t.Errorf("NextCheckAt/CreatedAt must be stamped, got %d/%d", rec.NextCheckAt, rec.CreatedAt)
	}
	if ts := decodeTargets(t, rec); len(ts) != 1 || ts[0] != "term_1" {
		t.Errorf("targets = %v, want [term_1]", ts)
	}
	if opts := decodeOpts(t, rec); opts["spawnMode"] != "edit" {
		t.Errorf("spawnMode = %v, want edit", opts["spawnMode"])
	}
}

// TestBuildSupervisorWatcherRecordOptions covers the conditional options keys:
// verificationScope only when a worktree is known, acceptanceCriteria only when set,
// and ExtraOptions merged on top.
func TestBuildSupervisorWatcherRecordOptions(t *testing.T) {
	t.Run("worktree + criteria + extra", func(t *testing.T) {
		rec := BuildSupervisorWatcherRecord(SupervisorWatcherSpec{
			TerminalID:         "t",
			WorktreeID:         "wt-9",
			Title:              "watch x",
			Goal:               "g",
			CadenceMs:          1000,
			SpawnMode:          "explore",
			AcceptanceCriteria: "all tests pass",
			ExtraOptions:       map[string]any{"adoptMode": true},
		})
		opts := decodeOpts(t, rec)
		if opts["spawnMode"] != "explore" {
			t.Errorf("spawnMode = %v, want explore", opts["spawnMode"])
		}
		if opts["acceptanceCriteria"] != "all tests pass" {
			t.Errorf("acceptanceCriteria = %v", opts["acceptanceCriteria"])
		}
		if opts["adoptMode"] != true {
			t.Errorf("adoptMode should be merged true, got %v", opts["adoptMode"])
		}
		vs, ok := opts["verificationScope"].(map[string]any)
		if !ok || vs["worktreeId"] != "wt-9" {
			t.Errorf("verificationScope = %v, want {worktreeId: wt-9}", opts["verificationScope"])
		}
	})

	t.Run("no worktree, no criteria", func(t *testing.T) {
		rec := BuildSupervisorWatcherRecord(SupervisorWatcherSpec{
			TerminalID: "t", Title: "watch x", Goal: "g", CadenceMs: 1000, SpawnMode: "edit",
		})
		opts := decodeOpts(t, rec)
		if _, present := opts["verificationScope"]; present {
			t.Errorf("verificationScope must be absent without a worktree, got %v", opts["verificationScope"])
		}
		if _, present := opts["acceptanceCriteria"]; present {
			t.Errorf("acceptanceCriteria must be absent when empty")
		}
	})
}
