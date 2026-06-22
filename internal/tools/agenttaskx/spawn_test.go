package agenttaskx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// --- fakes ------------------------------------------------------------------

type recordedCall struct {
	name string
	args map[string]any
}

// scriptMCP is a scriptable MCP: per-tool result, an optional agent.launch
// throw, and a recorded call log. It also runs an optional hook on agent.launch
// (used to model an abort torn mid-launch).
type scriptMCP struct {
	connected    bool
	launchResult MCPCallResult
	launchThrows bool
	launchErr    error
	listResult   MCPCallResult
	onLaunch     func()
	calls        []recordedCall
}

func (m *scriptMCP) Connected() bool { return m.connected }

func (m *scriptMCP) CallTool(_ context.Context, name string, args map[string]any) (MCPCallResult, error) {
	m.calls = append(m.calls, recordedCall{name: name, args: args})
	switch name {
	case "agent.launch":
		if m.onLaunch != nil {
			m.onLaunch()
		}
		if m.launchThrows {
			if m.launchErr != nil {
				return MCPCallResult{}, m.launchErr
			}
			return MCPCallResult{}, errBoom("connection reset")
		}
		return m.launchResult, nil
	case "terminal.list":
		return m.listResult, nil
	}
	return MCPCallResult{}, nil
}

type errBoom string

func (e errBoom) Error() string { return string(e) }

func (m *scriptMCP) launchCount() int {
	n := 0
	for _, c := range m.calls {
		if c.name == "agent.launch" {
			n++
		}
	}
	return n
}

func (m *scriptMCP) called(name string) bool {
	for _, c := range m.calls {
		if c.name == name {
			return true
		}
	}
	return false
}

func (m *scriptMCP) lastLaunchArgs() map[string]any {
	for i := len(m.calls) - 1; i >= 0; i-- {
		if m.calls[i].name == "agent.launch" {
			return m.calls[i].args
		}
	}
	return nil
}

// sagaStore is a stateful in-memory Store reproducing the saga semantics:
// FindActiveAgentLaunch returns the newest NON-terminal row for a key; inserts
// and patches mutate in place. insertWatcherErr forces a watcher-attach failure.
type sagaStore struct {
	launches         map[string]*domain.AgentLaunchRecord
	order            []string
	watchers         []domain.WatcherRecord
	insertWatcherErr error
	nextID           int
}

func newSagaStore() *sagaStore { return &sagaStore{launches: map[string]*domain.AgentLaunchRecord{}} }

func (s *sagaStore) InsertWatcher(rec domain.WatcherRecord) (domain.WatcherRecord, error) {
	if s.insertWatcherErr != nil {
		return domain.WatcherRecord{}, s.insertWatcherErr
	}
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWatcher)
	}
	s.watchers = append(s.watchers, rec)
	return rec, nil
}

func (s *sagaStore) FindActiveAgentLaunch(key string) (*domain.AgentLaunchRecord, error) {
	terminal := map[domain.AgentLaunchStage]bool{
		domain.LaunchConfirmed: true, domain.LaunchFailed: true,
	}
	for i := len(s.order) - 1; i >= 0; i-- {
		r := s.launches[s.order[i]]
		if r.IdempotencyKey == key && !terminal[r.Stage] {
			return r, nil
		}
	}
	return nil, nil
}

func (s *sagaStore) InsertAgentLaunch(rec domain.AgentLaunchRecord) (domain.AgentLaunchRecord, error) {
	s.nextID++
	rec.ID = domain.NewID(domain.PrefixAgentLaunch)
	cp := rec
	s.launches[rec.ID] = &cp
	s.order = append(s.order, rec.ID)
	return cp, nil
}

func (s *sagaStore) UpdateAgentLaunch(id string, patch map[string]any) error {
	r, ok := s.launches[id]
	if !ok {
		return nil
	}
	if v, ok := patch["stage"].(string); ok {
		r.Stage = domain.AgentLaunchStage(v)
	}
	if v, ok := patch["terminalId"].(string); ok {
		r.TerminalID = &v
	}
	if v, ok := patch["watcherId"].(string); ok {
		r.WatcherID = &v
	}
	return nil
}

func (s *sagaStore) get(id string) *domain.AgentLaunchRecord { return s.launches[id] }

