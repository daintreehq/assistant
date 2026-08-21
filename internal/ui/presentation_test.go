package ui

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
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

// TestPresentTool_NoRawDottedNames guards against the presentation gap that motivated
// the verb-map expansion: a first-party tool that renders its raw dotted name (e.g.
// "scratch.create") next to nicely-verbed neighbours ("Delegated"). Every canonical
// local tool name should resolve to a human label — never a string still carrying a
// dot from its namespace.
func TestPresentTool_NoRawDottedNames(t *testing.T) {
	names := []string{
		"scratch.create", "scratch.set", "scratch.get", "scratch.delete", "scratch.drop",
		"terminal.awaitAll", "terminal.sendCommand", "terminal.rename", "terminal.close",
		"terminal.arm", "terminal.disarm", "terminal.disarmAll", "terminal.moveToWorktree",
		"terminal.run.async", "terminal.await.async", "async.list", "async.cancel",
		"memory.recall", "memory.list", "memory.save", "memory.forget", "memory.pin", "memory.unpin",
		"artifact.read", "copyTree.generate", "copyTree.injectToTerminal",
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
