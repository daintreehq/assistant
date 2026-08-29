package timer

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

type memStore struct {
	inserted    []domain.TimerRecord
	revoked     []string
	revokeCount int
	revokeErr   error
	patched     []map[string]any
	grants      map[string][]domain.AutomationGrantRecord
}

func (m *memStore) InsertTimer(rec domain.TimerRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ListTimers(string) ([]domain.TimerRecord, error) {
	return m.inserted, nil
}
func (m *memStore) GetTimer(id string) (*domain.TimerRecord, error) {
	for i := range m.inserted {
		if m.inserted[i].ID == id {
			return &m.inserted[i], nil
		}
	}
	return nil, nil
}
func (m *memStore) ClaimDueTimer(id string, expectFireAt int64, patch map[string]any) (bool, error) {
	for i := range m.inserted {
		if m.inserted[i].ID != id || m.inserted[i].Status != "scheduled" ||
			m.inserted[i].FireAt != expectFireAt {
			continue
		}
		m.patched = append(m.patched, patch)
		if st, ok := patch["status"].(string); ok {
			m.inserted[i].Status = st
		}
		return true, nil
	}
	return false, nil
}
func (m *memStore) ListGrants(actorID string, _ int64) ([]domain.AutomationGrantRecord, error) {
	return m.grants[actorID], nil
}

// revokeCount is deliberately NOT 1: a handler that hard-coded the count, or read it
// from the wrong place, would sail past an assertion of 1. revokeErr, when set, drives
// the cascade-failure branch.
func (m *memStore) RevokeGrantsByActor(id string, _ int64) (int, error) {
	m.revoked = append(m.revoked, id)
	if m.revokeErr != nil {
		return 0, m.revokeErr
	}
	return m.revokeCount, nil
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
	st := &memStore{inserted: []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}}, revokeCount: 7}
	tool := find(Tools(Deps{Store: st}), "timer.cancel")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.revoked) != 1 || st.revoked[0] != "tmr_1" {
		t.Fatalf("expected grant revoke for cancelled timer, got %v", st.revoked)
	}
	// The cascade has to be OBSERVABLE, not just performed: without revokedGrants in
	// the result the model's only record of it is a sentence in the tool description,
	// and it revokes the grant again — which used to fail. 7 is the store's number, so
	// a hard-coded count cannot satisfy this.
	if got := res.Result.(map[string]any)["revokedGrants"]; got != 7 {
		t.Fatalf("expected revokedGrants=7 (the store's count) in the result, got %v", got)
	}
	if got := res.Result.(map[string]any)["grantRevokeFailed"]; got != false {
		t.Fatalf("a successful cascade must report grantRevokeFailed=false, got %v", got)
	}

	res = tool.Handle(context.Background(), json.RawMessage(`{"id":"nope"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeTimerNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable TIMER_NOT_FOUND, got %+v", res)
	}
}

// A cascade that found nothing still reports the field, and one that FAILED must not
// report a confident 0 — the description tells the model that 0 means "no follow-up
// grant.revoke needed", so a swallowed storage error would strand a live grant behind
// a tool result that says everything is clean.
func TestCancelReportsCascadeOutcomeHonestly(t *testing.T) {
	t.Run("no grants held", func(t *testing.T) {
		st := &memStore{inserted: []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}}, revokeCount: 0}
		tool := find(Tools(Deps{Store: st}), "timer.cancel")
		res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("expected ok, got %+v", res.Error)
		}
		got, present := res.Result.(map[string]any)["revokedGrants"]
		if !present || got != 0 {
			t.Fatalf("revokedGrants must be present and 0, got %v (present=%v)", got, present)
		}
		if failed := res.Result.(map[string]any)["grantRevokeFailed"]; failed != false {
			t.Fatalf("want grantRevokeFailed=false, got %v", failed)
		}
	})

	t.Run("cascade failed", func(t *testing.T) {
		st := &memStore{
			inserted:  []domain.TimerRecord{{ID: "tmr_1", Status: "scheduled"}},
			revokeErr: errors.New("db locked"),
		}
		tool := find(Tools(Deps{Store: st}), "timer.cancel")
		res := tool.Handle(context.Background(), json.RawMessage(`{"id":"tmr_1"}`), &tools.ToolContext{})
		// Still Ok: the timer really is cancelled, and failing the call would be the
		// bigger lie. The failure rides the result instead.
		if !res.Ok {
			t.Fatalf("a failed cascade must not fail the cancel, got %+v", res.Error)
		}
		if failed := res.Result.(map[string]any)["grantRevokeFailed"]; failed != true {
			t.Fatalf("want grantRevokeFailed=true, got %v", failed)
		}
		if !strings.Contains(res.Summary, "grant.revoke") {
			t.Fatalf("the summary must point at the recovery, got %q", res.Summary)
		}
	})
}
