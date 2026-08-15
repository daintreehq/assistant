package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/markdown"
	"github.com/daintreehq/assistant/internal/ui/theme"
)

// render_interject_test.go covers the inline card the cockpit folds into a running turn
// when the human types while the model is working (StepInterject).

// The reported failure: a message typed mid-stream rendered as a single bar'd row butted
// straight against the paragraph above it, so it read as one more line of the model's own
// prose. It must own a blank line on BOTH sides.
func TestStepInterject_BreathesAboveAndBelow(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID:    "turn_interject_gap",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "All five agent terminals are idle at prompts, left open."},
			{Kind: StepInterject, Text: "Please close all the terminals"},
			{Kind: StepProse, Text: "Closing them now."},
		},
	}
	lines := strings.Split(stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false)), "\n")
	label, body := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, interjectLabel) {
			label = i
		}
		if strings.Contains(ln, "Please close all the terminals") {
			body = i
		}
	}
	if label < 1 || body <= label {
		t.Fatalf("could not locate the card rows (label=%d body=%d): %q", label, body, lines)
	}
	if strings.TrimSpace(lines[label-1]) != "" {
		t.Errorf("expected a blank line ABOVE the mid-turn card, got %q — it reads as the model's own prose", lines[label-1])
	}
	if body+1 >= len(lines) || strings.TrimSpace(lines[body+1]) != "" {
		t.Errorf("expected a blank line BELOW the mid-turn card, got %q", lines[body:])
	}
}

// The card must survive as the human's own surface: the same accent bar the YOU card uses,
// plus an anchor naming WHEN it arrived (the one thing its position cannot say).
func TestInterjectionCard_AnchorAndBar(t *testing.T) {
	th := darkTheme()
	out := renderInterjection(th, "stop after round 2", 60)
	rows := strings.Split(out, "\n")
	if len(rows) != 2 {
		t.Fatalf("a one-line message must render as anchor + body, got %d rows: %q", len(rows), ansi.Strip(out))
	}
	if !strings.Contains(ansi.Strip(out), interjectLabel) {
		t.Errorf("card missing its %q anchor: %q", interjectLabel, ansi.Strip(out))
	}
	if !strings.Contains(ansi.Strip(out), "stop after round 2") {
		t.Errorf("card must carry the message text: %q", ansi.Strip(out))
	}
	for i, row := range rows {
		if !strings.HasPrefix(ansi.Strip(row), th.Glyphs.Bar) {
			t.Errorf("row %d must lead with the accent bar %q: %q", i, th.Glyphs.Bar, ansi.Strip(row))
		}
	}
}

// No card row may exceed the width at ANY width, so a card committed to native scrollback
// can never wrap a frozen row when the host terminal shrinks. The floor cases are the ones
// that bite: chromeWidth() floors at 1, and a card's chrome alone (bar + gap + one content
// cell) is 3 cells — so widths 1-2 must drop the chrome rather than overflow. This also
// covers the old interjection renderer's fixed `inner >= 10` floor, which overflowed far
// wider terminals than that.
func TestCards_RowsFitEveryWidth(t *testing.T) {
	th := darkTheme()
	long := "please close every terminal you opened and summarize what each one did"
	cards := map[string]func(w int) string{
		"interjection": func(w int) string { return renderInterjection(th, long, w) },
		"skill":        func(w int) string { return renderSkillCard(th, long, w) },
		"you":          func(w int) string { return renderUserMessage(th, long, w) },
		"queued":       func(w int) string { return renderQueuedInjections(th, []string{long, long}, w, 99) },
	}
	for _, w := range []int{1, 2, 3, 4, 5, 6, 8, 12, 20, 40, 72, 120} {
		for name, render := range cards {
			for i, row := range strings.Split(render(w), "\n") {
				if got := ansi.StringWidth(row); got > w {
					t.Errorf("%s at width %d: row %d is %d cells (overflows, would wrap a frozen row): %q",
						name, w, i, got, ansi.Strip(row))
				}
			}
		}
	}
}

// A step that renders NOTHING must not eat the blank line a card is owed. A whitespace-only
// prose step is reachable (appendProse rejects only ""), and keying the separator off the
// raw previous step let one stand between an interjection and whatever followed, silently
// gluing them together.
func TestStepInterject_GapSurvivesANonRenderingStep(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID:    "turn_ij_ghost",
		State: TurnComplete,
		Steps: []TurnStep{
			{Kind: StepInterject, Text: "close them"},
			{Kind: StepProse, Text: "   "}, // renders nothing
			{Kind: StepSkill, Text: "Tear down a workspace safely"},
		},
	}
	lines := strings.Split(stripAnsi(renderTurnSteps(th, markdown.New(th), turn, 0, -1, 72, 70, false, 0, 1, false)), "\n")
	card := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Skill loaded") {
			card = i
		}
	}
	if card < 1 {
		t.Fatalf("could not locate the skill card: %q", lines)
	}
	if strings.TrimSpace(lines[card-1]) != "" {
		t.Errorf("a non-rendering step swallowed the blank owed below the mid-turn card: %q", lines)
	}
}

