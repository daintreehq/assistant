package app

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/config"
)

// newOfflineApp builds a fully-wired App against a temp-dir state DB in offline
// mode (no network, no model calls). It applies the scheduler-context setup
// (overrides: offline + stateDir + projectPath + operator tier) but exercises the
// real DefaultToolBuilder so AssertSafe runs over the whole wired tool set.
func newOfflineApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }

// TestCreateWiresEveryDependency asserts App.Create builds the full dependency
// graph in the canonical order — config → store → mcp → queue → router → registry
// (AssertSafe already passed inside Create) → skills → session — with no nil seam.
func TestCreateWiresEveryDependency(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if a.Store == nil {
		t.Error("Store is nil")
	}
	if a.MCP == nil {
		t.Error("MCP is nil")
	}
	if a.Queue == nil {
		t.Error("Queue is nil")
	}
	if a.Router == nil {
		t.Error("Router is nil")
	}
	if a.Registry == nil {
		t.Error("Registry is nil")
	}
	if a.Skills == nil {
		t.Error("Skills is nil")
	}
	if a.Session == nil {
		t.Error("Session is nil")
	}
	if a.SessionID == "" || !strings.HasPrefix(a.SessionID, "ses_") {
		t.Errorf("SessionID = %q, want ses_ prefix", a.SessionID)
	}
}

// TestCreateRegistersFullToolSet asserts the real builder wires the full tool
// inventory and that AssertSafe (the hard no-file-edit gate inside Create) passed
// over it. The parity worklist expects 81 tools (incl. the agentTask.superviseTerminal
// adopt tool, the agentTask.status / agentTask.list readers, the worktree.list /
// worktree.getCurrent readers, the git.getProjectPulse read wrapper, the
// terminal.close wrapper, the terminal.rename wrapper, the terminal.awaitAll cohort finish-wait, the
// terminal.extract.json structured-extract tool, and the five scratch.* session-scratch
// tools); we assert that exact count so a silent family add/drop is caught.
func TestCreateRegistersFullToolSet(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	got := len(a.Registry.List())
	if got != 81 {
		t.Errorf("registered tools = %d, want 81", got)
	}
	// The count bump from 79→80 is the structured-extract split; assert the new tool
	// by name so the count guard can't be satisfied by some unrelated add/drop.
	if !a.Registry.Has("terminal.extract.json") {
		t.Error("terminal.extract.json (structured extract) not registered")
	}
	// AssertSafe ran inside Create (boot would have failed otherwise); re-run it to
	// pin the invariant that the full wired set carries no file-edit tool.
	if err := a.Registry.AssertSafe(); err != nil {
		t.Errorf("AssertSafe over full set: %v", err)
	}
}

// TestScratchToolsWired asserts the scratch.* session-scratch family is registered
// by the real builder, and that none of its tools leaked into the always-offered
// core set (scratch is optional workflow tooling, discovered on demand).
func TestScratchToolsWired(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	scratchTools := []string{
		"scratch.create", "scratch.set", "scratch.get", "scratch.delete", "scratch.drop",
	}
	for _, name := range scratchTools {
		if !a.Registry.Has(name) {
			t.Errorf("scratch tool %q not registered", name)
		}
	}
	core := make(map[string]bool, len(agent.CoreToolNames()))
	for _, n := range agent.CoreToolNames() {
		core[n] = true
	}
	for _, name := range scratchTools {
		if core[name] {
			t.Errorf("scratch tool %q must not be a core tool", name)
		}
	}
}

// TestCreateSeedsThreeControlMessages asserts the session boots with the three
// fixed control messages (base prompt, runtime context, loaded skills) at indices
// 0/1/2 — the cached-prefix layout the prompt contract depends on.
func TestCreateSeedsThreeControlMessages(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	msgs := a.Session.Messages()
	if len(msgs) < 3 {
		t.Fatalf("session messages = %d, want >= 3 control rows", len(msgs))
	}
	for i := 0; i < 3; i++ {
		if msgs[i].Role != "system" {
			t.Errorf("control[%d] role = %q, want system", i, msgs[i].Role)
		}
	}
}

// TestSchedulerContextDormantBeforeStart: before StartScheduler,
// PromptContext().SchedulerActive is false and the runtime
// context message (message[1]) carries the dormant note.
func TestSchedulerContextDormantBeforeStart(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if a.PromptContext().SchedulerActive {
		t.Error("SchedulerActive = true before StartScheduler, want false")
	}
	runtimeMsg := a.Session.Messages()[1].ContentToText()
	if !strings.Contains(runtimeMsg, "the scheduler is NOT running") {
		t.Errorf("runtime message missing dormant note:\n%s", runtimeMsg)
	}
}

// TestSchedulerContextActiveAfterStart: after StartScheduler,
// SchedulerActive flips true and the dormant note is cleared
// from the refreshed runtime context message.
func TestSchedulerContextActiveAfterStart(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.StartScheduler(context.Background(), nil)

	if !a.PromptContext().SchedulerActive {
		t.Error("SchedulerActive = false after StartScheduler, want true")
	}
	runtimeMsg := a.Session.Messages()[1].ContentToText()
	if strings.Contains(runtimeMsg, "the scheduler is NOT running") {
		t.Errorf("runtime message still carries dormant note after start:\n%s", runtimeMsg)
	}
}

