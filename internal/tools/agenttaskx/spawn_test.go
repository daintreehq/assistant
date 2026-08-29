package agenttaskx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
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
	agentRoster  MCPCallResult // agent.listAvailable response; zero value ⇒ fail-open roster
	worktreeList MCPCallResult // worktree.list response; zero value ⇒ fail-open roster
	onLaunch     func()
	calls        []recordedCall
	// listCtxErrs records ctx.Err() at each terminal.list call, so a test can prove
	// a reconcile read ran on a LIVE (detached) ctx even after the turn's ctx died.
	listCtxErrs []error
}

func (m *scriptMCP) Connected() bool { return m.connected }

func (m *scriptMCP) CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error) {
	m.calls = append(m.calls, recordedCall{name: name, args: args})
	if name == "terminal.list" {
		m.listCtxErrs = append(m.listCtxErrs, ctx.Err())
	}
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
	case "agent.listAvailable":
		return m.agentRoster, nil
	case "worktree.list":
		return m.worktreeList, nil
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
// and patches mutate in place. insertWatcherErr forces a watcher-attach failure;
// insertWorkflowErr forces a ledger-insert failure (to prove best-effort).
type sagaStore struct {
	launches          map[string]*domain.AgentLaunchRecord
	order             []string
	watchers          []domain.WatcherRecord
	workflowRuns      map[string]*domain.WorkflowRunRecord
	workflowOrder     []string
	workflowPatches   map[string]map[string]any
	insertWatcherErr  error
	insertWorkflowErr error
	nextID            int
}

func newSagaStore() *sagaStore {
	return &sagaStore{
		launches:        map[string]*domain.AgentLaunchRecord{},
		workflowRuns:    map[string]*domain.WorkflowRunRecord{},
		workflowPatches: map[string]map[string]any{},
	}
}

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

// ListWatchers returns the seeded/inserted watchers matching status (empty status ⇒
// all; an unset record Status is treated as "active", mirroring storage).
func (s *sagaStore) ListWatchers(status string) ([]domain.WatcherRecord, error) {
	var out []domain.WatcherRecord
	for _, w := range s.watchers {
		st := w.Status
		if st == "" {
			st = "active"
		}
		if status == "" || st == status {
			out = append(out, w)
		}
	}
	return out, nil
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
	if v, ok := patch["workflowRunId"].(string); ok {
		r.WorkflowRunID = &v
	}
	return nil
}

// InsertWorkflowRun records a ledger row (assigning a wfr_ id) unless
// insertWorkflowErr is set, which models a best-effort ledger failure.
func (s *sagaStore) InsertWorkflowRun(rec domain.WorkflowRunRecord) (domain.WorkflowRunRecord, error) {
	if s.insertWorkflowErr != nil {
		return domain.WorkflowRunRecord{}, s.insertWorkflowErr
	}
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixWorkflow)
	}
	cp := rec
	s.workflowRuns[rec.ID] = &cp
	s.workflowOrder = append(s.workflowOrder, rec.ID)
	return cp, nil
}

// UpdateWorkflowRun records the patch and applies the columns the tests assert on.
func (s *sagaStore) UpdateWorkflowRun(id string, patch map[string]any) error {
	r, ok := s.workflowRuns[id]
	if !ok {
		return nil
	}
	s.workflowPatches[id] = patch
	if v, ok := patch["status"].(string); ok {
		r.Status = domain.WorkflowRunStatus(v)
	}
	if v, ok := patch["watcherIdsJson"].(string); ok {
		r.WatcherIdsJson = &v
	}
	return nil
}

func (s *sagaStore) get(id string) *domain.AgentLaunchRecord { return s.launches[id] }

// onlyWorkflowRun returns the single ledger row (failing if not exactly one).
func (s *sagaStore) onlyWorkflowRun(t *testing.T) *domain.WorkflowRunRecord {
	t.Helper()
	if len(s.workflowOrder) != 1 {
		t.Fatalf("expected exactly one workflow run, got %d", len(s.workflowOrder))
	}
	return s.workflowRuns[s.workflowOrder[0]]
}

func (s *sagaStore) GetAgentLaunch(id string) (*domain.AgentLaunchRecord, error) {
	r, ok := s.launches[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}

// ListAgentLaunches returns the launches newest-first (reverse insertion order,
// which approximates updatedAt DESC for the test data), bounded by limit.
func (s *sagaStore) ListAgentLaunches(limit int) ([]domain.AgentLaunchRecord, error) {
	if limit <= 0 {
		limit = 20
	}
	var out []domain.AgentLaunchRecord
	for i := len(s.order) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, *s.launches[s.order[i]])
	}
	return out, nil
}