// A long paste typed mid-turn is trimmed to head + "N lines hidden" + tail, exactly like
// the YOU card — otherwise it commits its full height into scrollback and buries the turn.
func TestInterjectionCard_LongPasteIsTrimmed(t *testing.T) {
	th := darkTheme()
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, "log line")
	}
	out := stripAnsi(renderInterjection(th, strings.Join(lines, "\n"), 72))
	rows := strings.Split(out, "\n")
	// anchor + head + rule + tail.
	if want := 1 + userMsgHeadLines + 1 + userMsgTailLines; len(rows) != want {
		t.Errorf("long paste must collapse to %d rows, got %d", want, len(rows))
	}
	if !strings.Contains(out, "lines hidden") {
		t.Errorf("trimmed card missing the \"N lines hidden\" rule: %q", out)
	}
}

// An injection folds in at the TOP of a round, so it can be the turn's FIRST step — before
// any prose or tool has rendered. The blank line is what separates it from the ◆ DAINTREE
// marker there; without it the card butts straight against the marker.
func TestStepInterject_BreathesUnderTheMarker(t *testing.T) {
	th := darkTheme()
	turn := &TurnCell{
		ID: "turn_ij_first", UserText: "start the work", State: TurnActive,
		Phase: domain.PhaseGenerating,
		Steps: []TurnStep{{Kind: StepInterject, Text: "actually, hold on"}},
	}
	lines := strings.Split(stripAnsi(renderTurn(th, markdown.New(th), turn, 72, 70, false, 0, 1)), "\n")
	marker, card := -1, -1
	for i, ln := range lines {
		if strings.Contains(ln, "DAINTREE") {
			marker = i
		}
		if strings.Contains(ln, interjectLabel) {
			card = i
		}
	}
	if marker < 0 || card < 0 {
		t.Fatalf("could not locate the marker (%d) and card (%d): %q", marker, card, lines)
	}
	if card != marker+2 || strings.TrimSpace(lines[marker+1]) != "" {
		t.Errorf("a first-step mid-turn card must be separated from the marker by one blank row: %q", lines)
	}
}

// The realistic lifecycle: prose streams and flushes, THEN the message folds in, then the
// model answers. Every row must reconcile byte-for-byte across flush + seal — the card's
// two new blank rows included, which a token-count assertion would not catch.
func TestInterject_ArrivingMidFlushReconcilesByteExact(t *testing.T) {
	turn := &TurnCell{
		ID: "turn_ij_life", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseGenerating,
		Steps: []TurnStep{
			proseStep("ALPHAPARA a completed first paragraph.\n\nBETALIVE still forming", true),
		},
	}
	m := armedModel(turn)
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the completed paragraph must flush before the interjection arrives")
	}
	flushedBefore := turn.flushedRowsText
	if !strings.Contains(ansi.Strip(flushedBefore), "ALPHAPARA") {
		t.Fatalf("test setup: the first paragraph did not flush:\n%s", ansi.Strip(flushedBefore))
	}

	// The message folds in mid-turn, then the model responds and the turn seals.
	turn.sealProse()
	turn.Steps = append(turn.Steps,
		TurnStep{Kind: StepInterject, Text: "INTERJECTX close them"},
		proseStep("GAMMAPARA closing them now.", false))

	rows := m.activeTurnRows(turn) // the live footer render this frame
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the card and the prose that closed it must flush")
	}
	// The already-committed prefix never moved, and the new prefix is a byte-exact ROW
	// PREFIX of the footer render — so scrollback and the live footer cannot disagree.
	if !strings.HasPrefix(turn.flushedRowsText, flushedBefore) {
		t.Errorf("the flush frontier rewrote already-committed rows:\nbefore:\n%q\nafter:\n%q", flushedBefore, turn.flushedRowsText)
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
	// And the card kept exactly one blank row on each side through the whole lifecycle.
	sealed := strings.Split(ansi.Strip(strings.Join(sealedRows, "\n")), "\n")
	label, body := -1, -1
	for i, ln := range sealed {
		if strings.Contains(ln, interjectLabel) {
			label = i
		}
		if strings.Contains(ln, "INTERJECTX") {
			body = i
		}
	}
	if label < 1 || body != label+1 {
		t.Fatalf("could not locate the sealed card rows (label=%d body=%d): %q", label, body, sealed)
	}
	if strings.TrimSpace(sealed[label-1]) != "" || strings.TrimSpace(sealed[label-2]) == "" {
		t.Errorf("want EXACTLY one blank row above the sealed card: %q", sealed)
	}
	if body+2 >= len(sealed) || strings.TrimSpace(sealed[body+1]) != "" || strings.TrimSpace(sealed[body+2]) == "" {
		t.Errorf("want EXACTLY one blank row below the sealed card: %q", sealed)
	}
}

