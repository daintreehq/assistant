package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/markdown"
)

// render_skill_test.go covers the inline "Skill loaded" card the cockpit folds into a
// running turn when the backend's selector loads runbooks (StepSkill): ONE card per
// contiguous run, one row per skill, each name truncated to its row (never wrapped).

func TestSkillCard_LabelAndFullName(t *testing.T) {
	out := stripAnsi(renderSkillCard(darkTheme(), []string{"Orchestrate multiple agents on one problem"}, 60))
	rows := strings.Split(out, "\n")
	if len(rows) != 2 {
		t.Fatalf("single-skill card must be exactly two rows (anchor + name), got %d: %q", len(rows), out)
	}
	if !strings.Contains(out, "Skill loaded") {
		t.Errorf("card missing the \"Skill loaded\" anchor: %q", out)
	}
	// The FULL name survives (no ellipsis truncation) at a comfortable width.
	if !strings.Contains(out, "Orchestrate multiple agents on one problem") {
		t.Errorf("card must carry the full skill name: %q", out)
	}
}

// A round that loads several skills renders ONE card: a single anchor row, then one row
// per skill — not a two-row card (plus separator blank) per skill.
func TestSkillCard_MultipleSkillsOneRowEach(t *testing.T) {
	titles := []string{
		"Spawn a visible agent for edits or exploration",
		"Daintree orchestration foundation",
		"Create a plain Daintree worktree",
	}
	out := stripAnsi(renderSkillCard(darkTheme(), titles, 72))
	rows := strings.Split(out, "\n")
	if want := 1 + len(titles); len(rows) != want {
		t.Fatalf("card must be anchor + one row per skill (%d rows), got %d: %q", want, len(rows), out)
	}
	if got := strings.Count(out, "Skill loaded"); got != 1 {
		t.Errorf("card must carry exactly one anchor, got %d: %q", got, out)
	}
	for i, title := range titles {
		if !strings.Contains(rows[1+i], title) {
			t.Errorf("row %d must carry skill %q: %q", 1+i, title, rows[1+i])
		}
	}
}

// A long name occupies EXACTLY one row, cut with an ellipsis — never wrapped onto more
// rows. Row-count stability per skill is what lets the flush commit a run's early rows
// while later skills are still appending.
func TestSkillCard_LongNameTruncatesToOneRow(t *testing.T) {
	name := "Prepare a branch for review by running the full gate suite and summarizing the diff"
	out := stripAnsi(renderSkillCard(darkTheme(), []string{name}, 40))
	rows := strings.Split(out, "\n")
	if len(rows) != 2 {
		t.Fatalf("long name must stay on one row (anchor + name), got %d rows: %q", len(rows), out)
	}
	// The fill pads the row to the block width, so trim it before the suffix check.
	if nameRow := strings.TrimRight(rows[1], " "); !strings.HasSuffix(nameRow, "…") {
		t.Errorf("truncated name row must end in an ellipsis: %q", nameRow)
	}
	if !strings.Contains(rows[1], "Prepare a branch") {
		t.Errorf("name row must keep the name's head: %q", rows[1])
	}
	if strings.Contains(rows[1], "the diff") {
		t.Errorf("the name's tail must be cut, not wrapped: %q", rows[1])
	}
}

// The skill card must NEVER take the YOU-card head/tail collapse ("N lines hidden"),
// however many skills load: the collapse is a function of the TOTAL line count, and a run
// can grow after its first rows were flushed — crossing the threshold would rewrite
// committed scrollback rows. This pins the renderCard collapse=false wiring.
func TestSkillCard_ManySkillsNeverCollapse(t *testing.T) {
	n := userMsgHeadLines + userMsgTailLines + 2 // past the YOU-card collapse threshold
	titles := make([]string, n)
	for i := range titles {
		titles[i] = fmt.Sprintf("Skill number %02d", i)
	}
	full := renderSkillCard(darkTheme(), titles, 60)
	out := stripAnsi(full)
	rows := strings.Split(out, "\n")
	if want := 1 + n; len(rows) != want {
		t.Fatalf("card must keep one row per skill (%d rows), got %d: %q", want, len(rows), out)
	}
	if strings.Contains(out, "lines hidden") {
		t.Fatalf("skill card must never take the head/tail collapse: %q", out)
	}
	for i, title := range titles {
		if !strings.Contains(rows[1+i], title) {
			t.Errorf("row %d must carry %q in order: %q", 1+i, title, rows[1+i])
		}
	}
	// Crossing the threshold only APPENDS a row: the n-1 render is a strict row-prefix.
	part := renderSkillCard(darkTheme(), titles[:n-1], 60)
	if part == "" || !strings.HasPrefix(full, part+"\n") {
		t.Errorf("growing past the collapse threshold must only append rows:\npart: %q\nfull: %q", part, full)
	}
}