func launchOK(terminalID string) MCPCallResult {
	return MCPCallResult{StructuredContent: map[string]any{"terminalId": terminalID, "location": "grid"}}
}

func launchNoTerminal() MCPCallResult {
	return MCPCallResult{StructuredContent: map[string]any{"location": "grid"}}
}

func launchMissingCLI(terminalID string) MCPCallResult {
	return MCPCallResult{StructuredContent: map[string]any{
		"terminalId": terminalID, "location": "grid", "spawnStatus": "missing-cli",
	}}
}

func terminalListResult(entries ...map[string]any) MCPCallResult {
	arr := make([]any, len(entries))
	for i, e := range entries {
		arr[i] = e
	}
	return MCPCallResult{StructuredContent: map[string]any{"terminals": arr}}
}

func runSpawn(deps Deps, a spawnArgs) tools.ToolResult {
	return spawnMain(context.Background(), deps, &a)
}

func baseSpawn() spawnArgs {
	return spawnArgs{Title: "Fix OAuth", TaskPrompt: "go"}
}

// --- spawn happy path -------------------------------------------------------

// A spawn that names no agent launches the user's configured default, not the built-in
// constant — the whole point of the host reporting one.
func TestSpawnLaunchesTheHostDefaultAgentWhenNoneIsNamed(t *testing.T) {
	mcp := &scriptMCP{
		connected:    true,
		launchResult: launchOK("term_1"),
		agentRoster:  agentRoster("claude", "codex"),
	}
	deps := Deps{
		MCP: mcp, DB: newSagaStore(),
		DaemonActive: func() bool { return true },
		DefaultAgent: func() string { return "codex" },
	}

	a := baseSpawn()
	a.WorktreeID = "wt-1"
	if res := runSpawn(deps, a); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	launch := mcp.lastLaunchArgs()
	if launch["agentId"] != "codex" {
		t.Fatalf("launched agentId = %v, want the host default %q", launch["agentId"], "codex")
	}
	// The tab label and the saga's reconciliation identity derive from the same id.
	if launch["name"] != "Codex: Fix OAuth" {
		t.Fatalf("launch name = %v, want it to name the default agent", launch["name"])
	}
}

// A host that reports no default (older Daintree, or a discovery read that failed open)
// still spawns: the built-in fallback stands.
func TestSpawnFallsBackWhenTheHostReportsNoDefault(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1"), agentRoster: agentRoster("claude")}
	deps := Deps{MCP: mcp, DB: newSagaStore(), DaemonActive: func() bool { return true }}

	a := baseSpawn()
	a.WorktreeID = "wt-1"
	if res := runSpawn(deps, a); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if got := mcp.lastLaunchArgs()["agentId"]; got != fallbackAgentID {
		t.Fatalf("launched agentId = %v, want the built-in fallback %q", got, fallbackAgentID)
	}
}

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

// Issue #337: the model copies the rendered tab label into `title`, so the wrapper
// used to prefix it twice — "Claude: Claude: prs merge target". The redundant label
// is stripped ONCE at the argument, which has to clean every surface the raw title
// reaches, not just the tab: the watcher title and goal (visible in the attention
// queue) and the persisted saga record.
func TestSpawnStripsSelfPrefixedTitleFromEverySurface(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_337")}
	st := newSagaStore()
	deps := Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}

	a := baseSpawn()
	a.Title = "Claude: prs merge target"
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(deps, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if name := mcp.lastLaunchArgs()["name"]; name != "Claude: prs merge target" {
		t.Errorf("launch name = %v, want %q (not doubled)", name, "Claude: prs merge target")
	}
	if rec := st.get(res.Result.(map[string]any)["launchId"].(string)); rec.Title != "prs merge target" {
		t.Errorf("saga record title = %q, want the stripped title", rec.Title)
	}
	if len(st.watchers) != 1 {
		t.Fatalf("expected exactly one watcher, got %d", len(st.watchers))
	}
	if got := st.watchers[0].Title; got != "watch prs merge target" {
		t.Errorf("watcher title = %q, want %q", got, "watch prs merge target")
	}
	if got := st.watchers[0].Goal; got != "Supervise: prs merge target" {
		t.Errorf("watcher goal = %q, want %q", got, "Supervise: prs merge target")
	}
	// The success summary quotes the title back to the user; it must not be the one
	// surface still showing the doubled name.
	if strings.Contains(res.Summary, "Claude: prs merge target") {
		t.Errorf("summary still carries the redundant label: %q", res.Summary)
	}
}

