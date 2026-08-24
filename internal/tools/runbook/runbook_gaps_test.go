package runbook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// keyedStore models the (sessionId, runbookId) natural key so we can assert
// per-runbook isolation within a session and cross-session isolation.
type keyedStore struct {
	recs map[string]*domain.RunbookRunStateRecord
}

func newKeyedStore() *keyedStore {
	return &keyedStore{recs: map[string]*domain.RunbookRunStateRecord{}}
}

func key(session, runbook string) string { return session + "\x00" + runbook }

func (s *keyedStore) GetRunbookRunState(_ context.Context, session, runbook string) (*domain.RunbookRunStateRecord, error) {
	return s.recs[key(session, runbook)], nil
}
func (s *keyedStore) InsertRunbookRunState(_ context.Context, rec domain.RunbookRunStateRecord) (string, error) {
	cp := rec
	s.recs[key(rec.SessionID, rec.RunbookID)] = &cp
	return rec.ID, nil
}
func (s *keyedStore) UpdateRunbookRunState(_ context.Context, rec domain.RunbookRunStateRecord) error {
	cp := rec
	s.recs[key(rec.SessionID, rec.RunbookID)] = &cp
	return nil
}

func advance(t *testing.T, tool *tools.Tool, ctx *tools.ToolContext, body string) map[string]any {
	t.Helper()
	res := tool.Handle(context.Background(), json.RawMessage(body), ctx)
	if !res.Ok {
		t.Fatalf("advance failed: %+v", res.Error)
	}
	return res.Result.(map[string]any)["state"].(map[string]any)
}

func steps(state map[string]any) []domain.RunbookStepProgress {
	raw, _ := json.Marshal(state["steps"])
	var out []domain.RunbookStepProgress
	_ = json.Unmarshal(raw, &out)
	return out
}

// A step status defaults to 'done' when omitted, and the checkpoint array
// accumulates across advances.
func TestStepAdvanceDefaultDoneAndAccumulates(t *testing.T) {
	st := newKeyedStore()
	tool := find(Tools(Deps{Store: st}), "runbook.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}

	s1 := advance(t, tool, ctx, `{"runbookId":"r.flow","completedStep":1,"nextStep":2}`)
	if steps(s1)[0].Status != domain.RunbookStepDone {
		t.Fatalf("default status: %v", steps(s1)[0].Status)
	}
	s2 := advance(t, tool, ctx, `{"runbookId":"r.flow","completedStep":2,"nextStep":3}`)
	idx := func(ss []domain.RunbookStepProgress) []int {
		out := make([]int, len(ss))
		for i, s := range ss {
			out[i] = s.Index
		}
		return out
	}
	if got := idx(steps(s2)); len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("steps not accumulated: %v", got)
	}
}

// A repeated advance of the same step is idempotent: latest note wins, no dup.
func TestStepAdvanceIdempotentLatestNoteWins(t *testing.T) {
	st := newKeyedStore()
	tool := find(Tools(Deps{Store: st}), "runbook.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}

	advance(t, tool, ctx, `{"runbookId":"r.flow","completedStep":1,"nextStep":2,"notes":"first"}`)
	state := advance(t, tool, ctx, `{"runbookId":"r.flow","completedStep":1,"nextStep":2,"notes":"second"}`)
	ss := steps(state)
	if len(ss) != 1 {
		t.Fatalf("duplicate step entry: %d", len(ss))
	}
	if ss[0].Notes == nil || *ss[0].Notes != "second" {
		t.Fatalf("latest note should win: %v", ss[0].Notes)
	}
}

// A skipped step records its status and note.
func TestStepAdvanceSkippedWithNote(t *testing.T) {
	st := newKeyedStore()
	tool := find(Tools(Deps{Store: st}), "runbook.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}
	state := advance(t, tool, ctx, `{"runbookId":"r.flow","completedStep":1,"nextStep":2,"status":"skipped","notes":"n/a here"}`)
	ss := steps(state)
	if ss[0].Status != domain.RunbookStepSkipped || ss[0].Notes == nil || *ss[0].Notes != "n/a here" {
		t.Fatalf("skipped step not recorded: %+v", ss[0])
	}
}

// Corrupted stored step entries are dropped; the new step is still recorded.
func TestStepAdvanceDropsCorruptedEntries(t *testing.T) {
	st := newKeyedStore()
	st.recs[key("sess1", "r.corrupt")] = &domain.RunbookRunStateRecord{
		ID: "rrs_x", SessionID: "sess1", RunbookID: "r.corrupt", CurrentStep: 1,
		StepsJson: `[{"index":1,"status":"blocked","ts":1},"garbage"]`, Status: domain.RunbookRunActive,
	}
	tool := find(Tools(Deps{Store: st}), "runbook.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}
	state := advance(t, tool, ctx, `{"runbookId":"r.corrupt","completedStep":2,"nextStep":3}`)
	ss := steps(state)
	if len(ss) != 1 || ss[0].Index != 2 || ss[0].Status != domain.RunbookStepDone {
		t.Fatalf("corrupted entries not dropped cleanly: %+v", ss)
	}
}

// Separate runs per runbook within the same session are isolated.
func TestStepAdvanceSeparateRunsPerRunbook(t *testing.T) {
	st := newKeyedStore()
	tool := find(Tools(Deps{Store: st}), "runbook.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}
	advance(t, tool, ctx, `{"runbookId":"r.a","completedStep":1,"nextStep":2}`)
	advance(t, tool, ctx, `{"runbookId":"r.b","completedStep":1,"nextStep":2}`)
	if st.recs[key("sess1", "r.a")] == nil || st.recs[key("sess1", "r.b")] == nil {
		t.Fatal("expected two distinct runs")
	}
}

// runbook.run.get returns ok with null state (not a failure) when none exists, and
// isolates checkpoints by session.
func TestRunGetNullStateAndSessionIsolation(t *testing.T) {
	st := newKeyedStore()
	advance := find(Tools(Deps{Store: st}), "runbook.step.advance")
	get := find(Tools(Deps{Store: st}), "runbook.run.get")

	none := get.Handle(context.Background(), json.RawMessage(`{"runbookId":"r.never"}`), &tools.ToolContext{SessionID: "sess1"})
	if !none.Ok || none.Result.(map[string]any)["state"] != nil {
		t.Fatalf("missing run should be ok+null, got %+v", none)
	}

	advance.Handle(context.Background(), json.RawMessage(`{"runbookId":"r.flow","completedStep":1,"nextStep":2}`), &tools.ToolContext{SessionID: "sess_one"})
	other := get.Handle(context.Background(), json.RawMessage(`{"runbookId":"r.flow"}`), &tools.ToolContext{SessionID: "sess_two"})
	if !other.Ok || other.Result.(map[string]any)["state"] != nil {
		t.Fatalf("another session must see no checkpoint, got %+v", other)
	}
}