// A newline smuggled into a title must not break the one-row-per-skill guarantee.
func TestSkillCard_NewlineInTitleFlattensToOneRow(t *testing.T) {
	out := stripAnsi(renderSkillCard(darkTheme(), []string{"Tear down\na workspace safely"}, 60))
	rows := strings.Split(out, "\n")
	if len(rows) != 2 {
		t.Fatalf("multi-line title must flatten to one row, got %d rows: %q", len(rows), out)
	}
	if !strings.Contains(rows[1], "Tear down a workspace safely") {
		t.Errorf("flattened title must read as one line: %q", rows[1])
	}
}

func TestSkillCard_EmptyTitlesRenderNothing(t *testing.T) {
	if got := renderSkillCard(darkTheme(), nil, 60); got != "" {
		t.Errorf("no titles must render nothing, got %q", got)
	}
	if got := renderSkillCard(darkTheme(), []string{"   ", ""}, 60); got != "" {
		t.Errorf("blank titles must render nothing, got %q", got)
	}
	// Blank entries drop out of a mixed run without leaving an empty row, wherever they
	// sit — the real titles keep their relative order.
	out := stripAnsi(renderSkillCard(darkTheme(), []string{"  ", "Real skill", "", "Second skill", " "}, 60))
	rows := strings.Split(out, "\n")
	if len(rows) != 3 {
		t.Fatalf("blank titles must not occupy rows, got %d rows: %q", len(rows), out)
	}
	if !strings.Contains(rows[1], "Real skill") || !strings.Contains(rows[2], "Second skill") {
		t.Errorf("real titles must survive in order: %q", rows)
	}
}

// The card holds its shape at EVERY narrow width (nonempty, one row per skill, no row
// wider than the terminal), and a wide-rune (CJK) title truncates cleanly within budget.
func TestSkillCard_NarrowWidthsAndWideRunes(t *testing.T) {
	th := darkTheme()
	titles := []string{"alpha skill", "beta skill"}
	for w := 1; w <= 40; w++ {
		out := renderSkillCard(th, titles, w)
		if out == "" {
			t.Fatalf("width %d: card must not vanish", w)
		}
		rows := strings.Split(out, "\n")
		if len(rows) != 3 {
			t.Fatalf("width %d: want anchor + 2 rows, got %d: %q", w, len(rows), stripAnsi(out))
		}
		for i, row := range rows {
			if got := cellWidth(row); got > w {
				t.Errorf("width %d: row %d is %d cells (would wrap a frozen row): %q", w, i, got, stripAnsi(row))
			}
		}
	}
	out := stripAnsi(renderSkillCard(th, []string{"日本語のスキルタイトルがとても長い場合の切り詰め"}, 20))
	rows := strings.Split(out, "\n")
	if len(rows) != 2 {
		t.Fatalf("CJK title must stay on one row, got %d: %q", len(rows), out)
	}
	if !strings.Contains(rows[1], "…") {
		t.Errorf("truncated CJK row must carry the ellipsis: %q", rows[1])
	}
	if got := cellWidth(rows[1]); got > 20 {
		t.Errorf("CJK row is %d cells, exceeds width 20: %q", got, rows[1])
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

// A contiguous run of skill steps folds into ONE card: single anchor, one contiguous row
// per skill (no blank separators inside the run), then the usual blank before the prose
// that follows.
func TestStepSkill_ContiguousRunRendersOneCard(t *testing.T) {
	th := darkTheme()
	titles := []string{
		"Spawn a visible agent for edits or exploration",
		"Daintree orchestration foundation",
		"Create a plain Daintree worktree",
	}
	turn := &TurnCell{
		ID:    "turn_skillrun",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: titles[0]},
			{Kind: StepSkill, Text: titles[1]},
			{Kind: StepSkill, Text: titles[2]},
			{Kind: StepProse, Text: "Spawning now."},
		},
	}
	out := stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false))
	if got := strings.Count(out, "Skill loaded"); got != 1 {
		t.Fatalf("a contiguous skill run must render ONE anchor, got %d: %q", got, out)
	}
	lines := strings.Split(out, "\n")
	label := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Skill loaded") {
			label = i
		}
	}
	if label < 0 {
		t.Fatalf("could not locate the anchor row: %q", lines)
	}
	// Anchor + the three name rows are contiguous — no blank separators inside the card.
	for i, title := range titles {
		row := label + 1 + i
		if row >= len(lines) || !strings.Contains(lines[row], title) {
			t.Errorf("expected skill %q on row %d: %q", title, row, lines)
		}
	}
	// The blank + prose follow the last name row.
	after := label + 1 + len(titles)
	if after+1 >= len(lines) || strings.TrimSpace(lines[after]) != "" || !strings.Contains(lines[after+1], "Spawning now") {
		t.Errorf("expected blank + prose below the card, got %q", lines[after:])
	}
}

