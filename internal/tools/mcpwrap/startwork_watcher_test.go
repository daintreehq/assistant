package mcpwrap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// watcherStore captures inserted watchers and serves a configurable active list.
type watcherStore struct {
	inserted []domain.WatcherRecord
	active   []domain.WatcherRecord
}

func (s *watcherStore) InsertWatcher(_ context.Context, rec domain.WatcherRecord) (domain.WatcherRecord, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWatcher)
	}
	s.inserted = append(s.inserted, rec)
	return rec, nil
}
func (s *watcherStore) ListWatchers(_ context.Context, _ string) ([]domain.WatcherRecord, error) {
	return s.active, nil
}

// wfStore captures inserted workflow ledger rows and any follow-up patches.
type wfStore struct {
	inserted []domain.WorkflowRunRecord
	patches  map[string]map[string]any
}

func newWFStore() *wfStore { return &wfStore{patches: map[string]map[string]any{}} }

func (s *wfStore) InsertWorkflowRun(_ context.Context, rec domain.WorkflowRunRecord) (string, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflow)
	}
	s.inserted = append(s.inserted, rec)
	return rec.ID, nil
}

func (s *wfStore) UpdateWorkflowRun(_ context.Context, id string, patch map[string]any) error {
	s.patches[id] = patch
	return nil
}

// launchResult builds a startWorkOnIssue passthrough result whose
// structuredContent carries the spawned terminalId.
func launchResult(structured map[string]any) tools.MCPCallResult {
	return tools.MCPCallResult{StructuredContent: structured}
}

func startWorkTool(deps Deps) *tools.Tool {
	return findTool(Tools(deps), "workflow.startWorkOnIssue")
}

func runStartWork(t *testing.T, deps Deps, m *fakeMCP, args string) tools.ToolResult {
	t.Helper()
	tool := startWorkTool(deps)
	parsed, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return tool.Handle(context.Background(), parsed, ctxWith(m))
}

