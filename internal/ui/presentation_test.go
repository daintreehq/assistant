package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// presentation_test.go: presentTool maps first-party tools to human verbs (never
// raw fn() syntax for unknowns), and
// BuildAgentRows merges/orders watchers + threads the epistemic kind (persisted
// preference, classification fallback, raw passthrough).

func TestPresentTool_FirstPartyVerbs(t *testing.T) {
	cases := map[string]string{
		"fs.search":               "Searched",
		"agentTask.spawnForEdits": "Delegated",
		"watcher.terminal.create": "Watching",
		"fs.read":                 "Read",
		"fs.list":                 "Listed",
		"fs.find":                 "Found",
		// The scheduling verb is keyed on the real tool name.
		"timer.schedule": "Scheduled",
		// High-frequency orchestration tools that used to fall through to their raw
		// dotted name in the activity tree (the scratch store + await/send family).
		"terminal.awaitAll":    "Waited",
		"terminal.sendCommand": "Ran",
		"scratch.create":       "Scratch",
		"scratch.set":          "Scratch",
		"scratch.get":          "Scratch",
		"memory.recall":        "Recalled",
		"artifact.read":        "Read artifact",
	}
	for name, want := range cases {
		if got := presentTool(name); got != want {
			t.Errorf("presentTool(%q) = %q, want %q", name, got, want)
		}
	}
}

// The copy-tree label (`name`) is what the user actually wants to read in the
// activity row — "auth flow context" beats a worktree id — but it is optional,
// so the id previews must survive as the fallback when it is absent or blank.
func TestPresentToolTarget_CopyTreePrefersName(t *testing.T) {
	cases := []struct {
		tool string
		args string
		want string
	}{
		{"copyTree.generate", `{"name":"auth flow context","worktreeId":"wt-1"}`, "auth flow context"},
		{"copyTree.generateAndCopyFile", `{"name":"auth flow context","worktreeId":"wt-1"}`, "auth flow context"},
		{"copyTree.injectToTerminal", `{"name":"auth flow context","terminalId":"t1"}`, "auth flow context"},
		// No name → fall back to the id the row previewed before the label existed.
		{"copyTree.generate", `{"worktreeId":"wt-1"}`, "wt-1"},
		{"copyTree.generateAndCopyFile", `{"worktreeId":"wt-1"}`, "wt-1"},
		{"copyTree.injectToTerminal", `{"terminalId":"t1"}`, "t1"},
		// A blank name means "derive a label" and must not eat the fallback.
		{"copyTree.generate", `{"name":"  ","worktreeId":"wt-1"}`, "wt-1"},
		{"copyTree.injectToTerminal", `{"name":"","terminalId":"t1"}`, "t1"},
		// A control-only name sanitizes to nothing and must ALSO fall through
		// rather than black-hole the row (previewLine runs before the check).
		{"copyTree.generate", `{"name":"\u001b\u000b","worktreeId":"wt-1"}`, "wt-1"},
	}
	for _, c := range cases {
		if got := presentToolTarget(c.tool, c.args); got != c.want {
			t.Errorf("presentToolTarget(%q, %s) = %q, want %q", c.tool, c.args, got, c.want)
		}
	}

	// The label is free text, so the preview must flatten it to one safe row:
	// newlines/tabs/padding collapse to single spaces (previewLine)…
	if got := presentToolTarget("copyTree.generate", `{"name":"  auth\nflow\t context "}`); got != "auth flow context" {
		t.Errorf("multi-line/padded name must collapse to one line, got %q", got)
	}
	// …an ESC byte (spelled \u001b in the JSON) becomes a space, leaving the
	// sequence's residue as inert text — the row can never be restyled/cleared…
	if got := presentToolTarget("copyTree.generate", `{"name":"auth \u001b[31mflow"}`); got != "auth [31mflow" {
		t.Errorf("an escape byte must be neutralized to inert text, got %q", got)
	}
	// …and an over-long label truncates to the 48-cell budget with an ellipsis.
	long := strings.Repeat("a", 60)
	if got := presentToolTarget("copyTree.generate", `{"name":"`+long+`"}`); got != strings.Repeat("a", 47)+"…" {
		t.Errorf("long name must truncate at 48 cells with an ellipsis, got %q", got)
	}
}

