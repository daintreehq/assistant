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
	// cancelled records the (id, reason) of the last CancelWatcher call so tests can
	// assert watcher.cancel stamps the user-cancel reason.
	cancelledID     string
	cancelledReason string
	revokedActor    string
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
func (m *memStore) CancelWatcher(_ context.Context, id, reason string) error {
	m.cancelledID, m.cancelledReason = id, reason
	return nil
}
func (m *memStore) RevokeGrantsByActor(_ context.Context, actorID string) (int, error) {
	m.revokedActor = actorID
	return 0, nil
}

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

// watcher.cancel stamps the user-cancel reason (so a deliberate cancel is
// distinguishable from the session-boundary sweep's 'session_ended') and revokes the
// watcher's automation grants.
func TestCancelStampsUserCancelledReason(t *testing.T) {
	st := &memStore{inserted: []domain.WatcherRecord{{ID: "wch_1", Kind: "terminal", Title: "t", Status: "active"}}}
	tool := find(Tools(Deps{Store: st}), "watcher.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"wch_1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.cancelledID != "wch_1" {
		t.Fatalf("cancelled id: got %q want wch_1", st.cancelledID)
	}
	if st.cancelledReason != reasonUserCancelled {
		t.Fatalf("cancel reason: got %q want %q", st.cancelledReason, reasonUserCancelled)
	}
	if st.revokedActor != "wch_1" {
		t.Fatalf("revoked actor: got %q want wch_1", st.revokedActor)
	}
}

// Re-cancelling an already-terminal watcher (here a 'session_ended' teardown, which
// is status='cancelled') is refused so it can't clobber the existing endedReason —
// the distinction this records would otherwise be destroyed.
func TestCancelRefusesAlreadyEndedWatcher(t *testing.T) {
	for _, status := range []string{"cancelled", "condition_met", "timeout", "error"} {
		st := &memStore{inserted: []domain.WatcherRecord{{ID: "wch_1", Kind: "terminal", Title: "t", Status: status}}}
		tool := find(Tools(Deps{Store: st}), "watcher.cancel")
		res := tool.Handle(context.Background(), json.RawMessage(`{"id":"wch_1"}`), &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeWatcherNotFound {
			t.Fatalf("status %q: expected WATCHER_NOT_FOUND, got %+v", status, res)
		}
		if st.cancelledID != "" {
			t.Fatalf("status %q: must not re-cancel/clobber a terminal watcher, got %q", status, st.cancelledID)
		}
	}
}

// Cancelling an unknown watcher fails closed (no CancelWatcher call).
func TestCancelUnknownWatcher(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "watcher.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"wch_missing"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeWatcherNotFound {
		t.Fatalf("expected WATCHER_NOT_FOUND, got %+v", res)
	}
	if st.cancelledID != "" {
		t.Fatalf("must not cancel a missing watcher, got %q", st.cancelledID)
	}
}