// The card's leading blank is a new row in the canonical turn rows, so it has to hold the
// flush contract: what the incremental flush commits to scrollback must stay a row-exact
// PREFIX of the live footer render, and the seal must not re-commit any of it.
func TestInterject_FlushPrefixAndSealReconcile(t *testing.T) {
	turn := &TurnCell{
		ID: "turn_ij_flush", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseGenerating,
		Steps: []TurnStep{
			proseStep("ALPHALINE the first answer.", false),
			{Kind: StepInterject, Text: "INTERJECTX close them"},
			proseStep("GAMMALIVE still going", true),
		},
	}
	m := armedModel(turn)

	final := m.activeTurnFinalRows(turn)
	full := dropTrailingStatusRows(m.activeTurnRows(turn))
	for i := range final {
		if i >= len(full) {
			t.Fatalf("flush prefix (%d rows) runs past the live render (%d rows)", len(final), len(full))
		}
		if full[i] != final[i] {
			t.Fatalf("flush row %d diverges from the live footer:\nflush:  %q\nfooter: %q",
				i, ansi.Strip(final[i]), ansi.Strip(full[i]))
		}
	}

	flushedChunk := ansi.Strip(strings.Join(final[turn.FlushedRows:], "\n"))
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("a turn whose interjection card has closed must flush")
	}
	turn.sealProse()
	turn.State = TurnComplete
	combined := flushedChunk + "\n" + ansi.Strip(m.sealedBlock(0).Rendered)
	for _, w := range []string{"QUESTIONX", "ALPHALINE", interjectLabel, "INTERJECTX", "GAMMALIVE"} {
		if n := strings.Count(combined, w); n != 1 {
			t.Errorf("token %q appears %d times across flush+seal, want exactly 1", w, n)
		}
	}
}

// The FILL BLOCK is the whole point of making this a card — a lone bar read like another
// branch of the tool tree, which is exactly why the YOU card grew a fill. Pin it per mode
// (mirrors TestUserMessageBackgroundByMode): a background SGR on every row in the color
// modes, none in the no-background modes, and a full-width block either way.
func TestInterjectionCard_FillBlockByMode(t *testing.T) {
	const w = 40
	hasBG := func(s string) bool {
		return strings.Contains(s, "\x1b[48;") || strings.Contains(s, ";48;")
	}
	var rowCount int
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		out := renderInterjection(uiTheme(mode), "close every terminal you opened", w)
		rows := strings.Split(out, "\n")
		// The row count must not move with the theme, or a mode switch would reflow a card
		// already frozen in scrollback.
		if rowCount == 0 {
			rowCount = len(rows)
		} else if len(rows) != rowCount {
			t.Errorf("mode=%v: card is %d rows, want %d (stable across modes)", mode, len(rows), rowCount)
		}
		fill := mode == theme.ModeDark || mode == theme.ModeLight
		for i, row := range rows {
			if got := hasBG(row); got != fill {
				t.Errorf("mode=%v row %d: background fill = %v, want %v: %q", mode, i, got, fill, row)
			}
			if fill && cellWidth(row) != w-1 {
				t.Errorf("mode=%v row %d: width %d, want %d (full block)", mode, i, cellWidth(row), w-1)
			}
		}
	}
	// The inline anchor is bold — it has to out-rank the body rows beside it on the fill.
	// Bold can be the SGR's only param, its first, or a later one.
	label := strings.SplitN(renderInterjection(uiTheme(theme.ModeDark), "x", w), "\n", 2)[0]
	if !strings.Contains(label, "\x1b[1m") && !strings.Contains(label, "\x1b[1;") && !strings.Contains(label, ";1m") {
		t.Errorf("the inline anchor must be bold: %q", label)
	}
}

// InterjectionSurface is the YOU card's palette with ONE change — a brighter anchor, because
// the anchor rides INSIDE the fill here instead of floating above it. Everything else must
// stay identical, or the human's words would read as two different voices.
func TestInterjectionSurface_OnlyTheAnchorDiffers(t *testing.T) {
	for _, mode := range []theme.Mode{theme.ModeDark, theme.ModeLight, theme.ModeANSI, theme.ModeNone} {
		you := uiTheme(mode).UserMessageSurface()
		mid := uiTheme(mode).InterjectionSurface()
		if you.Bar != mid.Bar || you.Text != mid.Text || you.Fill != mid.Fill {
			t.Errorf("mode=%v: bar/text/fill must match the YOU card; you=%+v mid=%+v", mode, you, mid)
		}
		// Only the color modes have a fill for the anchor to sit on, so only they retune it.
		if fill := mode == theme.ModeDark || mode == theme.ModeLight; fill && you.Label == mid.Label {
			t.Errorf("mode=%v: the inline anchor must be brighter than the YOU card's floating one", mode)
		}
	}
}

func TestInterjectionCard_BlankTextRendersNothing(t *testing.T) {
	if got := renderInterjection(darkTheme(), "  \n\n ", 60); got != "" {
		t.Errorf("blank interjection must render nothing, got %q", got)
	}
}