// Grouping stops at the first non-skill step: two runs separated by a tool step render
// as TWO cards, each with its own anchor and only its own titles, with the standard
// single-blank block separators around the tool row.
func TestStepSkill_RunsSplitByToolRenderTwoCards(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID:    "turn_skillsplit",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: "ALPHASKILL first run one"},
			{Kind: StepSkill, Text: "BETASKILL first run two"},
			toolStep("t1", "fs.read", "read main.go", ActDone),
			{Kind: StepSkill, Text: "GAMMASKILL second run one"},
			{Kind: StepSkill, Text: "DELTASKILL second run two"},
		},
	}
	out := stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false))
	if got := strings.Count(out, "Skill loaded"); got != 2 {
		t.Fatalf("two runs must render two anchors, got %d: %q", got, out)
	}
	lines := strings.Split(out, "\n")
	var anchors []int
	for i, ln := range lines {
		if strings.Contains(ln, "Skill loaded") {
			anchors = append(anchors, i)
		}
	}
	// First card: anchor + its two titles, then ONE blank, then the tool row.
	a1 := anchors[0]
	if !strings.Contains(lines[a1+1], "ALPHASKILL") || !strings.Contains(lines[a1+2], "BETASKILL") {
		t.Errorf("first card must carry only the first run's titles: %q", lines[a1:a1+3])
	}
	if strings.TrimSpace(lines[a1+3]) != "" || strings.TrimSpace(lines[a1+4]) == "" {
		t.Errorf("want exactly one blank between the first card and the tool row: %q", lines[a1+3:a1+5])
	}
	// Second card: ONE blank after the tool row, then anchor + its two titles.
	a2 := anchors[1]
	if a2 != a1+6 || strings.TrimSpace(lines[a2-1]) != "" {
		t.Errorf("want exactly one blank between the tool row and the second card (a1=%d a2=%d): %q", a1, a2, lines)
	}
	if !strings.Contains(lines[a2+1], "GAMMASKILL") || !strings.Contains(lines[a2+2], "DELTASKILL") {
		t.Errorf("second card must carry only the second run's titles: %q", lines[a2:])
	}
}

// A skill run as the turn's FIRST step sits glued directly under the ◆ DAINTREE marker —
// the card takes no blank above (space-below-only, unlike an interjection).
func TestStepSkill_GluedUnderTheMarker(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID: "turn_skillfirst", UserText: "start the work", State: TurnActive,
		Phase: domain.PhaseGenerating,
		Steps: []TurnStep{{Kind: StepSkill, Text: "First skill of the turn"}},
	}
	lines := strings.Split(stripAnsi(renderTurn(th, markdown.New(th), turn, 72, 70, false, 0, 1)), "\n")
	marker, anchor := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "DAINTREE") {
			marker = i
		}
		if strings.Contains(ln, "Skill loaded") {
			anchor = i
		}
	}
	if marker < 0 || anchor < 0 {
		t.Fatalf("could not locate the marker (%d) and anchor (%d): %q", marker, anchor, lines)
	}
	if anchor != marker+1 {
		t.Errorf("a first-step skill card must sit directly under the marker (marker=%d anchor=%d): %q", marker, anchor, lines)
	}
}

