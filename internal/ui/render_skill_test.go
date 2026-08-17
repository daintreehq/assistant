package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/markdown"
)

// render_skill_test.go covers the inline skill card the cockpit folds into a running
// turn when the backend's selector loads runbooks (StepSkill): ONE card per contiguous
// run, a "Skill loaded"/"Skills loaded" anchor that pluralizes on the run's size, one
// row per skill, each name truncated to its row (never wrapped).

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
	if got := strings.Count(out, "Skills loaded"); got != 1 {
		t.Errorf("card must carry exactly one PLURAL anchor, got %d: %q", got, out)
	}
	for i, title := range titles {
		if !strings.Contains(rows[1+i], title) {
			t.Errorf("row %d must carry skill %q: %q", 1+i, title, rows[1+i])
		}
	}
}

// The anchor pluralizes on the run's size: one skill reads "Skill loaded", several read
// "Skills loaded".
func TestSkillCard_AnchorPluralizes(t *testing.T) {
	one := stripAnsi(renderSkillCard(darkTheme(), []string{"Only skill"}, 60))
	if !strings.Contains(one, "Skill loaded") || strings.Contains(one, "Skills loaded") {
		t.Errorf("a single skill must carry the singular anchor: %q", one)
	}
	two := stripAnsi(renderSkillCard(darkTheme(), []string{"First skill", "Second skill"}, 60))
	if !strings.Contains(two, "Skills loaded") || strings.Contains(two, "Skill loaded") {
		t.Errorf("multiple skills must carry the plural anchor: %q", two)
	}
	// Blank titles don't count toward the plural: one real title stays singular.
	mixed := stripAnsi(renderSkillCard(darkTheme(), []string{"  ", "Only skill", ""}, 60))
	if !strings.Contains(mixed, "Skill loaded") || strings.Contains(mixed, "Skills loaded") {
		t.Errorf("blank titles must not pluralize the anchor: %q", mixed)
	}
}