// TestPresentTool_NoRawDottedNames guards against the presentation gap that motivated
// the verb-map expansion: a first-party tool that renders its raw dotted name (e.g.
// "scratch.create") next to nicely-verbed neighbours ("Delegated"). Every canonical
// local tool name should resolve to a human label — never a string still carrying a
// dot from its namespace.
func TestPresentTool_NoRawDottedNames(t *testing.T) {
	names := []string{
		"scratch.create", "scratch.set", "scratch.get", "scratch.delete", "scratch.drop",
		"terminal.awaitAll", "terminal.sendCommand", "terminal.rename", "terminal.close",
		"terminal.arm", "terminal.disarm", "terminal.disarmAll",
		"terminal.run.async", "terminal.await.async", "async.list", "async.cancel",
		"memory.recall", "memory.list", "memory.save", "memory.forget", "memory.pin", "memory.unpin",
		"fs.find",
		"artifact.read", "copyTree.generate", "copyTree.injectToTerminal",
		"docs.search", "docs.getPage", "docs.getRelatedPages",
		"forge.getPR", "watcher.watchPR", "audit.export",
		"agentTask.status", "agentTask.list", "agentTask.superviseTerminal",
		"user.askMultipleChoice",
		"workflow.plan", "workflow.reconcile", "workflow.create", "workflow.list",
	}
	for _, n := range names {
		got := presentTool(n)
		if got == n || contains(got, ".") {
			t.Errorf("presentTool(%q) = %q — still a raw dotted name; add a human verb", n, got)
		}
	}
}

func TestPresentTool_UnknownFallsBackToInternalName(t *testing.T) {
	// Unknown tools fall back to the RAW internal name — NEVER raw "fn(" syntax, and
	// NOT title-cased.
	got := presentTool("some.exotic.tool")
	if got != "some.exotic.tool" {
		t.Errorf("presentTool unknown leaf = %q, want raw internal name some.exotic.tool", got)
	}
	for _, name := range []string{"some.exotic.thing", "weird"} {
		if p := presentTool(name); contains(p, "(") {
			t.Errorf("presentTool(%q) = %q must not contain a paren (raw fn syntax)", name, p)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// watcherRec builds a watcher record with overridable classification/epistemic.
func watcherRec(id string, classification string, epistemic *domain.EpistemicKind) domain.WatcherRecord {
	w := domain.WatcherRecord{
		ID:        id,
		Kind:      "terminal",
		Title:     "repair tests",
		Goal:      "wait for tests",
		Status:    "active",
		CadenceMs: 1000,
	}
	if classification != "" {
		c := classification
		w.LastClassification = &c
	}
	w.LastEpistemicKind = epistemic
	return w
}

func TestBuildAgentRows_OrdersByUrgency(t *testing.T) {
	rows := BuildAgentRows([]domain.WatcherRecord{
		watcherRec("wch_working", string(domain.ClassStillWorking), nil),
		watcherRec("wch_input", string(domain.ClassWaitingForInput), nil),
	}, nil, nil)
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	// Needs-input sorts ahead of still-working.
	if rows[0].ID != "wch_input" {
		t.Errorf("rows[0] = %q, want wch_input (needs-input first)", rows[0].ID)
	}
	if !rows[0].NeedsAttention {
		t.Error("needs-input row should flag NeedsAttention")
	}
}

func TestBuildAgentRows_PrefersPersistedEpistemicKind(t *testing.T) {
	// still_working derives "observed" (deterministic working bypass), but a stored
	// kind is authoritative — here a persisted "inferred" overrides the derivation.
	inf := domain.EpistemicInferred
	rows := BuildAgentRows([]domain.WatcherRecord{
		watcherRec("a", string(domain.ClassStillWorking), &inf),
	}, nil, nil)
	if rows[0].EpistemicKind != domain.EpistemicInferred {
		t.Errorf("epistemicKind = %q, want inferred (persisted wins)", rows[0].EpistemicKind)
	}
}

func TestBuildAgentRows_ClassificationFallback(t *testing.T) {
	cases := []struct {
		classification string
		want           domain.EpistemicKind
	}{
		{string(domain.ClassTerminalExited), domain.EpistemicObserved},
		{string(domain.ClassStillWorking), domain.EpistemicObserved},
		{string(domain.ClassUnknown), domain.EpistemicUnverified},
	}
	for _, c := range cases {
		rows := BuildAgentRows([]domain.WatcherRecord{watcherRec("x", c.classification, nil)}, nil, nil)
		if rows[0].EpistemicKind != c.want {
			t.Errorf("classification %q → %q, want %q", c.classification, rows[0].EpistemicKind, c.want)
		}
	}
}

func TestBuildAgentRows_RawEpistemicPassthrough(t *testing.T) {
	// A corrupt stored kind degrades safely (epistemicTag returns "" → no tag) but is
	// not re-validated away — the row keeps it verbatim.
	bogus := domain.EpistemicKind("bogus")
	rows := BuildAgentRows([]domain.WatcherRecord{watcherRec("a", string(domain.ClassStillWorking), &bogus)}, nil, nil)
	if rows[0].EpistemicKind != "bogus" {
		t.Errorf("raw epistemicKind = %q, want passthrough bogus", rows[0].EpistemicKind)
	}
}
