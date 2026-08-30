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

// A payload the registry says can never run at fire time is refused AT SCHEDULE TIME,
// and nothing is stored.
//
// This is the bug the preflight seam exists for: a timer-dispatched spawn that named
// no worktree was accepted, reported back as "Scheduled.", and then failed on its only
// firing into a queue row nobody had open. Refusing here is what puts the error in
// front of the model while it can still fix the call.
func TestScheduleRefusesAnUnrunnablePayload(t *testing.T) {
	st := &memStore{}
	var sawTool string
	var sawArgs string
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(name string, args json.RawMessage) (string, string) {
			sawTool, sawArgs = name, string(args)
			return "", "it names no worktreeId"
		},
	}), "timer.schedule")

	args := json.RawMessage(`{"title":"spawn","delayMs":10000,"payload":` +
		`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits","args":{"title":"go"}}}}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})

	if res.Ok || res.Error.Code != codeTimerUnrunnable {
		t.Fatalf("expected %s, got %+v", codeTimerUnrunnable, res)
	}
	// Retrying the identical call cannot help — only a different call can.
	if res.Error.Recoverable {
		t.Fatal("an unrunnable payload is not recoverable by retrying it")
	}
	// The reason has to reach the model, not just a code: the code says "no" and the
	// reason is the only part that says what to write instead.
	if !strings.Contains(res.Error.Message, "names no worktreeId") {
		t.Fatalf("failure should carry the tool's own reason, got %q", res.Error.Message)
	}
	if len(st.inserted) != 0 {
		t.Fatalf("a refused schedule must persist nothing, got %+v", st.inserted)
	}
	// The tool is asked about ITS OWN arguments, not the timer's.
	if sawTool != "agentTask.spawnForEdits" || !strings.Contains(sawArgs, `"title":"go"`) {
		t.Fatalf("preflight got (%q, %q)", sawTool, sawArgs)
	}
}

// The preflight is consulted for the tool payload only — a reminder has no tool to ask.
func TestScheduleSkipsPreflightForAReminder(t *testing.T) {
	called := false
	st := &memStore{}
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(n string, _ json.RawMessage) (string, string) {
			called = true
			return n, "no"
		},
	}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":{"type":"enqueue","message":"hi"}}`),
		&tools.ToolContext{})
	if !res.Ok || called {
		t.Fatalf("a reminder should schedule without a preflight; ok=%v called=%v", res.Ok, called)
	}
}

// No preflight wired ⇒ scheduling behaves exactly as it did before the seam existed.
func TestScheduleWithoutPreflightIsUnchanged(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "timer.schedule")
	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"spawn","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask.spawnForEdits"}}}`),
		&tools.ToolContext{})
	if !res.Ok || len(st.inserted) != 1 {
		t.Fatalf("expected a stored timer, got ok=%v inserted=%+v", res.Ok, st.inserted)
	}
}

// The name that gets STORED is the one dispatch will look up, not the one that was
// typed. Fire-time dispatch resolves nothing, so persisting a wire spelling is how a
// payload that passed every check still dies with UNKNOWN_TOOL on its only firing.
func TestScheduleStoresTheCanonicalToolName(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{
		Store: st,
		PrepareScheduledCall: func(name string, _ json.RawMessage) (string, string) {
			if name == "agentTask__spawnForEdits" {
				return "agentTask.spawnForEdits", ""
			}
			return name, ""
		},
	}), "timer.schedule")

	res := tool.Handle(context.Background(),
		json.RawMessage(`{"title":"spawn","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"agentTask__spawnForEdits","args":{"worktreeId":"/w"}}}}`),
		&tools.ToolContext{})
	if !res.Ok || len(st.inserted) != 1 {
		t.Fatalf("expected a stored timer, got ok=%v inserted=%+v", res.Ok, st.inserted)
	}
	if !strings.Contains(st.inserted[0].PayloadJson, `"toolName":"agentTask.spawnForEdits"`) {
		t.Fatalf("the stored payload should carry the resolved name, got %s", st.inserted[0].PayloadJson)
	}
}

// Absent args reach the check as `{}` — what dispatch will hand the handler — rather
// than as the `null` a nil map marshals to, which no decoder accepts.
func TestSchedulePassesAbsentArgsAsAnEmptyObject(t *testing.T) {
	var saw string
	tool := find(Tools(Deps{
		Store: &memStore{},
		PrepareScheduledCall: func(n string, args json.RawMessage) (string, string) {
			saw = string(args)
			return n, ""
		},
	}), "timer.schedule")

	tool.Handle(context.Background(),
		json.RawMessage(`{"title":"x","delayMs":1000,"payload":`+
			`{"type":"call_safe_tool","toolCall":{"toolName":"fs.read"}}}`),
		&tools.ToolContext{})
	if saw != "{}" {
		t.Fatalf("absent args should arrive as {}, got %q", saw)
	}
}