func launchOK(terminalID string) MCPCallResult {
	return MCPCallResult{StructuredContent: map[string]any{"terminalId": terminalID, "location": "grid"}}
}

func launchNoTerminal() MCPCallResult {
	return MCPCallResult{StructuredContent: map[string]any{"location": "grid"}}
}

func terminalListResult(entries ...map[string]any) MCPCallResult {
	arr := make([]any, len(entries))
	for i, e := range entries {
		arr[i] = e
	}
	return MCPCallResult{StructuredContent: map[string]any{"terminals": arr}}
}

func runSpawn(deps Deps, a spawnArgs) tools.ToolResult {
	return spawn(context.Background(), deps, &a)
}

func baseSpawn() spawnArgs {
	return spawnArgs{Title: "Fix OAuth", TaskPrompt: "go"}
}

// --- spawn happy path -------------------------------------------------------

func TestSpawnReadsTerminalIDAndAttachesWatcher(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_9")}
	st := newSagaStore()
	deps := Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}

	a := baseSpawn()
	a.Title = "Fix OAuth callback"
	a.TaskPrompt = "Repair the OAuth callback handler."
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(deps, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["terminalId"] != "term_9" {
		t.Fatalf("terminalId: %v", m["terminalId"])
	}
	if m["watcherId"] == nil || m["watcherId"] == "" {
		t.Fatal("expected a watcherId")
	}
	launchID := m["launchId"].(string)
	if !strings.HasPrefix(launchID, domain.PrefixAgentLaunch) {
		t.Fatalf("launchId prefix: %q", launchID)
	}
	rec := st.get(launchID)
	if rec.Stage != domain.LaunchConfirmed {
		t.Fatalf("expected confirmed stage, got %s", rec.Stage)
	}
	if rec.TerminalID == nil || *rec.TerminalID != "term_9" {
		t.Fatalf("record terminalId: %v", rec.TerminalID)
	}
	// agent.launch carried the constraints block, a deterministic requestKey and
	// a "Claude: <title>" name.
	args := mcp.lastLaunchArgs()
	if !strings.Contains(args["prompt"].(string), "only in this worktree") {
		t.Fatal("edit constraints block missing from prompt")
	}
	if _, ok := args["requestKey"].(string); !ok {
		t.Fatal("requestKey missing")
	}
	if args["name"] != "Claude: Fix OAuth callback" {
		t.Fatalf("name: %v", args["name"])
	}
}

func TestSpawnAmbiguousWhenNoTerminalID(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchNoTerminal(), listResult: terminalListResult()}
	st := newSagaStore()
	deps := Deps{MCP: mcp, DB: st}
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(deps, a)
	if res.Ok || res.Error.Code != codeAgentLaunchAmbiguous || !res.Error.Recoverable {
		t.Fatalf("expected recoverable AGENT_LAUNCH_AMBIGUOUS, got %+v", res)
	}
	details := res.Error.Details.(map[string]any)
	launchID := details["launchId"].(string)
	if st.get(launchID).Stage != domain.LaunchAmbiguous {
		t.Fatalf("record not parked ambiguous: %s", st.get(launchID).Stage)
	}
	if !mcp.called("terminal.list") {
		t.Fatal("expected a reconciliation read")
	}
}

func TestSpawnReconcilesViaTerminalListNameMatch(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchNoTerminal(),
		listResult: terminalListResult(
			map[string]any{"id": "term_42", "name": "Claude: Fix OAuth", "agentId": "claude", "worktreeId": "wt-1"},
			map[string]any{"id": "term_other", "name": "Codex: something else"},
		)}
	st := newSagaStore()
	deps := Deps{MCP: mcp, DB: st}
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(deps, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["terminalId"] != "term_42" {
		t.Fatalf("terminalId: %v", m["terminalId"])
	}
	if st.get(m["launchId"].(string)).Stage != domain.LaunchConfirmed {
		t.Fatal("expected confirmed after reconcile")
	}
}