// A title that is NOTHING but the agent label carries no task text. Whatever stands in
// for it has to stand in everywhere: the tab, the saga record and the watcher reading
// three different names for one task is worse than the doubled label this fixes.
func TestSpawnCanonicalizesALabelOnlyTitle(t *testing.T) {
	for _, title := range []string{"Claude:", "claude: claude:"} {
		mcp := &scriptMCP{connected: true, launchResult: launchOK("term_338")}
		st := newSagaStore()
		deps := Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}

		a := baseSpawn()
		a.Title = title
		a.WorktreeID = "wt-1"
		a.Watcher = &spawnWatcher{Create: true}

		res := runSpawn(deps, a)
		if !res.Ok {
			t.Fatalf("%q: expected ok, got %+v", title, res.Error)
		}
		if name := mcp.lastLaunchArgs()["name"]; name != "Claude: task" {
			t.Errorf("%q: launch name = %v, want %q", title, name, "Claude: task")
		}
		if rec := st.get(res.Result.(map[string]any)["launchId"].(string)); rec.Title != "task" {
			t.Errorf("%q: saga title = %q, want %q", title, rec.Title, "task")
		}
		if got := st.watchers[0].Title; got != "watch task" {
			t.Errorf("%q: watcher title = %q, want %q", title, got, "watch task")
		}
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
	// The message carries the circuit-breaker hint so a model that hits this for EVERY spawn
	// stops retrying and reports the blocker instead of improvising shell-command workarounds.
	if !strings.Contains(res.Error.Message, "stop retrying and report it") {
		t.Fatalf("ambiguous message should carry the stop-and-report hint, got %q", res.Error.Message)
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

func TestSpawnRejectsAtomicMissingCLIDiagnostic(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchMissingCLI("diagnostic-panel")}
	st := newSagaStore()
	res := runSpawn(Deps{MCP: mcp, DB: st}, baseSpawn())
	if res.Ok || res.Error.Code != codeAgentUnavailable {
		t.Fatalf("missing-CLI diagnostic must not count as an agent spawn: %+v", res)
	}
	launchID := res.Error.Details.(map[string]any)["launchId"].(string)
	if got := st.get(launchID).Stage; got != domain.LaunchFailed {
		t.Fatalf("diagnostic launch stage = %s, want failed", got)
	}
	if len(st.watchers) != 0 {
		t.Fatalf("diagnostic panel received %d watcher(s)", len(st.watchers))
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

// Regression: a watcher requested via the FLAT fields (not the legacy nested object)
// that fails to attach must ALSO leave the saga terminal_bound — the "settled"
// decision keys off the resolved want-watcher, not a.Watcher. Otherwise a flat-watch
// attach failure finalizes as confirmed and a retry can't re-attach (fresh-launches).
func TestSpawnFlatWatchAttachFailureStaysRecoverable(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	st := newSagaStore()
	st.insertWatcherErr = errBoom("disk full")
	a := baseSpawn()
	yes := true
	a.Watch = &yes
	a.WatchGoal = "surface the answer"

	res := runSpawn(Deps{MCP: mcp, DB: st}, a)
	if !res.Ok {
		t.Fatalf("launch should stay ok despite watcher failure: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if _, has := m["watcherId"]; has {
		t.Fatal("watcherId should be absent on attach failure")
	}
	if st.get(m["launchId"].(string)).Stage != domain.TerminalBound {
		t.Fatalf("flat-watch attach failure must stay terminal_bound, got %s", st.get(m["launchId"].(string)).Stage)
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
	if !strings.Contains(prompt, "read-only task") {
		t.Fatal("explore prompt missing read-only language")
	}
	// The explore constraints must NOT inject a codebase-survey assumption — that
	// contradicts a non-codebase delegated task (see exploreConstraintsBlock).
	if strings.Contains(prompt, "project's structure") || strings.Contains(prompt, "tech debt") {
		t.Fatal("explore prompt leaked a codebase-survey assumption")
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
	// The failure must point at /reconnect — the in-session recovery command that
	// works in both the REPL and the host (issue #211).
	if !strings.Contains(res.Error.Message, "/reconnect") {
		t.Errorf("disconnected spawn hint must name /reconnect: %q", res.Error.Message)
	}
}

func TestSpawnRequestKeyMirrorsForwardedArgs(t *testing.T) {
	a := baseSpawn()
	a.TaskPrompt = "Repair the OAuth callback handler."
	a.WorktreeID = "wt-1"
	// Identical args produce an identical requestKey, so a genuine double-fire dedupes.
	mcpA := &scriptMCP{connected: true, launchResult: launchOK("t")}
	mcpA2 := &scriptMCP{connected: true, launchResult: launchOK("t")}
	_ = runSpawn(Deps{MCP: mcpA, DB: newSagaStore()}, a)
	_ = runSpawn(Deps{MCP: mcpA2, DB: newSagaStore()}, a)
	keyA := mcpA.lastLaunchArgs()["requestKey"].(string)
	if keyA != mcpA2.lastLaunchArgs()["requestKey"].(string) {
		t.Fatal("identical args must produce an identical requestKey")
	}
	if len(keyA) != 16 {
		t.Fatalf("requestKey length %d, want 16", len(keyA))
	}
	// A title change alters the forwarded `name`, so it MUST change the requestKey.
	// The old shape excluded the title and reused the key with a changed name, which
	// tripped Daintree's "same requestKey, different arguments" collision on retry.
	b := a
	b.Title = "different title"
	mcpB := &scriptMCP{connected: true, launchResult: launchOK("t")}
	_ = runSpawn(Deps{MCP: mcpB, DB: newSagaStore()}, b)
	if mcpB.lastLaunchArgs()["requestKey"] == keyA {
		t.Fatal("a title change must change the requestKey (it changes the forwarded name)")
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

// --- flat watcher fields (the taught shape; the nested object is legacy) --------

func TestSpawnFlatWatchFieldsAttachWatcher(t *testing.T) {
	// watch:true + watchGoal as FLAT top-level scalars attach a supervisor watcher —
	// no nested object (the shape the large model mangles into "watcher<...>create").
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	yes := true
	a.Watch = &yes
	a.WatchGoal = "surface the agent's answer"
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if len(st.watchers) != 1 {
		t.Fatalf("flat watch fields must attach a watcher, got %d", len(st.watchers))
	}
	if st.watchers[0].Goal != "surface the agent's answer" {
		t.Fatalf("watchGoal not honoured: %q", st.watchers[0].Goal)
	}
}

func TestSpawnWatchGoalAloneImpliesWatcher(t *testing.T) {
	// Providing watchGoal without an explicit watch:true still attaches a watcher.
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	a.WatchGoal = "watch and report"
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if len(st.watchers) != 1 {
		t.Fatalf("watchGoal alone must attach a watcher, got %d", len(st.watchers))
	}
}

func TestSpawnWatchFalseSuppressesLegacyNestedWatcher(t *testing.T) {
	// An explicit flat watch:false wins over a legacy nested watcher.create:true.
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	no := false
	a.Watch = &no
	a.Watcher = &spawnWatcher{Create: true}
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if len(st.watchers) != 0 {
		t.Fatalf("watch:false must suppress the watcher, got %d", len(st.watchers))
	}
}

func TestSpawnFlatWatchCadenceOverride(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	cad := 45000
	a.WatchGoal = "watch"
	a.WatchCadenceMs = &cad
	if r := runSpawn(Deps{MCP: mcp, DB: st, DaemonActive: func() bool { return true }}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	if st.watchers[0].CadenceMs != 45000 {
		t.Fatalf("flat watchCadenceMs override not honoured: %d", st.watchers[0].CadenceMs)
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

// --- durable workflow ledger (issue #206) -----------------------------------

// A successful spawn-with-watcher records the work in the durable ledger: an
// active row linked to the terminal AND the watcher, the watcher back-links the
// row, the saga carries the runId, and the result surfaces it.
func TestSpawnPopulatesWorkflowLedger(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_9")}
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(Deps{MCP: mcp, DB: st}, a)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	run := st.onlyWorkflowRun(t)
	if run.Status != domain.WorkflowActive {
		t.Fatalf("ledger status: got %q want active", run.Status)
	}
	if run.WorktreeID == nil || *run.WorktreeID != "wt-1" {
		t.Fatalf("ledger worktreeId: %v", run.WorktreeID)
	}
	if run.TerminalIdsJson == nil || *run.TerminalIdsJson != `["term_9"]` {
		t.Fatalf("ledger terminalIdsJson: %v", run.TerminalIdsJson)
	}
	// The watcher's id is recorded on the row, and the watcher back-links the row.
	w := st.watchers[0]
	if run.WatcherIdsJson == nil || *run.WatcherIdsJson != `["`+w.ID+`"]` {
		t.Fatalf("ledger watcherIdsJson: %v (watcher %s)", run.WatcherIdsJson, w.ID)
	}
	if w.WorkflowRunID == nil || *w.WorkflowRunID != run.ID {
		t.Fatalf("watcher must back-link run %s, got %v", run.ID, w.WorkflowRunID)
	}
	// The saga carries the runId (idempotency anchor) and the result surfaces it.
	if rec := st.get(res.Result.(map[string]any)["launchId"].(string)); rec.WorkflowRunID == nil || *rec.WorkflowRunID != run.ID {
		t.Fatalf("saga must carry workflowRunId %s, got %v", run.ID, rec.WorkflowRunID)
	}
	if res.Result.(map[string]any)["workflowRunId"] != run.ID {
		t.Fatalf("result workflowRunId: %v want %s", res.Result.(map[string]any)["workflowRunId"], run.ID)
	}
}

// A spawn WITHOUT a watcher still records the work (the ledger is about the spawn,
// not the supervision) — the row is left active with no watcher link.
func TestSpawnRecordsLedgerWithoutWatcher(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_x")}
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"

	if r := runSpawn(Deps{MCP: mcp, DB: st}, a); !r.Ok {
		t.Fatalf("expected ok, got %+v", r.Error)
	}
	run := st.onlyWorkflowRun(t)
	if run.Status != domain.WorkflowActive || run.WatcherIdsJson != nil {
		t.Fatalf("unsupervised spawn should record an active, watcher-less row: %+v", run)
	}
}

// The ledger insert is best-effort: a failure NEVER fails a successful spawn, and
// no watcher back-link is recorded (the run id is empty).
func TestSpawnOkWhenLedgerInsertFails(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	st := newSagaStore()
	st.insertWorkflowErr = errBoom("disk full")
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	res := runSpawn(Deps{MCP: mcp, DB: st}, a)
	if !res.Ok {
		t.Fatalf("ledger failure must not fail the spawn: %+v", res.Error)
	}
	if len(st.workflowOrder) != 0 {
		t.Fatalf("no ledger row should persist on insert failure, got %d", len(st.workflowOrder))
	}
	// The watcher still attaches, but carries no back-link.
	if len(st.watchers) != 1 || st.watchers[0].WorkflowRunID != nil {
		t.Fatalf("watcher should attach without a back-link, got %+v", st.watchers)
	}
	if _, has := res.Result.(map[string]any)["workflowRunId"]; has {
		t.Fatal("result must omit workflowRunId when the ledger insert failed")
	}
}

// An idempotent retry (saga left terminal_bound after a watcher-attach failure)
// re-uses the SAME ledger row rather than inserting a duplicate.
func TestSpawnLedgerIdempotentOnRetry(t *testing.T) {
	st := newSagaStore()
	a := baseSpawn()
	a.WorktreeID = "wt-1"
	a.Watcher = &spawnWatcher{Create: true}

	// First attempt: ledger row created, but the watcher attach fails → saga parks
	// at terminal_bound carrying the runId.
	st.insertWatcherErr = errBoom("disk full")
	first := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	r1 := runSpawn(Deps{MCP: first, DB: st}, a)
	if !r1.Ok {
		t.Fatalf("first attempt should stay ok: %+v", r1.Error)
	}
	if len(st.workflowOrder) != 1 {
		t.Fatalf("first attempt should create one ledger row, got %d", len(st.workflowOrder))
	}
	runID := st.workflowOrder[0]

	// Second attempt: same task → idempotent hit on the bound terminal; watcher now
	// attaches. The ledger row is re-used (no duplicate) and gains the watcher link.
	st.insertWatcherErr = nil
	second := &scriptMCP{connected: true, launchResult: launchOK("should_not_relaunch")}
	r2 := runSpawn(Deps{MCP: second, DB: st}, a)
	if !r2.Ok {
		t.Fatalf("retry should succeed: %+v", r2.Error)
	}
	if second.launchCount() != 0 {
		t.Fatalf("idempotent retry must not relaunch, launched %d", second.launchCount())
	}
	if len(st.workflowOrder) != 1 || st.workflowOrder[0] != runID {
		t.Fatalf("retry must re-use the ledger row, got %v", st.workflowOrder)
	}
	if st.workflowRuns[runID].WatcherIdsJson == nil {
		t.Fatal("retry should record the now-attached watcher on the re-used row")
	}
}