// TestSessionEndedWatcherNoteSurfacesOnceWhenSchedulerActive asserts the one-time
// session-ended-watchers NOTE: gated on the scheduler (dormant sessions never show
// it), surfaced in message[1] once the scheduler is active, and stripped after the
// first turn consumes it (the storage carryover itself is covered by reopen_test.go).
func TestSessionEndedWatcherNoteSurfacesOnceWhenSchedulerActive(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// Stand in for a prior session having left a watcher running that the open-time
	// sweep cancelled and carried forward.
	a.sessionEndedWatchers = []string{"deploy watcher"}

	// Dormant (no scheduler) → gated off, no NOTE in message[1].
	if got := a.PromptContext().SessionEndedWatchers; got != nil {
		t.Fatalf("note must be gated off before StartScheduler, got %v", got)
	}
	if msg := a.Session.Messages()[1].ContentToText(); strings.Contains(msg, "previous session ended") {
		t.Fatalf("dormant runtime context must not carry the note:\n%s", msg)
	}

	// Scheduler active → NOTE appears in message[1].
	a.StartScheduler(context.Background(), nil)
	if msg := a.Session.Messages()[1].ContentToText(); !strings.Contains(msg, "previous session ended") ||
		!strings.Contains(msg, `"deploy watcher"`) {
		t.Fatalf("active runtime context missing the session-ended note:\n%s", msg)
	}

	// First turn consumes it → stripped from message[1], and it stays gone.
	a.ConsumeSessionEndedNote()
	if got := a.PromptContext().SessionEndedWatchers; got != nil {
		t.Fatalf("note must be consumed after the first turn, got %v", got)
	}
	if msg := a.Session.Messages()[1].ContentToText(); strings.Contains(msg, "previous session ended") {
		t.Fatalf("consumed note must be stripped from message[1]:\n%s", msg)
	}
}

// TestStartupContextRosterSurfacesInRuntimeMessage asserts the cached configured-agents
// roster and current worktree (populated by refreshStartupContext on connect) propagate
// through PromptContext into message[1]. The cache is set directly here — the connect
// fetch itself is exercised by the agenttaskx + parseCurrentWorktreeLabel unit tests;
// this pins the wiring App cache → MainPromptContext → rendered runtime context.
func TestStartupContextRosterSurfacesInRuntimeMessage(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// Before any connect the cache is empty → no roster surfaced.
	if got := a.PromptContext().ConfiguredAgentIDs; len(got) != 0 {
		t.Fatalf("expected empty roster before connect, got %v", got)
	}

	// Stand in for a connect having cached the roster + worktree.
	a.rosterMu.Lock()
	a.cachedAgentIDs = []string{"claude", "codex"}
	a.cachedActiveWorktree = "feature/issue-230"
	a.rosterMu.Unlock()

	pc := a.PromptContext()
	if len(pc.ConfiguredAgentIDs) != 2 || pc.ActiveWorktree != "feature/issue-230" {
		t.Fatalf("PromptContext did not surface the cache: %+v", pc)
	}

	a.Session.RefreshRuntimeContext(pc)
	msg := a.Session.Messages()[1].ContentToText()
	if !strings.Contains(msg, "Configured agents: claude, codex") {
		t.Fatalf("runtime message missing configured-agents roster:\n%s", msg)
	}
	if !strings.Contains(msg, "Active worktree: feature/issue-230") {
		t.Fatalf("runtime message missing active worktree:\n%s", msg)
	}
}

// TestStartSchedulerIdempotent asserts a second StartScheduler call does not leak a
// second ticker — it rebinds onto the existing scheduler and returns the same
// instance (the idempotency invariant).
func TestStartSchedulerIdempotent(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	first := a.StartScheduler(context.Background(), nil)
	second := a.StartScheduler(context.Background(), nil)
	if first != second {
		t.Error("StartScheduler returned a fresh scheduler on the second call (ticker leak)")
	}
}

// TestShutdownReversesCleanlyNoGoroutineLeak drives the full lifecycle — Create →
// StartScheduler → Shutdown — and asserts Shutdown tears down without leaving the
// scheduler goroutine running. The goroutine count must settle back to its
// pre-scheduler baseline once Shutdown's Stop+Drain completes.
func TestShutdownReversesCleanlyNoGoroutineLeak(t *testing.T) {
	dir := t.TempDir()
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	baseline := runtime.NumGoroutine()
	a.StartScheduler(context.Background(), nil)

	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	// After Stop+Drain the scheduler's ticker goroutine has returned; allow the
	// runtime a brief settle window before sampling (goroutine teardown is async).
	settled := false
	for i := 0; i < 50; i++ {
		if runtime.NumGoroutine() <= baseline {
			settled = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !settled {
		t.Errorf("goroutines did not settle after Shutdown: have %d, baseline %d",
			runtime.NumGoroutine(), baseline)
	}

	// Shutdown is safe to call again (store already closed); it should not panic.
	_ = a.Shutdown()
}

// TestShutdownWithoutSchedulerClosesStore asserts Shutdown is well-behaved when the
// scheduler was never started: it just closes MCP + the store and returns nil.
func TestShutdownWithoutSchedulerClosesStore(t *testing.T) {
	a := newOfflineApp(t)
	if err := a.Shutdown(); err != nil {
		t.Fatalf("Shutdown (no scheduler): %v", err)
	}
}