func TestSpawnIdempotentRetryReconcilesWithoutRelaunch(t *testing.T) {
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	// First attempt: no terminalId, empty inventory → ambiguous, record parked.
	first := &scriptMCP{connected: true, launchResult: launchNoTerminal(), listResult: terminalListResult()}
	r1 := runSpawn(Deps{MCP: first, DB: st}, a)
	if r1.Ok || r1.Error.Code != codeAgentLaunchAmbiguous {
		t.Fatalf("first should be ambiguous, got %+v", r1)
	}

	// Second attempt: agent has appeared; the retry binds WITHOUT relaunching.
	second := &scriptMCP{connected: true, launchResult: launchOK("should_not_be_used"),
		listResult: terminalListResult(map[string]any{"id": "term_77", "name": "Claude: Fix OAuth", "agentId": "claude"})}
	r2 := runSpawn(Deps{MCP: second, DB: st}, a)
	if !r2.Ok {
		t.Fatalf("retry should succeed, got %+v", r2.Error)
	}
	if r2.Result.(map[string]any)["terminalId"] != "term_77" {
		t.Fatalf("retry terminalId: %v", r2.Result)
	}
	if second.launchCount() != 0 {
		t.Fatalf("retry must not relaunch, launched %d", second.launchCount())
	}
}

func TestSpawnRefusesBindOnDuplicateName(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchNoTerminal(),
		listResult: terminalListResult(
			map[string]any{"id": "term_a", "name": "Claude: Fix OAuth"},
			map[string]any{"id": "term_b", "name": "Claude: Fix OAuth"},
		)}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore()}, baseSpawn())
	if res.Ok || res.Error.Code != codeAgentLaunchAmbiguous {
		t.Fatalf("multi-match must stay ambiguous, got %+v", res)
	}
}

func TestSpawnTransportThrowReconciles(t *testing.T) {
	// Entry keyed by terminalId (not id) exercises the parser fallback.
	mcp := &scriptMCP{connected: true, launchThrows: true,
		listResult: terminalListResult(map[string]any{"terminalId": "term_88", "name": "Claude: Fix OAuth", "agentId": "claude"})}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore()}, baseSpawn())
	if !res.Ok {
		t.Fatalf("expected reconcile ok, got %+v", res.Error)
	}
	if res.Result.(map[string]any)["terminalId"] != "term_88" {
		t.Fatalf("terminalId: %v", res.Result)
	}
}

func TestSpawnTransportThrowAmbiguousWhenNoMatch(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchThrows: true, listResult: terminalListResult()}
	st := newSagaStore()
	res := runSpawn(Deps{MCP: mcp, DB: st}, baseSpawn())
	if res.Ok || res.Error.Code != codeAgentLaunchAmbiguous {
		t.Fatalf("transport throw should be ambiguous, got %+v", res)
	}
	launchID := res.Error.Details.(map[string]any)["launchId"].(string)
	if st.get(launchID).Stage != domain.LaunchAmbiguous {
		t.Fatalf("expected ambiguous stage, got %s", st.get(launchID).Stage)
	}
}

func TestSpawnExplicitErrorIsCleanFailure(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: MCPCallResult{Text: "no worktree available", IsError: true}}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore()}, baseSpawn())
	if res.Ok || res.Error.Code != codeAgentLaunchFailed {
		t.Fatalf("expected AGENT_LAUNCH_FAILED, got %+v", res)
	}
}

func TestSpawnConfirmedDoesNotBlockFreshRun(t *testing.T) {
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"

	first := &scriptMCP{connected: true, launchResult: launchOK("term_a")}
	if r := runSpawn(Deps{MCP: first, DB: st}, a); !r.Ok {
		t.Fatalf("first: %+v", r.Error)
	}
	second := &scriptMCP{connected: true, launchResult: launchOK("term_b")}
	r2 := runSpawn(Deps{MCP: second, DB: st}, a)
	if !r2.Ok || r2.Result.(map[string]any)["terminalId"] != "term_b" {
		t.Fatalf("fresh run after confirm should relaunch: %+v", r2)
	}
	if second.launchCount() != 1 {
		t.Fatalf("expected a fresh launch, got %d", second.launchCount())
	}
}

func TestSpawnAmbiguousDeadlockEscapesWithFreshLaunch(t *testing.T) {
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"

	first := &scriptMCP{connected: true, launchResult: launchNoTerminal(), listResult: terminalListResult()}
	if r := runSpawn(Deps{MCP: first, DB: st}, a); r.Ok {
		t.Fatal("first should be ambiguous")
	}
	// Retry, inventory still empty: dead-end record retired, fresh launch proceeds.
	second := &scriptMCP{connected: true, launchResult: launchOK("term_fresh"), listResult: terminalListResult()}
	r2 := runSpawn(Deps{MCP: second, DB: st}, a)
	if !r2.Ok || r2.Result.(map[string]any)["terminalId"] != "term_fresh" {
		t.Fatalf("retry should launch fresh: %+v", r2)
	}
	if second.launchCount() != 1 {
		t.Fatalf("expected a fresh launch, got %d", second.launchCount())
	}
}

