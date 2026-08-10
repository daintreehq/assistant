package timer

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

type memStore struct {
	inserted []domain.TimerRecord
	revoked  []string
}

func (m *memStore) InsertTimer(_ context.Context, rec domain.TimerRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ListTimers(context.Context, string) ([]domain.TimerRecord, error) {
	return m.inserted, nil
}
func (m *memStore) GetTimer(_ context.Context, id string) (*domain.TimerRecord, error) {
	for i := range m.inserted {
		if m.inserted[i].ID == id {
			return &m.inserted[i], nil
		}
	}
	return nil, nil
}
func (m *memStore) UpdateTimerStatus(context.Context, string, string) error { return nil }
func (m *memStore) RevokeGrantsByActor(_ context.Context, id string) (int, error) {
	m.revoked = append(m.revoked, id)
	return 1, nil
}

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// An invalid fireAt is a non-recoverable TIMER_FIRE_AT (the model can't recover
// by retrying the same bad value).
func TestScheduleBadFireAt(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")
	args := json.RawMessage(`{"title":"x","fireAt":"not-a-date","payload":{"type":"enqueue"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTimerFireAt || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable TIMER_FIRE_AT, got %+v", res)
	}
}

// delayMs computes fireAt from now and persists a scheduled timer.
func TestScheduleDelayMs(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	args := json.RawMessage(`{"title":"ping","delayMs":5000,"payload":{"type":"enqueue","message":"hi"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.inserted) != 1 || st.inserted[0].Status != "scheduled" {
		t.Fatalf("timer not persisted scheduled: %+v", st.inserted)
	}
	if st.inserted[0].FireAt <= domain.NowMS() {
		t.Fatal("fireAt should be in the future")
	}
}

// call_safe_tool payload requires a toolName.
func TestScheduleCallSafeToolRequiresName(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "timer.schedule")
	args := json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"call_safe_tool"}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
}

// cancel of a known timer revokes its grants; an unknown id is non-recoverable.
func TestCancel(t *testing.T) {
	st := &memStore{inserted: []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}}}
	tool := find(Tools(Deps{Store: st}), "timer.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.revoked) != 1 || st.revoked[0] != "tmr_1" {
		t.Fatalf("expected grant revoke for cancelled timer, got %v", st.revoked)
	}

	res = tool.Handle(context.Background(), json.RawMessage(`{"id":"nope"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTimerNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable TIMER_NOT_FOUND, got %+v", res)
	}
}
