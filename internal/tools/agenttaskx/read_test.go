package agenttaskx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// errReadStore wraps a sagaStore but forces the read methods to error, so the
// CodeInternal path can be exercised. The other four methods are promoted.
type errReadStore struct {
	*sagaStore
	err error
}

func (e errReadStore) GetAgentLaunch(string) (*domain.AgentLaunchRecord, error) {
	return nil, e.err
}
func (e errReadStore) ListAgentLaunches(int) ([]domain.AgentLaunchRecord, error) {
	return nil, e.err
}

func callStatus(db Store, id string) tools.ToolResult {
	tool := newStatusTool(Deps{DB: db})
	raw, _ := json.Marshal(statusArgs{LaunchID: id})
	return tool.Handle(context.Background(), raw, nil)
}

func callList(db Store) tools.ToolResult {
	tool := newListTool(Deps{DB: db})
	return tool.Handle(context.Background(), json.RawMessage(`{}`), nil)
}

func TestStatusToolFound(t *testing.T) {
	st := newSagaStore()
	wt := "wt_1"
	ec := "AGENT_LAUNCH_FAILED"
	em := "it broke"
	rec, _ := st.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k1", AgentID: "claude", WorktreeID: &wt, Mode: "edit",
		Title: "Fix bug", Name: "Claude: Fix bug", Stage: domain.LaunchFailed,
		ErrorCode: &ec, ErrorMessage: &em, CreatedAt: 100, UpdatedAt: 200,
	})

	res := callStatus(st, rec.ID)
	if !res.Ok {
		t.Fatalf("status should succeed, got %+v", res.Error)
	}
	view, ok := res.Result.(launchView)
	if !ok {
		t.Fatalf("result should be a launchView, got %T", res.Result)
	}
	if view.ID != rec.ID || view.Stage != domain.LaunchFailed || view.Mode != "edit" ||
		view.Title != "Fix bug" || view.AgentID != "claude" || view.WorktreeID != "wt_1" ||
		view.ErrorCode != ec || view.ErrorMessage != em || view.CreatedAt != 100 || view.UpdatedAt != 200 {
		t.Fatalf("launchView fields wrong: %+v", view)
	}
	// Narrow view contract: internal saga plumbing must NOT be serialized.
	blob, _ := json.Marshal(res.Result)
	js := string(blob)
	if strings.Contains(js, "idempotencyKey") || strings.Contains(js, `"name"`) {
		t.Fatalf("launchView leaked internal fields: %s", js)
	}
	if !strings.Contains(js, "stage") {
		t.Fatalf("launchView missing stage: %s", js)
	}
}

func TestStatusToolNotFound(t *testing.T) {
	res := callStatus(newSagaStore(), "agt_deadbeef")
	if res.Ok || res.Error == nil || res.Error.Code != codeLaunchNotFound {
		t.Fatalf("expected LAUNCH_NOT_FOUND, got %+v", res)
	}
	if res.Error.Recoverable {
		t.Fatalf("not-found should be unrecoverable")
	}
}

func TestStatusToolEmptyID(t *testing.T) {
	for _, id := range []string{"", "   "} {
		res := callStatus(newSagaStore(), id)
		if res.Ok || res.Error == nil || res.Error.Code != codeInvalidArgs {
			t.Fatalf("empty/blank launchId %q should be INVALID_ARGS, got %+v", id, res)
		}
	}
}

func TestStatusToolStoreError(t *testing.T) {
	db := errReadStore{sagaStore: newSagaStore(), err: errors.New("disk gone")}
	res := callStatus(db, "agt_x")
	if res.Ok || res.Error == nil || res.Error.Code != domain.CodeInternal {
		t.Fatalf("store error should be internal_error, got %+v", res)
	}
}

func TestStatusArgsValidate(t *testing.T) {
	if (&statusArgs{LaunchID: "agt_ok"}).Validate() != nil {
		t.Fatalf("non-empty launchId should validate")
	}
	if (&statusArgs{LaunchID: "  "}).Validate() == nil {
		t.Fatalf("blank launchId must be rejected by Validate")
	}
}

func TestListToolReturnsRecordsNewestFirst(t *testing.T) {
	st := newSagaStore()
	var lastID string
	for _, key := range []string{"k1", "k2", "k3"} {
		rec, _ := st.InsertAgentLaunch(domain.AgentLaunchRecord{
			IdempotencyKey: key, AgentID: "c", Mode: "edit", Title: key, Name: "n",
		})
		lastID = rec.ID
	}

	res := callList(st)
	if !res.Ok {
		t.Fatalf("list should succeed, got %+v", res.Error)
	}
	payload, ok := res.Result.(map[string]any)
	if !ok {
		t.Fatalf("result should be a map, got %T", res.Result)
	}
	views, ok := payload["launches"].([]launchView)
	if !ok {
		t.Fatalf("launches should be []launchView, got %T", payload["launches"])
	}
	if len(views) != 3 {
		t.Fatalf("want 3 launches, got %d", len(views))
	}
	// Newest-first: the last-inserted record leads.
	if views[0].ID != lastID || views[0].Title != "k3" {
		t.Fatalf("newest launch should lead, got %+v", views[0])
	}
}

func TestListToolEmptyReturnsArrayNotNil(t *testing.T) {
	res := callList(newSagaStore())
	if !res.Ok {
		t.Fatalf("empty list should succeed, got %+v", res.Error)
	}
	payload := res.Result.(map[string]any)
	views, ok := payload["launches"].([]launchView)
	if !ok {
		t.Fatalf("launches should be []launchView, got %T", payload["launches"])
	}
	if views == nil {
		t.Fatalf("launches must be a non-nil empty slice, got nil")
	}
	if len(views) != 0 {
		t.Fatalf("empty store should yield 0 launches, got %d", len(views))
	}
}

func TestListToolStoreError(t *testing.T) {
	db := errReadStore{sagaStore: newSagaStore(), err: errors.New("boom")}
	res := callList(db)
	if res.Ok || res.Error == nil || res.Error.Code != domain.CodeInternal {
		t.Fatalf("store error should be internal_error, got %+v", res)
	}
}

func TestReadToolsAreRiskRead(t *testing.T) {
	deps := Deps{DB: newSagaStore()}
	if newStatusTool(deps).Risk != domain.RiskRead {
		t.Errorf("agentTask.status must be RiskRead, got %s", newStatusTool(deps).Risk)
	}
	if newListTool(deps).Risk != domain.RiskRead {
		t.Errorf("agentTask.list must be RiskRead, got %s", newListTool(deps).Risk)
	}
}

func TestAgentTaskToolsIncludesReaders(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range Tools(Deps{DB: newSagaStore()}) {
		names[tl.Name] = true
	}
	for _, want := range []string{"agentTask.spawnForEdits", "agentTask.status", "agentTask.list"} {
		if !names[want] {
			t.Errorf("Tools() missing %s", want)
		}
	}
}
