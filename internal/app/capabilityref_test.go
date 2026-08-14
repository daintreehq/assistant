package app

import (
	"flag"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/tools"
)

// update regenerates the committed generated docs instead of asserting on them:
//
//	go test ./internal/app -run TestGeneratedDocsAreCurrent -update
var update = flag.Bool("update", false, "rewrite the generated docs under docs/generated/")

// generatedDir resolves docs/generated/ relative to this package.
func generatedDir(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "docs", "generated")
}

// newFlaggedApp is newOfflineApp with the workflow-intelligence flag forced on, so the
// generated reference documents the complete registry — flag-gated tools included,
// labelled as such — rather than only what a default launch happens to register.
func newFlaggedApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	on := true
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:              boolPtr(true),
			StateDir:             &dir,
			ProjectPath:          &dir,
			Tier:                 strPtr("operator"),
			WorkflowIntelligence: &on,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return a
}

// TestGeneratedDocsAreCurrent is the anti-drift gate. It regenerates the capability
// reference and the compatibility manifest from the LIVE registry and constants, then
// diffs them against what is committed.
//
// Failing here is not a nuisance: it means the binary's capabilities changed and the
// documents describing them did not. That exact drift is what left the README claiming
// 67 tools, docs/TOOLS.md claiming 83, and the registry holding ~85 — including entries
// for tools that had been deleted. Regenerate with -update; do not hand-edit the output.
func TestGeneratedDocsAreCurrent(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	files := map[string]string{
		"TOOLS.md":         RenderToolReference(CollectToolDocs(a.Registry)),
		"COMPATIBILITY.md": RenderCompatibilityManifest(),
	}

	dir := generatedDir(t)
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		for name, want := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(want), 0o644); err != nil {
				t.Fatalf("write %s: %v", name, err)
			}
			t.Logf("regenerated %s", filepath.Join(dir, name))
		}
		return
	}

	for name, want := range files {
		path := filepath.Join(dir, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s is missing — regenerate with: go test ./internal/app -run TestGeneratedDocsAreCurrent -update\n  (%v)", path, err)
		}
		if string(got) != want {
			t.Errorf("%s is stale.\n\nThe registry or the compatibility constants changed without regenerating.\nRun: go test ./internal/app -run TestGeneratedDocsAreCurrent -update\n\n%s",
				path, firstDifference(string(got), want))
		}
	}

	// Every rendered document must be valid UTF-8. Byte-wise truncation once split the
	// multi-byte "→" in a tool description and emitted a lone continuation byte, which
	// is enough to make awk, iconv, and other validating consumers reject the file.
	for name, want := range files {
		if !utf8.ValidString(want) {
			t.Errorf("%s is not valid UTF-8 — something is truncating by bytes instead of runes", name)
		}
	}

	// The directory must hold EXACTLY the generated set and nothing else. Diffing only
	// the files the renderers currently produce cannot notice a document a renderer has
	// stopped producing: the stale file just sits there, tracked and unchanged, and every
	// check passes while the repo ships documentation nothing generates any more.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, expected := files[e.Name()]; !expected && e.Name() != commandRefBasename {
			t.Errorf("%s is in docs/generated/ but no renderer produces it — delete it, or wire up its generator",
				filepath.Join(dir, e.Name()))
		}
	}
}

// commandRefBasename is rendered by internal/commands (which imports this package, so
// the generator cannot live here). Named so the exact-file-set check above does not
// flag it as an orphan.
const commandRefBasename = "COMMANDS.md"

// firstDifference reports the first differing line, so a failure names WHAT drifted
// rather than dumping two ~200-line documents into the test log.
func firstDifference(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(g) && i < len(w); i++ {
		if g[i] != w[i] {
			return "first difference at line " + strconv.Itoa(i+1) + ":\n  committed: " + g[i] + "\n  generated: " + w[i]
		}
	}
	return "documents share a prefix but differ in length (committed " +
		strconv.Itoa(len(g)) + " lines, generated " + strconv.Itoa(len(w)) + ")"
}