// Attaches exactly ONE supervisor watcher on the launched terminal, carrying the
// edit spawnMode + a worktree-scoped verification, and never forwards attachWatcher.
func TestStartWorkAttachesOneSupervisorWatcher(t *testing.T) {
	m := &fakeMCP{connected: true, result: launchResult(map[string]any{
		"terminalId": "term_1", "worktreeId": "wt-1", "issueTitle": "Fix login",
	})}
	st := &watcherStore{}
	res := runStartWork(t, Deps{Store: st}, m, `{"arguments":{"issueNumber":7}}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.inserted) != 1 {
		t.Fatalf("expected exactly one watcher, got %d", len(st.inserted))
	}
	w := st.inserted[0]
	if w.IsSupervisor == nil || !*w.IsSupervisor {
		t.Fatal("expected a supervisor watcher")
	}
	if w.CadenceMs != supervisorDefaultCadenceMs {
		t.Fatalf("cadence: %d", w.CadenceMs)
	}
	if !watcherTargets(&w, "term_1") {
		t.Fatalf("watcher should target term_1: %s", w.TargetsJson)
	}
	var opts map[string]any
	_ = json.Unmarshal([]byte(*w.OptionsJson), &opts)
	if opts["spawnMode"] != "edit" {
		t.Fatalf("spawnMode: %v", opts["spawnMode"])
	}
	if _, leaked := m.lastArgs["attachWatcher"]; leaked {
		t.Fatal("attachWatcher leaked to Daintree")
	}
}

// A successful startWorkOnIssue records the work in the durable ledger: an active
// row carrying the issue fields + terminal, the watcher back-links it, and the row
// records the watcher id.
func TestStartWorkPopulatesWorkflowLedger(t *testing.T) {
	m := &fakeMCP{connected: true, result: launchResult(map[string]any{
		"terminalId": "term_1", "worktreeId": "wt-1", "issueTitle": "Fix login",
		"issueNumber": float64(7), "issueUrl": "https://example/issues/7", "branch": "feature/fix-login",
	})}
	st := &watcherStore{}
	wf := newWFStore()
	res := runStartWork(t, Deps{Store: st, WorkflowStore: wf}, m, `{"arguments":{"issueNumber":7}}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(wf.inserted) != 1 {
		t.Fatalf("expected exactly one ledger row, got %d", len(wf.inserted))
	}
	run := wf.inserted[0]
	if run.Status != domain.WorkflowActive {
		t.Fatalf("ledger status: %q want active", run.Status)
	}
	if run.IssueTitle == nil || *run.IssueTitle != "Fix login" ||
		run.IssueURL == nil || *run.IssueURL != "https://example/issues/7" ||
		run.Branch == nil || *run.Branch != "feature/fix-login" {
		t.Fatalf("issue fields not seeded: %+v", run)
	}
	if run.IssueNumber == nil || *run.IssueNumber != 7 {
		t.Fatalf("issueNumber: %v", run.IssueNumber)
	}
	if run.TerminalIdsJson == nil || *run.TerminalIdsJson != `["term_1"]` {
		t.Fatalf("terminalIdsJson: %v", run.TerminalIdsJson)
	}
	w := st.inserted[0]
	if w.WorkflowRunID == nil || *w.WorkflowRunID != run.ID {
		t.Fatalf("watcher must back-link run %s, got %v", run.ID, w.WorkflowRunID)
	}
	if p := wf.patches[run.ID]; p == nil || p["watcherIdsJson"] != `["`+w.ID+`"]` {
		t.Fatalf("ledger row should record the watcher id, got %v", wf.patches[run.ID])
	}
}

// A dedup (an active supervisor already on the terminal) records NO new ledger row
// — the row already exists from the prior setup.
func TestStartWorkDedupSkipsLedger(t *testing.T) {
	isSup := true
	targets, _ := json.Marshal([]string{"term_1"})
	st := &watcherStore{active: []domain.WatcherRecord{
		{ID: "wch_old", IsSupervisor: &isSup, TargetsJson: string(targets), Status: "active"},
	}}
	wf := newWFStore()
	m := &fakeMCP{connected: true, result: launchResult(map[string]any{"terminalId": "term_1"})}
	if res := runStartWork(t, Deps{Store: st, WorkflowStore: wf}, m, `{"arguments":{"issueNumber":7}}`); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(wf.inserted) != 0 {
		t.Fatalf("dedup must not insert a ledger row, got %d", len(wf.inserted))
	}
}

// attachWatcher:false skips attachment entirely.
func TestStartWorkAttachWatcherFalseSkips(t *testing.T) {
	m := &fakeMCP{connected: true, result: launchResult(map[string]any{"terminalId": "term_1"})}
	st := &watcherStore{}
	res := runStartWork(t, Deps{Store: st}, m, `{"arguments":{"issueNumber":7},"attachWatcher":false}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if len(st.inserted) != 0 {
		t.Fatalf("attachWatcher:false must skip, inserted %d", len(st.inserted))
	}
}

// A null/absent/whitespace terminalId skips attachment (nothing to supervise).
func TestStartWorkSkipsAttachWhenNoTerminal(t *testing.T) {
	for _, sc := range []map[string]any{
		{},                     // absent
		{"terminalId": ""},     // empty
		{"worktreeId": "wt-1"}, // other fields, no terminalId
	} {
		m := &fakeMCP{connected: true, result: launchResult(sc)}
		st := &watcherStore{}
		res := runStartWork(t, Deps{Store: st}, m, `{"arguments":{"issueNumber":7}}`)
		if !res.Ok {
			t.Fatalf("expected ok for sc=%v, got %+v", sc, res.Error)
		}
		if len(st.inserted) != 0 {
			t.Fatalf("no terminal should mean no watcher for sc=%v, inserted %d", sc, len(st.inserted))
		}
	}
}

// A passthrough failure attaches nothing.
func TestStartWorkPassthroughFailureAttachesNothing(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "no worktree"}}
	st := &watcherStore{}
	res := runStartWork(t, Deps{Store: st}, m, `{"arguments":{"issueNumber":7}}`)
	if res.Ok {
		t.Fatalf("expected failure, got ok")
	}
	if len(st.inserted) != 0 {
		t.Fatalf("failed passthrough must attach nothing, inserted %d", len(st.inserted))
	}
}

// No duplicate when an active supervisor already targets the terminal; a
// cancelled supervisor (absent from the active list) does NOT suppress a fresh attach.
func TestStartWorkNoDupButCancelledDoesNotSuppress(t *testing.T) {
	isSup := true
	targets, _ := json.Marshal([]string{"term_1"})

	// Active supervisor already on term_1 → no duplicate.
	dupStore := &watcherStore{active: []domain.WatcherRecord{
		{ID: "wch_old", IsSupervisor: &isSup, TargetsJson: string(targets), Status: "active"},
	}}
	m := &fakeMCP{connected: true, result: launchResult(map[string]any{"terminalId": "term_1"})}
	if res := runStartWork(t, Deps{Store: dupStore}, m, `{"arguments":{"issueNumber":7}}`); !res.Ok {
		t.Fatalf("dup case: %+v", res.Error)
	}
	if len(dupStore.inserted) != 0 {
		t.Fatalf("active supervisor on terminal should suppress a dup, inserted %d", len(dupStore.inserted))
	}

	// A cancelled supervisor is not in the active list, so a fresh attach proceeds.
	freshStore := &watcherStore{active: nil}
	m2 := &fakeMCP{connected: true, result: launchResult(map[string]any{"terminalId": "term_1"})}
	if res := runStartWork(t, Deps{Store: freshStore}, m2, `{"arguments":{"issueNumber":7}}`); !res.Ok {
		t.Fatalf("fresh case: %+v", res.Error)
	}
	if len(freshStore.inserted) != 1 {
		t.Fatalf("cancelled supervisor must not suppress a fresh attach, inserted %d", len(freshStore.inserted))
	}
}
