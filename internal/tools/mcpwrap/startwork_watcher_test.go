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

func (s *watcherStore) InsertWatcher(_ context.Context, rec domain.WatcherRecord) error {
	s.inserted = append(s.inserted, rec)
	return nil
}
func (s *watcherStore) ListWatchers(_ context.Context, _ string) ([]domain.WatcherRecord, error) {
	return s.active, nil
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