// The realistic lifecycle at the top of a round: the backend's meta lands several skills,
// the REAL flush machinery commits the card's first rows while the run's last step is
// still the live one (finalizedStepCount never finalizes the last step), then prose
// streams and the turn seals. Every committed row must survive byte-for-byte — the
// mid-run cut is exactly the case the count-independent one-row-per-skill design exists
// for (a fixed anchor, no collapse, no wrap reflow).
func TestSkillRun_MidRunFlushReconcilesByteExact(t *testing.T) {
	turn := &TurnCell{
		ID: "turn_skill_life", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseGenerating,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: "ALPHASKILL spawn a visible agent"},
			{Kind: StepSkill, Text: "BETASKILL orchestration foundation"},
			{Kind: StepSkill, Text: "GAMMASKILL plain worktree"},
		},
	}
	m := armedModel(turn)
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the run's finalized head (anchor + first two rows) must flush")
	}
	flushedBefore := turn.flushedRowsText
	stripped := stripAnsi(flushedBefore)
	if !strings.Contains(stripped, "ALPHASKILL") || !strings.Contains(stripped, "BETASKILL") {
		t.Fatalf("the first two skill rows did not flush:\n%s", stripped)
	}
	if strings.Contains(stripped, "GAMMASKILL") {
		t.Fatalf("the live last skill step must NOT flush yet:\n%s", stripped)
	}

	// The round's prose closes the run; the remaining row + the prose flush behind it.
	turn.Steps = append(turn.Steps, proseStep("DELTAPROSE spawning now.", false))
	rows := m.activeTurnRows(turn) // the live footer render this frame
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the run's last row and the prose must flush")
	}
	if !strings.HasPrefix(turn.flushedRowsText, flushedBefore) {
		t.Errorf("the flush frontier rewrote already-committed card rows:\nbefore:\n%q\nafter:\n%q", flushedBefore, turn.flushedRowsText)
	}
	if got := strings.Join(rows[:turn.FlushedRows], "\n"); got != turn.flushedRowsText {
		t.Errorf("committed prefix diverged from the footer render:\ncommitted:\n%q\nfooter:\n%q", turn.flushedRowsText, got)
	}

	turn.State = TurnComplete
	sealedRows := m.activeTurnRows(turn)
	tail := sealTail(sealedRows, turn.flushedRowsText)
	if turn.flushedRowsText+"\n"+tail != strings.Join(sealedRows, "\n") {
		t.Errorf("flushed prefix + seal tail does not reconstruct the sealed turn (dup/loss):\nprefix:\n%q\ntail:\n%q", turn.flushedRowsText, tail)
	}
	sealed := stripAnsi(strings.Join(sealedRows, "\n"))
	if got := strings.Count(sealed, "Skill loaded"); got != 1 {
		t.Errorf("the sealed run must carry exactly ONE anchor, got %d:\n%s", got, sealed)
	}
	for _, w := range []string{"ALPHASKILL", "BETASKILL", "GAMMASKILL"} {
		if n := strings.Count(sealed, w); n != 1 {
			t.Errorf("skill %q appears %d times across the sealed turn, want exactly 1", w, n)
		}
	}
}

// The flush frontier can cut a skill run mid-way (finalizedStepCount finalizes all but
// the live last step). The windowed render must be a strict ROW-PREFIX of the full one —
// that is what keeps already-committed scrollback rows byte-exact when the run grows.
func TestStepSkill_WindowCutMidRunIsRowPrefix(t *testing.T) {
	th := darkTheme()
	md := markdown.New(th)
	turn := &TurnCell{
		ID:    "turn_skillprefix",
		State: TurnActive,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: "Spawn a visible agent for edits or exploration"},
			{Kind: StepSkill, Text: "Daintree orchestration foundation"},
			{Kind: StepSkill, Text: "Create a plain Daintree worktree"},
		},
	}
	full := renderTurnSteps(th, md, turn, 0, -1, 72, 70, false, 0, 1, false)
	for _, cut := range []int{1, 2} {
		part := renderTurnSteps(th, md, turn, 0, cut, 72, 70, false, 0, 1, false)
		// STRICT prefix, ending on a row boundary: full continues with a newline, so the
		// cut can neither be empty, nor equal, nor extend the window's final row in place.
		if part == "" || !strings.HasPrefix(full, part+"\n") {
			t.Errorf("window [0,%d) must be a strict row-prefix of the full render:\npart: %q\nfull: %q", cut, part, full)
		}
		if rows := strings.Split(stripAnsi(part), "\n"); len(rows) != 1+cut {
			t.Errorf("window [0,%d) must render anchor + %d rows, got %d: %q", cut, cut, len(rows), rows)
		}
	}
}
