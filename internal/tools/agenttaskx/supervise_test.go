package agenttaskx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

func callSupervise(deps Deps, a superviseArgs) tools.ToolResult {
	tool := newSuperviseTerminalTool(deps)
	raw, _ := json.Marshal(a)
	return tool.Handle(context.Background(), raw, nil)
}

func TestSuperviseToolIsRiskTerminal(t *testing.T) {
	if got := newSuperviseTerminalTool(Deps{DB: newSagaStore()}).Risk; got != domain.RiskTerminal {
		t.Fatalf("agentTask.superviseTerminal must be RiskTerminal, got %s", got)
	}
}

func TestSuperviseToolRegistered(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range Tools(Deps{DB: newSagaStore()}) {
		names[tl.Name] = true
	}
	if !names["agentTask.superviseTerminal"] {
		t.Fatalf("Tools() must include agentTask.superviseTerminal")
	}
}

func TestSuperviseTerminalHappyPath(t *testing.T) {
	st := newSagaStore()
	deps := Deps{DB: st, DaemonActive: func() bool { return true }}

	res := callSupervise(deps, superviseArgs{
		TerminalID: "term_7", Title: "Fix OAuth", AcceptanceCriteria: "tests pass", WorktreeID: "wt-1",
	})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["terminalId"] != "term_7" || m["watcherId"] == nil || m["watcherId"] == "" {
		t.Fatalf("result missing terminalId/watcherId: %+v", m)
	}
	if !strings.Contains(res.Summary, "foreground-only") {
		t.Fatalf("summary must carry the foreground-only note, got %q", res.Summary)
	}
	// Adoption must NOT write an agent_launch saga.
	if len(st.launches) != 0 {
		t.Fatalf("superviseTerminal must not insert an agent_launch saga, got %d", len(st.launches))
	}
	// Exactly one supervisor watcher, targeting the terminal, marked adopted.
	if len(st.watchers) != 1 {
		t.Fatalf("want exactly 1 watcher inserted, got %d", len(st.watchers))
	}
	w := st.watchers[0]
	if w.IsSupervisor == nil || !*w.IsSupervisor || !watcherTargets(&w, "term_7") {
		t.Fatalf("watcher should be a supervisor targeting term_7: %+v", w)
	}
	var opts map[string]any
	if w.OptionsJson == nil || json.Unmarshal([]byte(*w.OptionsJson), &opts) != nil {
		t.Fatalf("watcher options should decode, got %v", w.OptionsJson)
	}
	if opts["adoptMode"] != true {
		t.Fatalf("adopted watcher must carry adoptMode:true, got %v", opts["adoptMode"])
	}
	if opts["acceptanceCriteria"] != "tests pass" {
		t.Fatalf("acceptanceCriteria should persist, got %v", opts["acceptanceCriteria"])
	}
}

func TestSuperviseTerminalDedupesExistingSupervisor(t *testing.T) {
	st := newSagaStore()
	deps := Deps{DB: st, DaemonActive: func() bool { return true }}
	// Seed an active supervisor already watching term_7.
	existing, _ := st.InsertWatcher(domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
		TerminalID: "term_7", Title: "watch existing", Goal: "g", CadenceMs: 3000, SpawnMode: "edit",
	}))

	res := callSupervise(deps, superviseArgs{TerminalID: "term_7"})
	if !res.Ok {
		t.Fatalf("dedup path should still succeed, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["deduped"] != true || m["watcherId"] != existing.ID {
		t.Fatalf("should reuse the existing watcher, got %+v", m)
	}
	// No second watcher inserted.
	if len(st.watchers) != 1 {
		t.Fatalf("dedup must not insert a second watcher, got %d", len(st.watchers))
	}
}

func TestSuperviseTerminalInsertFailure(t *testing.T) {
	st := newSagaStore()
	st.insertWatcherErr = errBoom("disk full")
	res := callSupervise(Deps{DB: st, DaemonActive: func() bool { return true }}, superviseArgs{TerminalID: "term_7"})
	if res.Ok || res.Error == nil || res.Error.Code != domain.CodeInternal {
		t.Fatalf("insert failure should be a CodeInternal Fail, got %+v", res)
	}
}

func TestSuperviseTerminalNoSchedulerNote(t *testing.T) {
	st := newSagaStore()
	res := callSupervise(Deps{DB: st, DaemonActive: func() bool { return false }}, superviseArgs{TerminalID: "term_7"})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "no scheduler is running") {
		t.Fatalf("summary should flag the inactive scheduler, got %q", res.Summary)
	}
}

func TestSuperviseArgsValidate(t *testing.T) {
	if (&superviseArgs{TerminalID: "term_1"}).Validate() != nil {
		t.Fatalf("a non-empty terminalId should validate")
	}
	if (&superviseArgs{TerminalID: "  "}).Validate() == nil {
		t.Fatalf("blank terminalId must be rejected")
	}
	if (&superviseArgs{TerminalID: "t", SpawnMode: "bogus"}).Validate() == nil {
		t.Fatalf("an unknown spawnMode must be rejected")
	}
	if (&superviseArgs{TerminalID: "t", SpawnMode: "explore"}).Validate() != nil {
		t.Fatalf("explore is a valid spawnMode")
	}
}
