package app

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/prompts"
	"github.com/daintreehq/assistant/internal/storage"
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
			// Force the flag OFF rather than inheriting it. Config resolution reads the
			// real process env, so a developer with DAINTREE_WORKFLOW_INTELLIGENCE=1
			// exported would otherwise get seven extra tools in the "unflagged" App and
			// fail the tests that compare the two states against each other.
			WorkflowIntelligence: boolPtr(false),
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
// graph in the canonical order — config → store → mcp → queue → backend → registry
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
	if a.Backend == nil {
		t.Error("Backend is nil")
	}
	if a.Registry == nil {
		t.Error("Registry is nil")
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
// over it. The worklist expects 79 tools (incl. the agentTask.superviseTerminal
// adopt tool, the agentTask.status / agentTask.list readers, the worktree.list /
// worktree.getCurrent readers, the git.getProjectPulse read wrapper, the
// terminal.close wrapper, the terminal.rename wrapper, the terminal.moveToWorktree cohort
// relocation wrapper, the subagent.run delegation primitive, the terminal.awaitAll cohort finish-wait, the
// terminal.extract.json structured-extract tool, the five scratch.* session-scratch
// tools, the four async-futures
// tools — terminal.run.async / terminal.await.async / async.list / async.cancel — and
// the user.askMultipleChoice question tool). The local skill.find / skill.load tools are
// GONE — the backend now owns skill selection (the migration off the client-side
// selector); skill.run.get / skill.step.advance remain. The three docs.* tools are GONE
// too (issue #332): the backend searches the documentation MCP itself. We assert that
// exact count so a silent family add/drop is caught.
func TestCreateRegistersFullToolSet(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// The exact count is a deliberate tripwire: the tool inventory is sent on EVERY
	// model round (~16k tokens), and inventory size measurably degrades tool-selection
	// accuracy, so a family that quietly grows must be a conscious decision.
	got := len(a.Registry.List())
	// 77 after the docs family was removed (issue #332): Daintree usage questions are
	// answered by the backend now, which searches the public docs MCP itself before the
	// model opens. Offering a local docs tool alongside that would give the model two
	// ways to answer the same question, and it would use both.
	//
	// 78 with subagent.run, the delegation primitive. It is the rare addition that
	// makes the inventory problem BETTER rather than worse: it exists to keep an
	// expensive search out of the main conversation entirely, and the sub-agent it
	// dispatches is offered a read-only subset of this same registry rather than a
	// second inventory of its own.
	//
	// 79 with the terminal.moveToWorktree cohort-relocation wrapper (issue #347).
	//
	// 80 with tool.schema, the MCP input-schema lookup restored from #311/PR #314
	// (issue #367). It is the counterpart to tool.search: search finds a tool's
	// NAME, and until this landed nothing could report its ARGUMENT SHAPE, so a
	// wrong guess had nowhere to be corrected.
	//
	// 88 with the eight issue #367 wrappers. These are the additions that make the
	// inventory SMALLER in practice rather than larger: every one of them was
	// previously reachable only through daintree.call, which is system risk and
	// always confirmed, so asking an ordinary question — "what do the issue comments
	// say?", "did the check pass?", "what errors were logged?" — cost a typed human
	// approval. Six carry read risk and now run with none.
	if got != 88 {
		t.Errorf("registered tools = %d, want 88", got)
	}
	if !a.Registry.Has("tool.schema") {
		t.Error("tool.schema (MCP input-schema lookup) must be registered")
	}
	for _, name := range []string{
		"project.detectRunners", "project.runCheck", "forge.listIssueComments",
		"agentSessionHistory.list", "browser.getConsoleMessages", "errors.recent",
		"notifications.recent", "worktree.resource.status",
	} {
		if !a.Registry.Has(name) {
			t.Errorf("%s (issue #367 typed wrapper) must be registered", name)
		}
	}
	// Name the newest addition explicitly: the count alone would stay green if this
	// tool were dropped while an unrelated one was added.
	if !a.Registry.Has("terminal.moveToWorktree") {
		t.Error("terminal.moveToWorktree must be registered")
	}
	// The local skill-selection tools were removed in the backend migration; assert
	// their absence so a re-introduction (or a stale wiring) is caught here.
	if a.Registry.Has("skill.find") || a.Registry.Has("skill.load") {
		t.Error("skill.find/skill.load must NOT be registered (skill selection is backend-owned)")
	}
	if !a.Registry.Has("terminal.extract.json") {
		t.Error("terminal.extract.json (structured extract) not registered")
	}
	// The docs family is backend-owned now. Assert its ABSENCE by name so a
	// re-introduction (or a stale wiring) is caught here rather than as a model that
	// answers usage questions two different ways.
	for _, name := range []string{"docs.search", "docs.getPage", "docs.getRelatedPages"} {
		if a.Registry.Has(name) {
			t.Errorf("%s must NOT be registered — docs lookups are backend-owned", name)
		}
	}
	// The async-futures family (the 82→86 bump): assert by name so the count guard
	// can't be satisfied by an unrelated add/drop.
	for _, name := range []string{"terminal.run.async", "terminal.await.async", "async.list", "async.cancel"} {
		if !a.Registry.Has(name) {
			t.Errorf("%s (async-futures family) not registered", name)
		}
	}
	// The 84→85 bump is user.askMultipleChoice (the multiple-choice question tool).
	if !a.Registry.Has("user.askMultipleChoice") {
		t.Error("user.askMultipleChoice (question tool) not registered")
	}
	// AssertSafe ran inside Create (boot would have failed otherwise); re-run it to
	// pin the invariant that the full wired set carries no file-edit tool.
	if err := a.Registry.AssertSafe(); err != nil {
		t.Errorf("AssertSafe over full set: %v", err)
	}
}

// stampStaleStateDB writes a state.db in dir stamped with an OLDER baseline
// user_version so the next storage.Open trips the stale-schema branch. The driver is
// registered transitively via the storage package's blank import of modernc.org/sqlite.
func stampStaleStateDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "state.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("PRAGMA user_version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCreateSchemaResetRecovery exercises the graceful recovery: a stale on-disk DB
// plus an OnSchemaStale handler that authorises a reset lets Create wipe-and-rebuild
// and boot a healthy App, while a declining handler aborts with the actionable error
// and leaves the file untouched.
func TestCreateSchemaResetRecovery(t *testing.T) {
	t.Run("authorised reset rebuilds and boots", func(t *testing.T) {
		dir := t.TempDir()
		stampStaleStateDB(t, dir)
		var sawHave, sawWant int
		a, err := Create(CreateOptions{
			Overrides: config.ConfigOverrides{
				Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
			},
			OnSchemaStale: func(have, want int) (bool, error) {
				sawHave, sawWant = have, want
				return true, nil
			},
		})
		if err != nil {
			t.Fatalf("Create after authorised reset: %v", err)
		}
		defer a.Shutdown()
		if sawHave != 1 || sawWant == 0 {
			t.Fatalf("handler saw Have=%d Want=%d, want Have=1 Want>0", sawHave, sawWant)
		}
		if a.Store == nil {
			t.Fatal("Store nil after reset recovery")
		}
		// Prove the DB was actually REBUILT, not merely reopened: the stale baseline (1)
		// must now be the current schema version.
		var version int
		if err := a.Store.DB().QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			t.Fatalf("read user_version: %v", err)
		}
		if version != sawWant {
			t.Fatalf("after authorised reset user_version = %d, want current %d", version, sawWant)
		}
	})

	t.Run("declined reset aborts with typed error", func(t *testing.T) {
		dir := t.TempDir()
		path := stampStaleStateDB(t, dir)
		_, err := Create(CreateOptions{
			Overrides: config.ConfigOverrides{
				Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
			},
			OnSchemaStale: func(int, int) (bool, error) { return false, nil },
		})
		var stale *storage.SchemaStaleError
		if !errors.As(err, &stale) {
			t.Fatalf("want *storage.SchemaStaleError on decline, got %T: %v", err, err)
		}
		// Declining must NOT touch the file — it is still there, still stamped stale.
		if _, serr := os.Stat(path); serr != nil {
			t.Fatalf("declined reset must leave the DB file in place, stat err = %v", serr)
		}
	})

	t.Run("handler error propagates verbatim", func(t *testing.T) {
		dir := t.TempDir()
		stampStaleStateDB(t, dir)
		sentinel := errors.New("prompt aborted")
		_, err := Create(CreateOptions{
			Overrides: config.ConfigOverrides{
				Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
			},
			OnSchemaStale: func(int, int) (bool, error) { return false, sentinel },
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("handler error must propagate, got %v", err)
		}
	})

	t.Run("non-stale open error bypasses the handler", func(t *testing.T) {
		dir := t.TempDir()
		// A directory where state.db should be makes storage.Open fail with a generic,
		// NON-stale error. The handler must NOT be consulted (errors.As short-circuits)
		// — we never prompt to wipe state for an unrelated failure.
		if err := os.Mkdir(filepath.Join(dir, "state.db"), 0o700); err != nil {
			t.Fatal(err)
		}
		called := false
		_, err := Create(CreateOptions{
			Overrides: config.ConfigOverrides{
				Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
			},
			OnSchemaStale: func(int, int) (bool, error) { called = true; return true, nil },
		})
		if err == nil {
			t.Fatal("expected a non-stale open error, got nil")
		}
		if called {
			t.Fatal("OnSchemaStale must NOT be consulted for a non-stale open error")
		}
		var stale *storage.SchemaStaleError
		if errors.As(err, &stale) {
			t.Fatalf("a directory-as-DB is not a stale-schema error, got %v", err)
		}
	})
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

// TestCreateStartsWithEmptyVisibleHistory asserts a fresh session boots with NO
// client-side control prefix: the backend owns the system prompt, developer
// instructions, and skill bodies, so domain.ControlMessageCount == 0 and the first
// turn appends the user message at index 0. (Replaces the old three-control-message
// seed test — that cached client-side prefix was removed in the backend migration.)
func TestCreateStartsWithEmptyVisibleHistory(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if domain.ControlMessageCount != 0 {
		t.Fatalf("ControlMessageCount = %d, want 0 (no client-side control prefix)", domain.ControlMessageCount)
	}
	if msgs := a.Session.Messages(); len(msgs) != 0 {
		t.Errorf("fresh session visible history = %d messages, want 0:\n%+v", len(msgs), msgs)
	}
}

// TestSchedulerContextDormantBeforeStart: before StartScheduler the App's live
// PromptContext reports SchedulerActive == false. The runtime context now travels
// as structured data (backend.RuntimeContext.SchedulerActive) built from this, NOT
// as a prose message[1] note, so the assertion is on PromptContext directly.
func TestSchedulerContextDormantBeforeStart(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if a.PromptContext().SchedulerActive {
		t.Error("SchedulerActive = true before StartScheduler, want false")
	}
}

// TestSchedulerContextActiveAfterStart: after StartScheduler the App's live
// PromptContext reports SchedulerActive == true (the structured runtime block is
// rebuilt from PromptContext each round — there is no message[1] note to refresh).
func TestSchedulerContextActiveAfterStart(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.StartScheduler(context.Background(), nil)

	if !a.PromptContext().SchedulerActive {
		t.Error("SchedulerActive = false after StartScheduler, want true")
	}
}

// TestResumedWatchersForFooterSchedulerGate asserts the footer seam for the one-time
// resumed-watchers note: gated OFF while dormant (no scheduler — nothing is actually
// being supervised), and OPEN once the scheduler is active — where it mirrors the
// ownership-boot summary captured at Create. The note never touches message[1]; its
// once-per-session surfacing in the uncached footer is covered by
// internal/agent/footer_test.go, and the adoption itself by storage/reopen_test.go.
func TestResumedWatchersForFooterSchedulerGate(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	// Dormant (no scheduler) → gated off, regardless of any adopted watchers.
	if got := a.resumedWatchersForFooter(); got != nil {
		t.Fatalf("footer seam must be gated off before StartScheduler, got %v", got)
	}
	// The visible history never carries the note anymore — there is no message[1] runtime
	// context, and the one-time note surfaces only through the uncached footer seam.
	if msgs := a.Session.Messages(); len(msgs) != 0 {
		t.Fatalf("resumed-watchers note must never enter visible history, got %d messages:\n%+v", len(msgs), msgs)
	}

	// Scheduler active → the gate opens and the seam mirrors the ownership summary
	// (empty for a fresh store, but no longer forced nil by the gate).
	a.StartScheduler(context.Background(), nil)
	got, want := a.resumedWatchersForFooter(), a.Ownership().ResumedWatcherTitles
	if len(got) != len(want) {
		t.Fatalf("active footer seam must mirror the ownership summary: got %v, want %v", got, want)
	}
}

// TestStartupSnapshotSurfacesInPromptContext pins the atomic cache → request-context
// wiring. Project/agents become the stable pre-conversation block; worktree becomes the
// fresh tail, but all three originate from one published Daintree snapshot.
func TestStartupSnapshotSurfacesInPromptContext(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	if got := a.PromptContext().AgentRoster; got != nil {
		t.Fatalf("expected unknown roster before connect, got %+v", got)
	}

	installed, visible := true, true
	a.startupMu.Lock()
	a.cachedProject = &prompts.ProjectContext{ID: "project-1", Name: "Demo", Path: "/repo"}
	a.cachedAgents = &prompts.AgentRosterContext{Agents: []prompts.AgentContext{
		{ID: "claude", Installed: &installed, ToolbarVisible: &visible},
		{ID: "codex", Installed: &installed},
	}}
	a.cachedWorktree = &prompts.WorktreeContext{Present: true, ID: "wt-1", Branch: "feature/issue-230"}
	a.startupMu.Unlock()

	pc := a.PromptContext()
	if pc.Project == nil || pc.Project.Name != "Demo" {
		t.Fatalf("PromptContext did not surface the project: %+v", pc.Project)
	}
	if pc.AgentRoster == nil || len(pc.AgentRoster.Agents) != 2 || pc.AgentRoster.Agents[0].ID != "claude" {
		t.Fatalf("PromptContext did not surface the roster: %+v", pc.AgentRoster)
	}
	if pc.Worktree == nil || pc.Worktree.ID != "wt-1" {
		t.Fatalf("PromptContext did not surface the worktree: %+v", pc.Worktree)
	}
	if got := a.activeWorktreeForFooter(); got != "feature/issue-230" {
		t.Fatalf("footer worktree seam = %q, want the cached label", got)
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