// The generated reference labels a set of tools as flag-gated. That label is a promise
// about registration, so verify it empirically: the tools present with the flag ON and
// absent with it OFF must be EXACTLY the labelled set. Otherwise the reference could
// quietly promise an unavailable tool, or hide an available one, forever.
func TestWorkflowGraphToolsAreExactlyTheFlagGatedDifference(t *testing.T) {
	on := newFlaggedApp(t)
	defer on.Shutdown()
	off := newOfflineApp(t)
	defer off.Shutdown()

	present := func(a *App) map[string]bool {
		m := map[string]bool{}
		for _, tool := range a.Registry.List() {
			m[tool.Name] = true
		}
		return m
	}
	withFlag, withoutFlag := present(on), present(off)

	var extra []string
	for name := range withFlag {
		if !withoutFlag[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)

	var want []string
	for name := range workflowGraphTools {
		want = append(want, name)
	}
	sort.Strings(want)

	if strings.Join(extra, ",") != strings.Join(want, ",") {
		t.Errorf("flag-gated tool set drifted.\n  flag ON adds: %v\n  labelled:     %v\nUpdate workflowGraphTools in capabilityref.go.", extra, want)
	}

	// Nothing may DISAPPEAR when the flag is on — a feature flag that removes a tool
	// would leave the model without a recovery path it had a moment ago.
	for name := range withoutFlag {
		if !withFlag[name] {
			t.Errorf("%s is registered with the flag OFF but not ON — a flag must only add", name)
		}
	}
}

// Every tool must declare a connection dependency the reference knows how to render.
// A typo'd or invented Connection would print as an unexplained token in a document
// testers use to decide what still works while Daintree is disconnected.
func TestEveryToolDeclaresAKnownConnection(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	known := map[tools.Connection]bool{
		tools.RequiresNothing:     true,
		tools.RequiresDaintreeMCP: true,
		tools.RequiresDocsMCP:     true,
		tools.RequiresBackend:     true,
		tools.RequiresInteractive: true,
	}
	for _, tool := range a.Registry.List() {
		if !known[tool.Requires] {
			t.Errorf("%s declares unknown connection %q", tool.Name, tool.Requires)
		}
	}
}

// The families that cannot work without the Daintree control plane must SAY so. This
// pins the specific claim the old docs got wrong — that terminal watchers work normally
// in degraded mode — and guards the general one: a tool that reaches Daintree while
// reporting itself local would be listed as available in a degraded-mode banner and
// then fail on use.
func TestControlPlaneToolsDeclareTheirDependency(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	mustNeedDaintree := []string{
		"agentTask.spawnForEdits", "agentTask.superviseTerminal",
		"terminal.read", "terminal.sendCommand", "terminal.close", "terminal.focus",
		"terminal.awaitAll", "terminal.extract", "terminal.run.async", "terminal.await.async",
		"terminal.summarize", "daintree.call", "daintree.listTools",
		"watcher.terminal.create", "watcher.watchPR",
		"worktree.list", "recipe.run", "forge.getIssue", "git.getProjectPulse",
	}
	mustNeedDocs := []string{"docs.search", "docs.getPage", "docs.getRelatedPages"}
	mustNeedBackend := []string{"workflow.plan", "workflow.reconcile"}
	// The degraded-mode set. Every name here is something a DISCONNECTED user needs, so
	// a family-level stamp that swept one of them up would tell them their remaining
	// options were unavailable — the most damaging direction for this column to be wrong.
	mustBeLocal := []string{
		"fs.read", "fs.list", "fs.search",
		"memory.save", "memory.recall", "timer.schedule", "watcher.list", "watcher.cancel",
		"scratch.set", "audit.export", "grant.create", "artifact.read",
		// Reads its own SQLite ledger, not Daintree.
		"agentTask.status", "agentTask.list",
		// Cancelling supervision you can no longer perform is exactly an offline need.
		"async.list", "async.cancel",
		// Explicitly best-effort: reports the outage as part of its answer.
		"context.snapshot",
		// The diagnostic FOR the outage — never mark it unavailable during one.
		"daintree.status",
		// A local ledger; only plan/reconcile call the backend.
		"workflow.create", "workflow.list", "workflow.getGraph", "workflow.next",
	}

	byName := map[string]*tools.Tool{}
	for _, tool := range a.Registry.List() {
		byName[tool.Name] = tool
	}
	check := func(names []string, want tools.Connection) {
		for _, n := range names {
			tool, ok := byName[n]
			if !ok {
				t.Errorf("%s is not registered — this list is stale", n)
				continue
			}
			if tool.Requires != want {
				t.Errorf("%s declares Requires=%q, want %q", n, tool.Requires, want)
			}
		}
	}
	check(mustNeedDaintree, tools.RequiresDaintreeMCP)
	check(mustNeedDocs, tools.RequiresDocsMCP)
	check(mustNeedBackend, tools.RequiresBackend)
	check(mustBeLocal, tools.RequiresNothing)
}
