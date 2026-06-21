package workflow

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

var wfIDRe = regexp.MustCompile(`^wfr_[0-9a-f]{8}$`)

// multiStore holds many workflow runs and filters list by status (the slice the
// real store's ListWorkflowRuns applies).
type multiStore struct {
	recs []domain.WorkflowRunRecord
}

func (s *multiStore) InsertWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) (string, error) {
	s.recs = append(s.recs, rec)
	return rec.ID, nil
}
func (s *multiStore) GetWorkflowRun(_ context.Context, id string) (*domain.WorkflowRunRecord, error) {
	for i := range s.recs {
		if s.recs[i].ID == id {
			return &s.recs[i], nil
		}
	}
	return nil, nil
}
func (s *multiStore) ListWorkflowRuns(_ context.Context, status string) ([]domain.WorkflowRunRecord, error) {
	var out []domain.WorkflowRunRecord
	for _, r := range s.recs {
		if status != "" && string(r.Status) != status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}
func (s *multiStore) UpdateWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) error {
	for i := range s.recs {
		if s.recs[i].ID == rec.ID {
			s.recs[i] = rec
			return nil
		}
	}
	return nil
}

// workflow.create returns an id of shape wfr_<8 hex>.
func TestCreateIDShape(t *testing.T) {
	st := &multiStore{}
	tool := find(Tools(Deps{Store: st}), "workflow.create")
	res := tool.Handle(context.Background(), json.RawMessage(`{"issueNumber":1}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("create: %+v", res.Error)
	}
	id := res.Result.(map[string]any)["id"].(string)
	if !wfIDRe.MatchString(id) {
		t.Fatalf("id shape: %q", id)
	}
}

// workflow.list filters by status.
func TestListFiltersByStatus(t *testing.T) {
	st := &multiStore{}
	create := find(Tools(Deps{Store: st}), "workflow.create")
	list := find(Tools(Deps{Store: st}), "workflow.list")
	for _, s := range []string{"active", "blocked", "active"} {
		if r := create.Handle(context.Background(), json.RawMessage(`{"status":"`+s+`"}`), &tools.ToolContext{}); !r.Ok {
			t.Fatalf("create %s: %+v", s, r.Error)
		}
	}
	active := list.Handle(context.Background(), json.RawMessage(`{"status":"active"}`), &tools.ToolContext{})
	if wfs := active.Result.(map[string]any)["workflows"]; countWfs(wfs) != 2 {
		t.Fatalf("expected 2 active, got %d", countWfs(wfs))
	}
	all := list.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if wfs := all.Result.(map[string]any)["workflows"]; countWfs(wfs) != 3 {
		t.Fatalf("expected 3 total, got %d", countWfs(wfs))
	}
}

func countWfs(v any) int {
	switch w := v.(type) {
	case []map[string]any:
		return len(w)
	case []domain.WorkflowRunRecord:
		return len(w)
	case []any:
		return len(w)
	}
	return -1
}

// workflow.update patches one field and leaves other fields intact (incl arrays).
func TestUpdateLeavesOtherFieldsIntact(t *testing.T) {
	st := &multiStore{}
	create := find(Tools(Deps{Store: st}), "workflow.create")
	update := find(Tools(Deps{Store: st}), "workflow.update")

	res := create.Handle(context.Background(),
		json.RawMessage(`{"issueNumber":5,"issueTitle":"keep me","terminalIds":["term_1"]}`), &tools.ToolContext{})
	id := res.Result.(map[string]any)["id"].(string)

	upd := update.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`","prNumber":77}`), &tools.ToolContext{})
	if !upd.Ok {
		t.Fatalf("update: %+v", upd.Error)
	}
	w := upd.Result.(map[string]any)["workflow"].(map[string]any)
	if w["prNumber"] != 77 && w["prNumber"] != float64(77) {
		t.Fatalf("prNumber not set: %v", w["prNumber"])
	}
	if w["issueTitle"] != "keep me" {
		t.Fatalf("issueTitle clobbered: %v", w["issueTitle"])
	}
	ids, _ := w["terminalIds"].([]string)
	if len(ids) != 1 || ids[0] != "term_1" {
		t.Fatalf("terminalIds clobbered: %v", w["terminalIds"])
	}
}