func TestSpawnOkWhenWatcherAttachThrows(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	st := newSagaStore()
	st.insertWatcherErr = errBoom("disk full")
	deps := Deps{MCP: mcp, DB: st}
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(deps, a)
	if !res.Ok {
		t.Fatalf("launch should stay ok despite watcher failure: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if _, has := m["watcherId"]; has {
		t.Fatal("watcherId should be absent on attach failure")
	}
	if w, _ := m["watcherWarning"].(string); !strings.Contains(w, "could not be attached") {
		t.Fatalf("watcherWarning: %v", m["watcherWarning"])
	}
	// Record stays terminal_bound (recoverable for a re-attach).
	if st.get(m["launchId"].(string)).Stage != domain.TerminalBound {
		t.Fatalf("expected terminal_bound, got %s", st.get(m["launchId"].(string)).Stage)
	}
}

func TestSpawnExploreModeReadOnlyConstraints(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	a := baseSpawn()
	a.Mode = "explore"
	a.WorktreeID = "wt-1"
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore()}, a)
	if !res.Ok {
		t.Fatalf("explore spawn failed: %+v", res.Error)
	}
	prompt := mcp.lastLaunchArgs()["prompt"].(string)
	if !strings.Contains(prompt, "READ-ONLY exploration") {
		t.Fatal("explore prompt missing read-only language")
	}
	if strings.Contains(prompt, "only in this worktree") || strings.Contains(prompt, "changed files") {
		t.Fatal("explore prompt leaked edit-mode language")
	}
}

func TestSpawnMCPDisconnectedFailsClean(t *testing.T) {
	res := runSpawn(Deps{MCP: &scriptMCP{connected: false}, DB: newSagaStore()}, baseSpawn())
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("expected MCP_UNAVAILABLE, got %+v", res)
	}
}

func TestSpawnDeterministicRequestKey(t *testing.T) {
	a := baseSpawn()
	a.TaskPrompt = "Repair the OAuth callback handler."
	a.WorktreeID = "wt-1"
	mcpA := &scriptMCP{connected: true, launchResult: launchOK("t")}
	mcpB := &scriptMCP{connected: true, launchResult: launchOK("t")}
	_ = runSpawn(Deps{MCP: mcpA, DB: newSagaStore()}, a)
	b := a
	b.Title = "different title" // title is excluded from the identity
	_ = runSpawn(Deps{MCP: mcpB, DB: newSagaStore()}, b)
	keyA := mcpA.lastLaunchArgs()["requestKey"].(string)
	keyB := mcpB.lastLaunchArgs()["requestKey"].(string)
	if keyA != keyB {
		t.Fatalf("requestKey not deterministic across title change: %s vs %s", keyA, keyB)
	}
	if len(keyA) != 16 {
		t.Fatalf("requestKey length %d, want 16", len(keyA))
	}
	// A different worktree changes the key.
	c := a
	c.WorktreeID = "wt-2"
	mcpC := &scriptMCP{connected: true, launchResult: launchOK("t")}
	_ = runSpawn(Deps{MCP: mcpC, DB: newSagaStore()}, c)
	if mcpC.lastLaunchArgs()["requestKey"] == keyA {
		t.Fatal("worktree change should change the requestKey")
	}
}

// --- agentTaskWatcher: lifecycle + cadence + spawnMode ----------------------

func TestSpawnWatcherLifecycleNoticeSchedulerRunning(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	a := baseSpawn()
	a.Watcher = &spawnWatcher{Create: true}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore(), DaemonActive: func() bool { return true }}, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "watcher") ||
		!strings.Contains(res.Summary, "discarded when you close the assistant") ||
		!strings.Contains(res.Summary, "does not resume on the next launch") {
		t.Fatalf("lifecycle note missing: %q", res.Summary)
	}
}

