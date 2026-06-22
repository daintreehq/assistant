package watcher

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memStore struct {
	inserted []domain.WatcherRecord
}

func (m *memStore) InsertWatcher(_ context.Context, rec domain.WatcherRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ListWatchers(context.Context, string) ([]domain.WatcherRecord, error) {
	return m.inserted, nil
}
func (m *memStore) GetWatcher(_ context.Context, id string) (*domain.WatcherRecord, error) {
	for i := range m.inserted {
		if m.inserted[i].ID == id {
			return &m.inserted[i], nil
		}
	}
	return nil, nil
}
func (m *memStore) UpdateWatcherStatus(context.Context, string, string) error { return nil }
func (m *memStore) RevokeGrantsByActor(context.Context, string) (int, error)  { return 0, nil }

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// A degenerate watch condition (empty contains) must be rejected — a watcher
// built from it could never meaningfully fire.
func TestTerminalCreateRejectsDegenerateCondition(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "watcher.terminal.create")
	args := json.RawMessage(`{"terminalIds":["t1"],"title":"x","goal":"g","stopWhen":{"contains":"   "}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for degenerate condition, got %+v", res)
	}
}

// A multi-key watch condition is rejected (exactly one variant key).
func TestTerminalCreateRejectsMultiKeyCondition(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "watcher.terminal.create")
	args := json.RawMessage(`{"terminalIds":["t1"],"title":"x","goal":"g","alertWhen":{"contains":"err","regex":"x"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for multi-key condition, got %+v", res)
	}
}

// A valid create persists a non-supervisor terminal watcher and honours the
// default monitor cadence + startAfterMs offset.
func TestTerminalCreateDefaults(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "watcher.terminal.create")
	args := json.RawMessage(`{"terminalIds":["t1","t2"],"title":"watch","goal":"done","startAfterMs":1000}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.inserted) != 1 {
		t.Fatalf("expected 1 watcher, got %d", len(st.inserted))
	}
	w := st.inserted[0]
	if w.CadenceMs != monitorDefaultCadenceMs {
		t.Fatalf("cadence: got %d want %d", w.CadenceMs, monitorDefaultCadenceMs)
	}
	if w.IsSupervisor == nil || *w.IsSupervisor {
		t.Fatal("expected non-supervisor watcher")
	}
	if w.Kind != "terminal" {
		t.Fatalf("kind: %s", w.Kind)
	}
}

// watchPR uses the fixed PR cadence and a display-label targetsJson.
func TestWatchPRFixedCadence(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "watcher.watchPR")
	res := tool.Handle(context.Background(), json.RawMessage(`{"prNumber":42}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	w := st.inserted[0]
	if w.CadenceMs != prWatcherCadenceMs {
		t.Fatalf("pr cadence: got %d want %d", w.CadenceMs, prWatcherCadenceMs)
	}
	if w.Kind != "pr_state" {
		t.Fatalf("kind: %s", w.Kind)
	}
	var targets []string
	_ = json.Unmarshal([]byte(w.TargetsJson), &targets)
	if len(targets) != 1 || targets[0] != "PR #42" {
		t.Fatalf("targetsJson: %v", targets)
	}
}
