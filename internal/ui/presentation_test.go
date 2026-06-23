package ui

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
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
		// Pulling a runbook reads as "Loading skill" (both the NL find and the by-id load).
		"skill.find": "Loading skill",
		"skill.load": "Loading skill",
	}
	for name, want := range cases {
		if got := presentTool(name); got != want {
			t.Errorf("presentTool(%q) = %q, want %q", name, got, want)
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

// A settled skill.find/skill.load that loaded a runbook earns the ✦ skill glyph; a
// find that matched nothing keeps the plain ✓ (the sparkle must mean "skill now live").
func TestSkillLoadedGlyph(t *testing.T) {
	th := theme.Resolve()
	loaded := Activity{Name: "skill.find", State: ActDone, Detail: "Orchestrate multiple agents on one problem"}
	if !skillLoaded(loaded) {
		t.Fatal("skill.find that resolved to a titled skill should count as loaded")
	}
	if g, _ := activityGlyph(th, loaded, 0); g != th.Glyphs.Skill {
		t.Errorf("loaded-skill glyph = %q, want Skill glyph %q", g, th.Glyphs.Skill)
	}
	nomatch := Activity{Name: "skill.find", State: ActDone, Detail: skillNoMatchSummary}
	if skillLoaded(nomatch) {
		t.Fatal("a no-match find must NOT count as loaded")
	}
	if g, _ := activityGlyph(th, nomatch, 0); g != th.Glyphs.Done {
		t.Errorf("no-match glyph = %q, want plain Done glyph %q", g, th.Glyphs.Done)
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
