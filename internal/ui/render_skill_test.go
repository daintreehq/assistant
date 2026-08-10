package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/ui/markdown"
)

// render_skill_test.go covers the inline "Skill loaded" card the cockpit folds into a
// running turn when the backend's selector loads a runbook (StepSkill).

func TestSkillCard_LabelAndFullName(t *testing.T) {
	out := stripAnsi(renderSkillCard(darkTheme(), "Orchestrate multiple agents on one problem", 60))
	rows := strings.Split(out, "\n")
	if len(rows) < 2 {
		t.Fatalf("skill card must be at least two rows, got %d: %q", len(rows), out)
	}
	if !strings.Contains(out, "Skill loaded") {
		t.Errorf("card missing the \"Skill loaded\" anchor: %q", out)
	}
	// The FULL name survives (no ellipsis truncation) at a comfortable width.
	if !strings.Contains(out, "Orchestrate multiple agents on one problem") {
		t.Errorf("card must carry the full skill name: %q", out)
	}
}

func TestSkillCard_LongNameWrapsKeepingEveryWord(t *testing.T) {
	name := "Prepare a branch for review by running the full gate suite and summarizing the diff"
	out := stripAnsi(renderSkillCard(darkTheme(), name, 40))
	// Drop the anchor row, re-flatten the wrapped name rows: every word of the full name
	// must survive the wrap (wrapped across rows, never truncated away).
	flat := strings.Join(strings.Fields(strings.ReplaceAll(out, "Skill loaded", "")), " ")
	for _, w := range strings.Fields(name) {
		if !strings.Contains(flat, w) {
			t.Errorf("wrapped card dropped word %q: %q", w, flat)
		}
	}
}

func TestSkillCard_EmptyTitleRendersNothing(t *testing.T) {
	if got := renderSkillCard(darkTheme(), "   ", 60); got != "" {
		t.Errorf("blank title must render nothing, got %q", got)
	}
}

func TestStepSkill_LinkedAboveBreathesBelow(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID:    "turn_skillgap",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "Before."},
			{Kind: StepSkill, Text: "Orchestrate multiple agents"},
			{Kind: StepProse, Text: "After."},
		},
	}
	lines := strings.Split(stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false)), "\n")
	label, name := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "Skill loaded") {
			label = i
		}
		if strings.Contains(ln, "Orchestrate multiple agents") {
			name = i
		}
	}
	if label < 1 || name <= label {
		t.Fatalf("could not locate the card rows (label=%d name=%d): %q", label, name, lines)
	}
	// NO blank line above the card: it stays linked to the content above it (here the
	// prose, in a real turn the ◆ DAINTREE marker), matching the YOU card's space-below-
	// only convention.
	if strings.TrimSpace(lines[label-1]) == "" {
		t.Errorf("expected the card glued to the row above (no blank), got blank at %d", label-1)
	}
	// A blank line BELOW the card (between the name row and the prose after it).
	if name+1 >= len(lines) || strings.TrimSpace(lines[name+1]) != "" {
		t.Errorf("expected a blank line below the card, got %q", lines[name:])
	}
}

func TestStepSkill_RendersInlineBetweenProse(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID:    "turn_skilltest",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "Working on it."},
			{Kind: StepSkill, Text: "Orchestrate multiple agents"},
			{Kind: StepProse, Text: "Now spawning agents."},
		},
	}
	out := stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false))
	// The card sits chronologically BETWEEN the two prose steps (the bug we fixed: it used
	// to land as a trailing footer note pinned to the bottom, out of place).
	iFirst := strings.Index(out, "Working on it")
	iCard := strings.Index(out, "Skill loaded")
	iName := strings.Index(out, "Orchestrate multiple agents")
	iSecond := strings.Index(out, "Now spawning agents")
	if iCard < 0 || iName < 0 {
		t.Fatalf("StepSkill must render inline as a card: %q", out)
	}
	if !(iFirst >= 0 && iFirst < iCard && iCard < iSecond) {
		t.Errorf("card must sit between the prose steps (first=%d card=%d second=%d): %q", iFirst, iCard, iSecond, out)
	}
}