func TestSpawnWatcherLifecycleNoticeNoScheduler(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	a := baseSpawn()
	a.Watcher = &spawnWatcher{Create: true}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore(), DaemonActive: func() bool { return false }}, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "no scheduler is running") || !strings.Contains(res.Summary, "will not check") {
		t.Fatalf("no-scheduler note missing: %q", res.Summary)
	}
}

func TestSpawnNoWatcherOmitsLifecycleNote(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	res := runSpawn(Deps{MCP: mcp, DB: newSagaStore(), DaemonActive: func() bool { return true }}, baseSpawn())
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if strings.Contains(res.Summary, "discarded when you close the assistant") {
		t.Fatalf("lifecycle note should be omitted with no watcher: %q", res.Summary)
	}
}

func TestSpawnAttachesFastSupervisorWatcher(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	a.Watcher = &spawnWatcher{Create: true}
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if len(st.watchers) != 1 {
		t.Fatalf("expected 1 watcher, got %d", len(st.watchers))
	}
	w := st.watchers[0]
	if w.CadenceMs != supervisorDefaultCadenceMs {
		t.Fatalf("cadence: got %d want %d", w.CadenceMs, supervisorDefaultCadenceMs)
	}
	if w.IsSupervisor == nil || !*w.IsSupervisor {
		t.Fatal("expected isSupervisor=true")
	}
}

func TestSpawnHonoursExplicitCadenceOverride(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	cad := 30000
	a := baseSpawn()
	a.Watcher = &spawnWatcher{Create: true, CadenceMs: &cad}
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if st.watchers[0].CadenceMs != 30000 {
		t.Fatalf("cadence override not honoured: %d", st.watchers[0].CadenceMs)
	}
}

func TestSpawnRecordsSpawnModeInOptionsJSON(t *testing.T) {
	// edit (default) records spawnMode=edit; worktree present → verificationScope set.
	editMCP := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	editSt := newSagaStore()
	editA := baseSpawn()
	editA.WorktreeID = "wt-1"
	editA.Watcher = &spawnWatcher{Create: true}
	if r := runSpawn(Deps{MCP: editMCP, DB: editSt}, editA); !r.Ok {
		t.Fatalf("edit spawn: %+v", r.Error)
	}
	if got := optString(t, editSt.watchers[0].OptionsJson, "spawnMode"); got != "edit" {
		t.Fatalf("spawnMode: got %q want edit", got)
	}

	// explore records spawnMode=explore; faked launch has no worktree on result so
	// verificationScope stays absent (mode recorded unconditionally).
	expMCP := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	expSt := newSagaStore()
	expA := baseSpawn()
	expA.Mode = "explore"
	expA.Watcher = &spawnWatcher{Create: true}
	if r := runSpawn(Deps{MCP: expMCP, DB: expSt}, expA); !r.Ok {
		t.Fatalf("explore spawn: %+v", r.Error)
	}
	if got := optString(t, expSt.watchers[0].OptionsJson, "spawnMode"); got != "explore" {
		t.Fatalf("spawnMode: got %q want explore", got)
	}
}

func optString(t *testing.T, raw *string, key string) string {
	t.Helper()
	if raw == nil {
		t.Fatal("optionsJson is nil")
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(*raw), &m); err != nil {
		t.Fatalf("optionsJson decode: %v", err)
	}
	s, _ := m[key].(string)
	return s
}

func TestSpawnAppliesNameAndWhitespaceFallbacks(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	a := baseSpawn()
	a.Title = "  Fix\n\nOAuth\t callback  "
	if r := runSpawn(Deps{MCP: mcp, DB: newSagaStore()}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if mcp.lastLaunchArgs()["name"] != "Claude: Fix OAuth callback" {
		t.Fatalf("whitespace not collapsed: %v", mcp.lastLaunchArgs()["name"])
	}

	blankMCP := &scriptMCP{connected: true, launchResult: launchOK("term_y")}
	b := baseSpawn()
	b.Title = "   "
	if r := runSpawn(Deps{MCP: blankMCP, DB: newSagaStore()}, b); !r.Ok {
		t.Fatalf("blank spawn: %+v", r.Error)
	}
	if blankMCP.lastLaunchArgs()["name"] != "Claude: task" {
		t.Fatalf("blank fallback: %v", blankMCP.lastLaunchArgs()["name"])
	}
}
