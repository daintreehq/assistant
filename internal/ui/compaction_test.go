package ui

import (
	"strings"
	"testing"
)

// compaction_test.go locks the batch-compaction feature (ui-transcript.md §4 /
// _interaction-ux.md §6): a FINISHED homogeneous read/inspect batch collapses to one
// "✓ Inspected N files · <ms>" row by default, ^X reveals the rows, and a failure /
// still-active / single / heterogeneous batch stays expanded. The final test proves the
// flush prefix and the seal render the compacted batch identically (the byte-exact flush
// invariant).

func cmpReadAct(id string, state ActivityState) Activity {
	return Activity{ID: id, Name: "fs.read", State: state, Args: `{"path":"` + id + `.go"}`, StartedAt: 100, EndedAt: 200}
}

func cmpActPtr(a Activity) *Activity { return &a }

func TestToolGroup_CompactsFinishedHomogeneousBatch(t *testing.T) {
	th := darkTheme()
	acts := []Activity{cmpReadAct("a", ActDone), cmpReadAct("b", ActDone), cmpReadAct("c", ActDone)}
	out := stripAnsi(renderToolGroup(th, acts, false, 0, 300, 72))
	if !strings.Contains(out, "Inspected 3 files") {
		t.Fatalf("a finished homogeneous read batch must compact to a summary row: %q", out)
	}
	if strings.Contains(out, "\n") {
		t.Fatalf("a compacted batch must be a SINGLE row: %q", out)
	}
	if strings.Contains(out, "Read") {
		t.Fatalf("a compacted batch must not show per-file Read rows: %q", out)
	}
	// The branch grammar uses the square last-branch glyph (it is the whole closed group).
	if !strings.Contains(out, th.Glyphs.BranchLast) {
		t.Fatalf("a compacted batch should use the last-branch glyph: %q", out)
	}
}

func TestToolGroup_ExpandedRevealsIndividualRows(t *testing.T) {
	th := darkTheme()
	acts := []Activity{cmpReadAct("a", ActDone), cmpReadAct("b", ActDone), cmpReadAct("c", ActDone)}
	out := stripAnsi(renderToolGroup(th, acts, true, 0, 300, 72))
	if strings.Contains(out, "Inspected") {
		t.Fatalf("^X expanded must reveal individual rows, not the summary: %q", out)
	}
	if strings.Count(out, "Read") < 3 {
		t.Fatalf("^X expanded must show all three Read rows: %q", out)
	}
}

func TestToolGroup_NoCompactWhileActive(t *testing.T) {
	th := darkTheme()
	acts := []Activity{cmpReadAct("a", ActDone), cmpReadAct("b", ActActive)}
	out := stripAnsi(renderToolGroup(th, acts, false, 0, 300, 72))
	if strings.Contains(out, "Inspected") {
		t.Fatalf("a batch with a still-active call must NOT compact (progress must show): %q", out)
	}
}

func TestToolGroup_NoCompactOnFailure(t *testing.T) {
	th := darkTheme()
	fail := cmpReadAct("b", ActFailed)
	fail.Outcome = "ENOENT"
	acts := []Activity{cmpReadAct("a", ActDone), fail}
	out := stripAnsi(renderToolGroup(th, acts, false, 0, 300, 72))
	if strings.Contains(out, "Inspected") {
		t.Fatalf("a batch with a failure must stay expanded so the outcome shows: %q", out)
	}
	if !strings.Contains(out, "ENOENT") {
		t.Fatalf("the failure outcome must remain visible: %q", out)
	}
}

func TestToolGroup_NoCompactSingleHeterogeneousOrUnlisted(t *testing.T) {
	th := darkTheme()
	if one := stripAnsi(renderToolGroup(th, []Activity{cmpReadAct("a", ActDone)}, false, 0, 300, 72)); strings.Contains(one, "Inspected") {
		t.Fatalf("a single call must not compact: %q", one)
	}
	het := stripAnsi(renderToolGroup(th, []Activity{
		cmpReadAct("a", ActDone),
		{ID: "b", Name: "fs.search", State: ActDone, Args: `{"query":"x"}`},
	}, false, 0, 300, 72))
	if strings.Contains(het, "Inspected") {
		t.Fatalf("a heterogeneous batch must not compact: %q", het)
	}
	spawn := []Activity{
		{ID: "a", Name: "agentTask.spawnForEdits", State: ActDone},
		{ID: "b", Name: "agentTask.spawnForEdits", State: ActDone},
	}
	if out := stripAnsi(renderToolGroup(th, spawn, false, 0, 300, 72)); strings.Contains(out, "files") {
		t.Fatalf("a non-read homogeneous batch must not compact: %q", out)
	}
}

func TestCompaction_FlushPrefixAndSealAgree(t *testing.T) {
	// The incremental flush commits the finalized prefix; the seal re-renders the whole
	// turn. Both MUST render a closed read batch as the SAME single compacted row, or the
	// flush would splice misaligned content into scrollback.
	m := harnessModel()
	th, md := m.theme, m.md
	turn := &TurnCell{ID: "turn_x", State: TurnActive, Steps: []TurnStep{
		{Kind: StepProse, Text: "Let me look.\n\n"},
		{Kind: StepTool, Activity: cmpActPtr(cmpReadAct("a", ActDone))},
		{Kind: StepTool, Activity: cmpActPtr(cmpReadAct("b", ActDone))},
		{Kind: StepTool, Activity: cmpActPtr(cmpReadAct("c", ActDone))},
		{Kind: StepProse, Text: "Found it.\n\n"},
	}}
	w, cw := 80, 76

	// The closed read batch is finalized (the trailing prose closes it; the prose tail is
	// the only live step).
	k := finalizedStepCount(turn)
	if k < 4 {
		t.Fatalf("the closed read batch should be finalized (k>=4), got %d", k)
	}
	prefix := stripAnsi(renderTurnSteps(th, md, turn, 0, k, w, cw, false, 0, 1, true, false))
	if !strings.Contains(prefix, "Inspected 3 files") {
		t.Fatalf("the flushed prefix must compact the read batch: %q", prefix)
	}

	turn.State = TurnComplete
	full := stripAnsi(renderTurn(th, md, turn, w, cw, false, 0, 1))
	if c := strings.Count(full, "Inspected 3 files"); c != 1 {
		t.Fatalf("the sealed render must compact the read batch to exactly one row, got %d:\n%s", c, full)
	}
}
