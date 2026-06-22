package workflow

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memStore struct {
	rec *domain.WorkflowRunRecord
}

func (m *memStore) InsertWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) (string, error) {
	m.rec = &rec
	return rec.ID, nil
}
func (m *memStore) GetWorkflowRun(_ context.Context, id string) (*domain.WorkflowRunRecord, error) {
	if m.rec != nil && m.rec.ID == id {
		return m.rec, nil
	}
	return nil, nil
}
func (m *memStore) ListWorkflowRuns(context.Context, string) ([]domain.WorkflowRunRecord, error) {
	if m.rec == nil {
		return nil, nil
	}
	return []domain.WorkflowRunRecord{*m.rec}, nil
}
func (m *memStore) UpdateWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) error {
	m.rec = &rec
	return nil
}

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// A workflow BORN terminal stamps completedAt immediately.
func TestCreateBornTerminalStampsCompletedAt(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "workflow.create")
	res := tool.Handle(context.Background(), json.RawMessage(`{"status":"done"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.rec.CompletedAt == nil {
		t.Fatal("expected completedAt stamped for born-terminal status")
	}
}

// A non-terminal create leaves completedAt unset; the first transition to a
// terminal status stamps it exactly once and a later update preserves it.
func TestUpdateStampsCompletedAtOnce(t *testing.T) {
	st := &memStore{}
	create := find(Tools(Deps{Store: st}), "workflow.create")
	res := create.Handle(context.Background(), json.RawMessage(`{"status":"active"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("create: %+v", res.Error)
	}
	if st.rec.CompletedAt != nil {
		t.Fatal("active workflow should not be completed")
	}
	id := st.rec.ID

	update := find(Tools(Deps{Store: st}), "workflow.update")
	res = update.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`","status":"failed"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("update: %+v", res.Error)
	}
	first := st.rec.CompletedAt
	if first == nil {
		t.Fatal("expected completedAt stamped on first terminal transition")
	}

	// A subsequent update to another terminal status must NOT re-stamp.
	res = update.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`","status":"cancelled"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("update2: %+v", res.Error)
	}
	if st.rec.CompletedAt == nil || *st.rec.CompletedAt != *first {
		t.Fatalf("completedAt re-stamped: was %v now %v", first, st.rec.CompletedAt)
	}
}

// Array fields REPLACE wholesale on update.
func TestUpdateArraysReplace(t *testing.T) {
	st := &memStore{}
	create := find(Tools(Deps{Store: st}), "workflow.create")
	res := create.Handle(context.Background(), json.RawMessage(`{"terminalIds":["a","b"]}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("create: %+v", res.Error)
	}
	id := st.rec.ID
	update := find(Tools(Deps{Store: st}), "workflow.update")
	res = update.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`","terminalIds":["c"]}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("update: %+v", res.Error)
	}
	var ids []string
	_ = json.Unmarshal([]byte(*st.rec.TerminalIdsJson), &ids)
	if len(ids) != 1 || ids[0] != "c" {
		t.Fatalf("array not replaced: %v", ids)
	}
}

// get of a missing workflow is a non-recoverable WORKFLOW_NOT_FOUND.
func TestGetNotFound(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "workflow.get")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"nope"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeWorkflowNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable WORKFLOW_NOT_FOUND, got %+v", res)
	}
}