// A long name occupies EXACTLY one row, cut with an ellipsis — never wrapped onto more
// rows: one line per skill is the card's whole display contract, however long the name.
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
// however many skills load — listing every skill one row each is the card's contract,
// and a trim would hide the very names the card exists to show. This pins the
// renderCard collapse=false wiring.
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
	// Crossing the collapse threshold must not restructure the card: with the anchor
	// already plural on both sides, one more skill only appends a row.
	part := renderSkillCard(darkTheme(), titles[:n-1], 60)
	if part == "" || !strings.HasPrefix(full, part+"\n") {
		t.Errorf("crossing the collapse threshold must only append a row:\npart: %q\nfull: %q", part, full)
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
	if got := strings.Count(out, "Skills loaded"); got != 1 {
		t.Fatalf("a contiguous skill run must render ONE plural anchor, got %d: %q", got, out)
	}
	lines := strings.Split(out, "\n")
	label := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Skills loaded") {
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
	if got := strings.Count(out, "Skills loaded"); got != 2 {
		t.Fatalf("two runs must render two plural anchors, got %d: %q", got, out)
	}
	lines := strings.Split(out, "\n")
	var anchors []int
	for i, ln := range lines {
		if strings.Contains(ln, "Skills loaded") {
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
// then prose streams, then the turn seals. Because the anchor pluralizes on the run's
// size, the card is a function of the WHOLE run — so the flush must commit it
// ATOMICALLY: nothing of the card while the run is the live tail, the whole card once a
// non-skill step closes it, and every committed row byte-exact through the seal.
func TestSkillRun_FlushCommitsCardAtomically(t *testing.T) {
	turn := &TurnCell{
		ID: "turn_skill_life", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseGenerating,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: "ALPHASKILL spawn a visible agent"},
			{Kind: StepSkill, Text: "BETASKILL orchestration foundation"},
			{Kind: StepSkill, Text: "GAMMASKILL plain worktree"},
		},
	}
	m := armedModel(turn)
	// While the run is the turn's live tail it may still grow, so NO card row commits —
	// only the preamble (YOU card + marker) is flushable this frame.
	m.flushActiveTurn()
	flushedBefore := turn.flushedRowsText
	if s := stripAnsi(flushedBefore); strings.Contains(s, "Skill") || strings.Contains(s, "ALPHASKILL") {
		t.Fatalf("no card row may flush while the run is still open:\n%s", s)
	}

	// The round's prose closes the run; the WHOLE card flushes behind it (the short
	// final prose row itself stays withheld as the live tail — the card is what commits).
	turn.Steps = append(turn.Steps, proseStep("DELTAPROSE spawning now.", false))
	rows := m.activeTurnRows(turn) // the live footer render this frame
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the closed run must flush")
	}
	if !strings.HasPrefix(turn.flushedRowsText, flushedBefore) {
		t.Errorf("the flush frontier rewrote already-committed rows:\nbefore:\n%q\nafter:\n%q", flushedBefore, turn.flushedRowsText)
	}
	committed := stripAnsi(turn.flushedRowsText)
	if !strings.Contains(committed, "Skills loaded") ||
		!strings.Contains(committed, "ALPHASKILL") || !strings.Contains(committed, "GAMMASKILL") {
		t.Errorf("the closed run must commit whole, plural anchor included:\n%s", committed)
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
	if strings.Count(sealed, "Skills loaded") != 1 || strings.Count(sealed, "Skill loaded") != 0 {
		t.Errorf("the sealed run must carry exactly ONE plural anchor and no singular one:\n%s", sealed)
	}
	for _, w := range []string{"ALPHASKILL", "BETASKILL", "GAMMASKILL"} {
		if n := strings.Count(sealed, w); n != 1 {
			t.Errorf("skill %q appears %d times across the sealed turn, want exactly 1", w, n)
		}
	}
}

// A turn that ENDS on an open skill run (cancelled or failed before any prose) must
// still seal the whole card exactly once: nothing committed while the run was the live
// tail, and the seal — finalizedStepCount returns everything for a non-active turn —
// emits the card with its plural anchor.
func TestSkillRun_OpenAtCancelSealsOnce(t *testing.T) {
	turn := &TurnCell{
		ID: "turn_skill_cancel", UserText: "QUESTIONX", State: TurnActive,
		Steps: []TurnStep{
			{Kind: StepSkill, Text: "ALPHASKILL one"},
			{Kind: StepSkill, Text: "BETASKILL two"},
		},
	}
	m := armedModel(turn)
	m.flushActiveTurn() // open run: commits the preamble at most
	if s := stripAnsi(turn.flushedRowsText); strings.Contains(s, "ALPHASKILL") || strings.Contains(s, "Skill") {
		t.Fatalf("an open trailing run must not flush any card row: %s", s)
	}
	turn.State = TurnCancelled
	sealedRows := m.activeTurnRows(turn)
	tail := sealTail(sealedRows, turn.flushedRowsText)
	combined := stripAnsi(turn.flushedRowsText + "\n" + tail)
	if strings.Count(combined, "Skills loaded") != 1 ||
		strings.Count(combined, "ALPHASKILL") != 1 || strings.Count(combined, "BETASKILL") != 1 {
		t.Errorf("a cancelled turn must seal the whole card exactly once:\n%s", combined)
	}
}

// finalizedStepCount treats a skill run like a tool run: flushable only once CLOSED by a
// non-skill step, never split — the pluralizing anchor makes the card a function of the
// whole run, so committing part of an open run could freeze a row a later skill rewrites.
func TestFinalize_SkillRunCommitsAtomically(t *testing.T) {
	s := func(txt string) TurnStep { return TurnStep{Kind: StepSkill, Text: txt} }
	cases := []struct {
		name  string
		state TurnState
		steps []TurnStep
		want  int
	}{
		{"open trailing run is withheld whole", TurnActive,
			[]TurnStep{s("a"), s("b"), s("c")}, 0},
		{"closed run finalizes whole", TurnActive,
			[]TurnStep{s("a"), s("b"), s("c"), proseStep("done", false)}, 3},
		{"prose before an open run still finalizes", TurnActive,
			[]TurnStep{proseStep("hi", false), s("a"), s("b")}, 1},
		{"sealed turn finalizes everything", TurnComplete,
			[]TurnStep{s("a"), s("b"), s("c")}, 3},
	}
	for _, tc := range cases {
		turn := &TurnCell{ID: "turn_fin", State: tc.state, Steps: tc.steps}
		if got := finalizedStepCount(turn); got != tc.want {
			t.Errorf("%s: finalizedStepCount = %d, want %d", tc.name, got, tc.want)
		}
	}
}
