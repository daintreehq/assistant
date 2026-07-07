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

// recordLimitStore captures the limit agentTask.list passes to the store, so a
// drift between listDefaultLimit and the actual store call is caught.
type recordLimitStore struct {
	*sagaStore
	gotLimit int
}

func (r *recordLimitStore) ListAgentLaunches(limit int) ([]domain.AgentLaunchRecord, error) {
	r.gotLimit = limit
	return r.sagaStore.ListAgentLaunches(limit)
}

func TestListToolCallsStoreWithDefaultLimit(t *testing.T) {
	rec := &recordLimitStore{sagaStore: newSagaStore()}
	if res := callList(rec); !res.Ok {
		t.Fatalf("list should succeed, got %+v", res.Error)
	}
	if rec.gotLimit != listDefaultLimit {
		t.Fatalf("list should call the store with limit=%d, got %d", listDefaultLimit, rec.gotLimit)
	}
}

// TestStatusToolAllNilOptionals proves the nil-pointer derefs in toLaunchView are
// safe and that omitempty drops the absent optional fields from the JSON payload.
func TestStatusToolAllNilOptionals(t *testing.T) {
	st := newSagaStore()
	rec, _ := st.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k", AgentID: "claude", Mode: "explore", Title: "look",
		Name: "n", Stage: domain.LaunchRequested, CreatedAt: 10, UpdatedAt: 10,
		// WorktreeID / TerminalID / WatcherID / ErrorCode / ErrorMessage all nil.
	})
	res := callStatus(st, rec.ID)
	if !res.Ok {
		t.Fatalf("status should succeed, got %+v", res.Error)
	}
	view := res.Result.(launchView)
	if view.WorktreeID != "" || view.TerminalID != "" || view.WatcherID != "" ||
		view.ErrorCode != "" || view.ErrorMessage != "" {
		t.Fatalf("nil optionals should deref to empty strings, got %+v", view)
	}
	js := string(mustJSON(t, res.Result))
	for _, key := range []string{"worktreeId", "terminalId", "watcherId", "errorCode", "errorMessage"} {
		if strings.Contains(js, key) {
			t.Fatalf("omitempty should drop the nil optional %q, got %s", key, js)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// The default "session" scope hides launches created before this session booted
// (stale cross-session rows carry colliding titles + dead terminal ids), while
// naming the hidden remainder so a history question isn't answered with a silent
// "none". scope:"all" restores the full window.
func TestListToolSessionScopeFiltersOldLaunches(t *testing.T) {
	st := newSagaStore()
	_, _ = st.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "old", AgentID: "codex", Mode: "explore", Title: "fact: Codex",
		Name: "n", CreatedAt: 100,
	})
	fresh, _ := st.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "new", AgentID: "codex", Mode: "explore", Title: "Fact: Codex",
		Name: "n", CreatedAt: 900,
	})

	tool := newListTool(Deps{DB: st, SessionStartedAt: 500})
	res := tool.Handle(context.Background(), json.RawMessage(`{}`), nil)
	if !res.Ok {
		t.Fatalf("list should succeed, got %+v", res.Error)
	}
	payload := res.Result.(map[string]any)
	views := payload["launches"].([]launchView)
	if len(views) != 1 || views[0].ID != fresh.ID {
		t.Fatalf("session scope should keep only the fresh launch, got %+v", views)
	}
	if payload["scope"] != "session" {
		t.Fatalf("result should echo the effective scope, got %v", payload["scope"])
	}
	if !strings.Contains(res.Summary, "this session") || !strings.Contains(res.Summary, `scope:"all"`) {
		t.Fatalf("summary must scope the count and name the hidden remainder, got %q", res.Summary)
	}

	// scope:"all" restores the cross-session window.
	resAll := tool.Handle(context.Background(), json.RawMessage(`{"scope":"all"}`), nil)
	if got := len(resAll.Result.(map[string]any)["launches"].([]launchView)); got != 2 {
		t.Fatalf("scope all should list both launches, got %d", got)
	}
}

// SessionStartedAt=0 (unwired) disables the filter so tests / stripped builds
// keep the old behaviour, and an invalid scope fails at the Decode gate.
func TestListToolScopeFallbacksAndValidation(t *testing.T) {
	st := newSagaStore()
	_, _ = st.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "old", AgentID: "c", Mode: "edit", Title: "t", Name: "n", CreatedAt: 100,
	})
	res := callList(st) // Deps.SessionStartedAt zero
	if got := len(res.Result.(map[string]any)["launches"].([]launchView)); got != 1 {
		t.Fatalf("an unwired SessionStartedAt must not hide rows, got %d", got)
	}
	if (&listArgs{Scope: "bogus"}).Validate() == nil {
		t.Fatal("an unknown scope must be rejected by Validate")
	}
	if (&listArgs{}).Validate() != nil || (&listArgs{Scope: "all"}).Validate() != nil {
		t.Fatal("blank and \"all\" scopes must validate")
	}
	// The rejection must fire through the real Decode gate (strict decoding +
	// Validator), not only when Validate is called by hand.
	tool := newListTool(Deps{DB: st})
	if _, err := tool.Decode(json.RawMessage(`{"scope":"bogus"}`)); err == nil {
		t.Fatal("Decode must reject an unknown scope")
	}
	if _, err := tool.Decode(json.RawMessage(`{"scope":"all"}`)); err != nil {
		t.Fatalf("Decode must accept scope all: %v", err)
	}
}
