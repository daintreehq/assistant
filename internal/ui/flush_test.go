package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// flush_test.go locks the INCREMENTAL ROW FLUSH (flush.go): the active turn's SETTLED rows
// commit to native scrollback AS THEY STREAM (auto-scroll), while the still-mutable tail renders
// LIVE in the footer (streamed token by token). A PLAIN growing paragraph settles line by line —
// every wrapped row but the last flushes as it forms; a MARKDOWN-risky growing paragraph falls
// back to paragraph-level commit (it flushes only on a completed "\n\n"). Flushed rows carry no
// caret and are byte-identical to the seal's render, so nothing is duplicated; the un-flushed tail
// lives only in the footer (liveCellsView slices off FlushedRows) and is height-bounded by
// lastLines(budget).

func armedModel(turn *TurnCell) Model {
	m := testModel(80)
	m.commitArmed = true
	m.queue.headerDone = true
	m.transcript = []TranscriptCell{{Turn: turn}}
	m.activeTurn = turn.ID
	return m
}

func proseStep(text string, streaming bool) TurnStep {
	return TurnStep{Kind: StepProse, Text: text, Streaming: streaming}
}

func toolStep(id, name, detail string, state ActivityState) TurnStep {
	return TurnStep{Kind: StepTool, Activity: &Activity{ID: id, Name: name, Detail: detail, State: state, EndedAt: 180}}
}

func streamingProse(text string) (Model, *TurnCell) {
	// Phase=Generating so the live "⠋ Writing" status renders alongside the growing paragraph,
	// which streams live in the footer (its settled rows flush; only the still-mutable tail stays).
	turn := &TurnCell{ID: "turn_1", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseGenerating, Steps: []TurnStep{proseStep(text, true)}}
	return armedModel(turn), turn
}

// TestFlush_StableParasFlow proves completed PARAGRAPHS flush to scrollback while streaming
// while the still-growing final paragraph is withheld from the COMMIT but renders live in the
// footer (paragraph-by-paragraph commit; smooth in-flight streaming).
func TestFlush_StableParasFlow(t *testing.T) {
	// Two completed paragraphs + an in-progress third paragraph (no trailing "\n\n").
	m, turn := streamingProse("ALPHALINE first completed paragraph.\n\nBETALINE second completed paragraph.\n\nGAMMALIVE still typing")

	final := m.activeTurnFinalRows(turn)
	flushedChunk := strings.Join(final[turn.FlushedRows:], "\n")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("a streaming turn with completed paragraphs must flush")
	}
	if turn.FlushedRows != len(final) {
		t.Errorf("FlushedRows = %d, want %d (all stable rows)", turn.FlushedRows, len(final))
	}
	// The flushed stable chunk carries the completed paragraphs but NOT the in-progress one.
	if !strings.Contains(ansi.Strip(flushedChunk), "ALPHALINE") || !strings.Contains(ansi.Strip(flushedChunk), "BETALINE") {
		t.Errorf("flushed chunk missing a completed paragraph:\n%s", ansi.Strip(flushedChunk))
	}
	if strings.Contains(ansi.Strip(flushedChunk), "GAMMALIVE") {
		t.Errorf("the in-progress paragraph must NOT be flushed:\n%s", ansi.Strip(flushedChunk))
	}
	if strings.Contains(flushedChunk, "▌") {
		t.Errorf("flushed rows must not carry the caret:\n%s", ansi.Strip(flushedChunk))
	}
	// The flushed paragraphs leave the footer (they live in scrollback now); the still-growing
	// final paragraph renders LIVE in the footer tail and commits only when it completes or seals.
	live := ansi.Strip(m.liveCellsView(m.contentW()))
	if strings.Contains(live, "ALPHALINE") {
		t.Errorf("a flushed paragraph is still in the live footer:\n%s", live)
	}
	if !strings.Contains(live, "GAMMALIVE") {
		t.Errorf("the in-progress paragraph must stream live in the footer:\n%s", live)
	}
}

// TestFlush_NoDupAcrossSeal proves the flushed stable prefix + the sealed tail reconstruct
// the whole turn with every distinctive token EXACTLY ONCE and no caret.
func TestFlush_NoDupAcrossSeal(t *testing.T) {
	m, turn := streamingProse("ALPHALINE first paragraph.\n\nBETALINE second paragraph.\n\nGAMMALIVE final paragraph still going")
	final := m.activeTurnFinalRows(turn)
	flushedChunk := ansi.Strip(strings.Join(final[turn.FlushedRows:], "\n"))
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected a flush")
	}

	turn.sealProse()
	turn.State = TurnComplete
	blk := m.sealedBlock(0)
	sealed := ansi.Strip(blk.Rendered)

	for _, w := range []string{"ALPHALINE", "BETALINE"} {
		if strings.Contains(sealed, w) {
			t.Errorf("sealed tail re-commits already-flushed token %q:\n%s", w, sealed)
		}
	}
	combined := flushedChunk + "\n" + sealed
	if strings.Contains(combined, "▌") {
		t.Errorf("a caret leaked into committed scrollback:\n%s", combined)
	}
	for _, w := range []string{"QUESTIONX", "ALPHALINE", "BETALINE", "GAMMALIVE"} {
		if n := strings.Count(combined, w); n != 1 {
			t.Errorf("token %q appears %d times across flush+seal, want exactly 1", w, n)
		}
	}
}

// dropTrailingStatusRows trims the tail-only live region (the blank separator + "⠋ Writing"
// status) off a row slice, leaving just the rendered prose/tool rows. Those status rows are
// never committed (renderLiveStatus returns "" once a turn seals), so they must be excluded
// when comparing what the footer shows live against what later commits.
func dropTrailingStatusRows(rows []string) []string {
	end := len(rows)
	for end > 0 {
		s := strings.TrimSpace(ansi.Strip(rows[end-1]))
		if s == "" || strings.Contains(s, "Writing") {
			end--
			continue
		}
		break
	}
	return rows[:end]
}

// TestFlush_LiveFooterMatchesFlushOnParagraphSeal locks the core smoothness guarantee: the rows
// shown LIVE in the footer for a growing paragraph are byte-identical to the rows that COMMIT
// when that paragraph later seals. If they differ, the paragraph would visibly jump or re-render
// the instant it settles into scrollback — the exact glitch the paragraph-by-paragraph commit
// boundary exists to avoid.
func TestFlush_LiveFooterMatchesFlushOnParagraphSeal(t *testing.T) {
	// One completed paragraph (flushes immediately) + a growing second paragraph (footer-only).
	m, turn := streamingProse("ALPHADONE first completed paragraph.\n\nBETALIVE second paragraph still typing")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected the completed first paragraph to flush")
	}
	oldFlushed := turn.FlushedRows

	// Capture what the footer shows live for the growing BETALIVE paragraph (the un-flushed tail,
	// minus the tail-only live status).
	rows := m.activeTurnRows(turn)
	if oldFlushed >= len(rows) {
		t.Fatalf("nothing live to capture: FlushedRows=%d, rows=%d", oldFlushed, len(rows))
	}
	liveBeta := dropTrailingStatusRows(rows[oldFlushed:])
	if !strings.Contains(ansi.Strip(strings.Join(liveBeta, "\n")), "BETALIVE") {
		t.Fatalf("test setup: the live tail did not contain the growing paragraph:\n%q", liveBeta)
	}

	// Seal BETALIVE by starting a third paragraph — BETALIVE is now a COMPLETED paragraph, so it
	// becomes part of the flushable prefix. Its text is unchanged (we only appended after it).
	last := len(turn.Steps) - 1
	turn.Steps[last].Text += "\n\nGAMMANEXT third paragraph forming"

	finalRows := m.activeTurnFinalRows(turn)
	if oldFlushed > len(finalRows) {
		t.Fatalf("flush prefix shrank: FlushedRows=%d, finalRows=%d", oldFlushed, len(finalRows))
	}
	flushBeta := finalRows[oldFlushed:]
	if strings.Join(flushBeta, "\n") != strings.Join(liveBeta, "\n") {
		t.Errorf("footer rows shown live differ from the rows committed on seal (a jump/dup):\nlive:\n%q\nflush:\n%q", liveBeta, flushBeta)
	}
}

// TestFlush_RedrawResetsActiveFlushState locks the resize-redraw bug where host
// scrollback was wiped but the active turn still believed its prefix was present
// there. That made the response disappear from both scrollback and the footer,
// leaving only the freshly re-committed masthead visible.
func TestFlush_RedrawResetsActiveFlushState(t *testing.T) {
	m, turn := streamingProse("ALPHALINE first paragraph.\n\nBETALINE second paragraph.\n\nGAMMALIVE final paragraph still going")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected a flush before redraw")
	}
	if turn.FlushedRows == 0 || turn.flushedRowsText == "" {
		t.Fatal("test setup failed: turn did not record flushed rows")
	}
	if strings.Contains(ansi.Strip(m.liveCellsView(m.contentW())), "ALPHALINE") {
		t.Fatal("test setup failed: flushed prefix should have left the live footer")
	}

	m.resizePending = 1
	next, _ := m.onRedraw(RedrawMsg{Nonce: 1})
	m = next.(Model)

	if turn.FlushedRows != 0 || turn.flushedRowsText != "" {
		t.Fatalf("redraw must reset active flushed rows, got rows=%d text=%q", turn.FlushedRows, turn.flushedRowsText)
	}
	live := ansi.Strip(m.liveCellsView(m.contentW()))
	if !strings.Contains(live, "ALPHALINE") {
		t.Fatalf("previously flushed active content must be renderable again after host wipe:\n%s", live)
	}
}

// TestFlush_RedrawResetsSealedFlushState covers the same host-wipe problem after a
// turn has sealed: replaying the transcript at a new size must re-commit the whole
// sealed turn, not just the tail that was not incrementally flushed in the old host
// scrollback.
func TestFlush_RedrawResetsSealedFlushState(t *testing.T) {
	m, turn := streamingProse("ALPHALINE first paragraph.\n\nBETALINE second paragraph.\n\nGAMMALIVE final paragraph still going")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected a flush before seal")
	}
	turn.sealProse()
	turn.State = TurnComplete
	m.activeTurn = ""
	m.inFlight = false

	if strings.Contains(ansi.Strip(m.sealedBlock(0).Rendered), "ALPHALINE") {
		t.Fatal("test setup failed: sealed tail should skip the already-flushed prefix")
	}

	m.resizePending = 1
	next, _ := m.onRedraw(RedrawMsg{Nonce: 1})
	m = next.(Model)

	if turn.FlushedRows != 0 || turn.flushedRowsText != "" {
		t.Fatalf("redraw must reset sealed flushed rows, got rows=%d text=%q", turn.FlushedRows, turn.flushedRowsText)
	}
	sealed := ansi.Strip(m.sealedBlock(0).Rendered)
	for _, word := range []string{"ALPHALINE", "BETALINE", "GAMMALIVE"} {
		if !strings.Contains(sealed, word) {
			t.Fatalf("redraw replay must include %q after host wipe:\n%s", word, sealed)
		}
	}
}

// TestFlush_PlainParagraphStreamsLineByLine proves a lone in-progress PLAIN paragraph settles
// into scrollback LINE BY LINE: every wrapped row except the still-mutable last one flushes as it
// forms (greedy word-wrap can no longer change a closed row), so prose flows token by token into
// native scrollback and the footer holds only the partial last line + the "⠋ Writing" status. The
// committed head must never re-render in the footer, and the paragraph must appear exactly once
// across the flushed prefix + the seal tail (no dup, no loss).
func TestFlush_PlainParagraphStreamsLineByLine(t *testing.T) {
	// A single plain paragraph (no "\n", no markdown) long enough to wrap to >=2 rows at width 80,
	// so the head row settles while the GIRAFFEONE tail keeps growing.
	m, turn := streamingProse("ZEBRAONE a single growing paragraph with no newline yet so it is all in progress GIRAFFEONE")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected the settled head line of the plain paragraph to flush")
	}
	// The settled head row commits; the still-mutable last row does not.
	final := ansi.Strip(strings.Join(m.activeTurnFinalRows(turn), "\n"))
	if !strings.Contains(final, "ZEBRAONE") {
		t.Errorf("the settled head line must be in the flushable prefix:\n%s", final)
	}
	if strings.Contains(final, "GIRAFFEONE") {
		t.Errorf("the still-growing last line must be withheld from the flushable prefix:\n%s", final)
	}
	// The footer shows the growing tail + status, and must NOT re-show the already-committed head.
	foot := ansi.Strip(m.footer())
	if !strings.Contains(foot, "GIRAFFEONE") {
		t.Errorf("the still-growing last line must stream live in the footer:\n%s", foot)
	}
	if strings.Contains(foot, "ZEBRAONE") {
		t.Errorf("the committed head line must not re-appear in the footer (it lives in scrollback now):\n%s", foot)
	}
	if !strings.Contains(foot, "Writing") {
		t.Errorf("the footer must show the live ⠋ Writing status while the paragraph forms:\n%s", foot)
	}
	// Seal: the paragraph appears exactly once across the flushed prefix + the seal tail.
	committed := ansi.Strip(turn.flushedRowsText)
	turn.sealProse()
	turn.State = TurnComplete
	committed += "\n" + ansi.Strip(m.sealedBlock(0).Rendered)
	if n := strings.Count(committed, "ZEBRAONE"); n != 1 {
		t.Errorf("head line appears %d times across flush+seal, want 1 (no dup/loss):\n%s", n, committed)
	}
	if n := strings.Count(committed, "GIRAFFEONE"); n != 1 {
		t.Errorf("tail line appears %d times across flush+seal, want 1 (no dup/loss):\n%s", n, committed)
	}
}

// TestFlush_MarkdownTailFallsBackToParagraph proves a growing paragraph whose tail holds an OPEN
// markdown construct (here an unclosed **bold** span) does NOT settle line by line — committing a
// row the closing delimiter would later restyle would freeze stale bytes in scrollback. So the flush
// falls back to paragraph-level: the completed paragraph still flushes, but the whole growing
// markdown paragraph stays live in the footer until a "\n\n" seals it.
func TestFlush_MarkdownTailFallsBackToParagraph(t *testing.T) {
	// One completed paragraph + a short growing paragraph carrying an unclosed bold span.
	m, turn := streamingProse("DONELINE a settled first paragraph here.\n\nLIVEMD a short **bold tail")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the completed first paragraph must flush")
	}
	final := ansi.Strip(strings.Join(m.activeTurnFinalRows(turn), "\n"))
	if !strings.Contains(final, "DONELINE") {
		t.Errorf("the completed paragraph must still flush:\n%s", final)
	}
	if strings.Contains(final, "LIVEMD") {
		t.Errorf("a markdown-risky growing paragraph must be withheld from the flushable prefix:\n%s", final)
	}
	// It still renders live in the footer so the user sees it form (just not committed yet).
	foot := ansi.Strip(m.footer())
	if !strings.Contains(foot, "LIVEMD") {
		t.Errorf("the growing markdown paragraph must still render live in the footer:\n%s", foot)
	}
}

// TestFlush_PlainParagraphAfterCompletedFlushesLineByLine proves a PLAIN growing paragraph that
// FOLLOWS an already-completed paragraph still settles line by line: the completed paragraph plus
// the blank separator plus the settled head of the second paragraph all commit, the still-mutable
// last line does not, the committed prefix is a byte-exact ROW PREFIX of the live footer render, and
// it never ends in the blank separator row (which would re-commit on seal).
func TestFlush_PlainParagraphAfterCompletedFlushesLineByLine(t *testing.T) {
	m, turn := streamingProse("DONEPARA a completed first paragraph here.\n\nTAILPARA a second paragraph that streams on long enough to wrap past a single visual row so its head settles while LASTWORD keeps forming")
	rows := m.activeTurnRows(turn) // the live footer render this frame (captured before the flush)
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("expected the completed paragraph and the settled head of the second to flush")
	}
	committed := ansi.Strip(turn.flushedRowsText)
	if !strings.Contains(committed, "DONEPARA") {
		t.Errorf("the completed first paragraph must be in the committed prefix:\n%s", committed)
	}
	if !strings.Contains(committed, "TAILPARA") {
		t.Errorf("the settled head of the second paragraph must be in the committed prefix:\n%s", committed)
	}
	if strings.Contains(committed, "LASTWORD") {
		t.Errorf("the still-mutable last line must NOT be committed:\n%s", committed)
	}
	// Byte-exact ROW PREFIX of the live footer render (no jump on seal).
	if got := strings.Join(rows[:turn.FlushedRows], "\n"); got != turn.flushedRowsText {
		t.Errorf("committed prefix diverged from the footer render:\ncommitted:\n%q\nfooter:\n%q", turn.flushedRowsText, got)
	}
	// Must not end in a blank separator row.
	cr := strings.Split(turn.flushedRowsText, "\n")
	if n := len(cr); n > 0 && strings.TrimSpace(ansi.Strip(cr[n-1])) == "" {
		t.Errorf("the committed prefix must not end in a blank row:\n%q", turn.flushedRowsText)
	}
	// Seal reconciles EXACTLY at the byte level: flushed prefix + seal tail reconstructs the whole
	// sealed turn, and the committed block is exactly the indented tail.
	turn.sealProse()
	turn.State = TurnComplete
	sealedRows := m.activeTurnRows(turn)
	tail := sealTail(sealedRows, turn.flushedRowsText)
	if turn.flushedRowsText+"\n"+tail != strings.Join(sealedRows, "\n") {
		t.Errorf("flushed prefix + seal tail does not reconstruct the sealed turn (dup/loss):\nprefix:\n%q\ntail:\n%q", turn.flushedRowsText, tail)
	}
	if blk := m.sealedBlock(0); blk.Rendered != indentLines(tail, LeftPad) {
		t.Errorf("sealed block is not the indented seal tail:\ngot:\n%q\nwant:\n%q", blk.Rendered, indentLines(tail, LeftPad))
	}
}

// TestFlush_PlainTailBecomesMarkdownRiskyAfterLineFlush proves the safe TRANSITION when a plain
// paragraph that has already line-flushed rows then gains a markdown trigger: the flush frontier
// must HOLD (never move backwards, never re-commit the already-flushed rows), the un-flushed tail
// stays live, and seal still reconciles with no dup/loss. This is the prefix-shrink case the reflow
// guard in flushActiveTurn (flush.go) protects.
func TestFlush_PlainTailBecomesMarkdownRiskyAfterLineFlush(t *testing.T) {
	m, turn := streamingProse("KEEPONE plain prose that runs on long enough to wrap past a single visual row so the head settles into scrollback while the tail TAILONE keeps forming")
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the long plain paragraph must line-flush its settled head")
	}
	flushedBefore := turn.FlushedRows
	committedBefore := turn.flushedRowsText
	if flushedBefore == 0 || !strings.Contains(ansi.Strip(committedBefore), "KEEPONE") {
		t.Fatalf("test setup: expected a settled head line committed, FlushedRows=%d:\n%s", flushedBefore, ansi.Strip(committedBefore))
	}
	// The tail gains a markdown trigger (an opening code span) — proseTailIsPlain flips false, so
	// activeTurnFinalRows shrinks below FlushedRows.
	turn.Steps[0].Text += " `code"
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Errorf("flush must HOLD once the prefix shrinks below FlushedRows, not emit a command")
	}
	if turn.FlushedRows != flushedBefore {
		t.Errorf("flush frontier moved after the tail turned risky: %d -> %d", flushedBefore, turn.FlushedRows)
	}
	if turn.flushedRowsText != committedBefore {
		t.Errorf("the committed prefix text changed after the tail turned risky (would re-commit)")
	}
	if foot := ansi.Strip(m.footer()); !strings.Contains(foot, "TAILONE") {
		t.Errorf("the un-flushed tail must stay live in the footer:\n%s", foot)
	}
	// Seal: the head appears exactly once across the flushed prefix + the seal tail (no dup/loss).
	turn.sealProse()
	turn.State = TurnComplete
	combined := ansi.Strip(committedBefore) + "\n" + ansi.Strip(m.sealedBlock(0).Rendered)
	if n := strings.Count(combined, "KEEPONE"); n != 1 {
		t.Errorf("head line appears %d times across flush+seal, want 1 (no dup/loss):\n%s", n, combined)
	}
}

// TestFlush_LineFrontierStaysPrefix proves the line-level flush frontier is monotonic and always a
// byte-exact ROW PREFIX of the live footer render as a plain paragraph grows token by token — so a
// row shown in the footer is identical to the row later committed (no jump, no reflow) the instant
// it settles. This is the core smoothness guarantee for streamed prose.
func TestFlush_LineFrontierStaysPrefix(t *testing.T) {
	m, turn := streamingProse("WORDONE")
	// Each frame appends more of one plain paragraph, growing it from one row to several.
	grow := []string{
		"WORDONE filling out",
		"WORDONE filling out the very first wrapped line of prose until it overflows the width",
		"WORDONE filling out the very first wrapped line of prose until it overflows the width and then keeps going to a second wrapped line that also runs long",
		"WORDONE filling out the very first wrapped line of prose until it overflows the width and then keeps going to a second wrapped line that also runs long before a third wrapped line begins here too",
	}
	prevFlushed := 0
	for _, text := range grow {
		turn.Steps[0].Text = text
		rows := m.activeTurnRows(turn) // exactly what the footer renders this frame
		_ = m.flushActiveTurn()
		if turn.FlushedRows < prevFlushed {
			t.Fatalf("flush frontier moved backwards: %d -> %d", prevFlushed, turn.FlushedRows)
		}
		prevFlushed = turn.FlushedRows
		if turn.FlushedRows > len(rows) {
			t.Fatalf("flushed %d rows but the footer render has only %d", turn.FlushedRows, len(rows))
		}
		// Every committed row is byte-identical to the same row in the live footer render.
		if got := strings.Join(rows[:turn.FlushedRows], "\n"); got != turn.flushedRowsText {
			t.Errorf("committed prefix diverged from the footer render at len=%d:\ncommitted:\n%q\nfooter:\n%q",
				turn.FlushedRows, turn.flushedRowsText, got)
		}
	}
	// By the last (multi-row) frame at least the first wrapped line must have settled into scrollback.
	if !strings.Contains(ansi.Strip(turn.flushedRowsText), "WORDONE") {
		t.Errorf("expected a settled prose line in the flushed prefix, got:\n%s", ansi.Strip(turn.flushedRowsText))
	}
}

// TestProseTailIsPlain locks the conservative guard that decides whether a growing paragraph may
// settle line by line: only plain single-line prose qualifies; anything that could open an inline
// span, a retroactive block, or a multi-line construct must fall back to paragraph-level commit.
func TestProseTailIsPlain(t *testing.T) {
	plain := []string{
		"just some words", "a sentence, with punctuation! and more", "trailing partial wor",
		"a price of 5 dollars and a ratio 3 to 1", "parens (like this) are fine",
	}
	for _, s := range plain {
		if !proseTailIsPlain(s) {
			t.Errorf("proseTailIsPlain(%q) = false, want true", s)
		}
	}
	risky := []string{
		"", "has **bold", "has `code`", "see [link]", "an <autolink>", "a & entity",
		"an escape \\here", "a | pipe", "strike ~tilde", "a # hash", "a > angle",
		"line one\nline two", "tab\tindented", "\tindented code",
		"- bullet item", "+ plus item", "1. ordered item", "1) ordered paren", "    indented code",
		// GFM linkify triggers — glamour styles these and rewrites earlier rows as the link grows.
		"visit https://example.com/long/path", "go to www.example.com", "ping me at name@host.com",
	}
	for _, s := range risky {
		if proseTailIsPlain(s) {
			t.Errorf("proseTailIsPlain(%q) = true, want false", s)
		}
	}
}

// TestFlush_FooterHeightCapped proves the live footer is never tall even while a huge in-progress
// paragraph streams: the growing paragraph DOES render live in the footer, but lastLines(budget)
// caps it to its last maxLiveRows rows — so a flush/commit tea.Println can't dump a tall footer
// into scrollback (bubbletea#1613).
func TestFlush_FooterHeightCapped(t *testing.T) {
	// HEAD_SENTINEL at the top, TAIL_SENTINEL at the bottom, ~400 words between → wraps to dozens
	// of rows. The cap must show the TAIL and drop the HEAD.
	huge := "HEAD_SENTINEL " + strings.Repeat("word ", 400) + "TAIL_SENTINEL"
	m, _ := streamingProse(huge)
	m.rows = 50 // a tall terminal — lastLines(budget) keeps the footer short regardless
	m.inFlight = true
	foot := ansi.Strip(m.footer())
	n := strings.Count(foot, "\n") + 1
	if n > maxLiveRows+10 {
		t.Errorf("footer is %d rows; want <= %d (live cap %d + bottom band)", n, maxLiveRows+10, maxLiveRows)
	}
	// The growing paragraph streams live, but lastLines caps it to the TAIL: the end shows, the
	// head is dropped. (Remove the cap and this would show HEAD_SENTINEL and overflow the footer.)
	if !strings.Contains(foot, "TAIL_SENTINEL") {
		t.Errorf("the growing paragraph's tail must stream live in the footer:\n%s", foot)
	}
	if strings.Contains(foot, "HEAD_SENTINEL") {
		t.Errorf("the growing paragraph's head must be dropped by the height cap, not shown:\n%s", foot)
	}
}

// TestLiveStatus_BlankLineAboveIt proves the live "⠋ Writing" status renders with a blank
// line above it so the thinking cue reads as a distinct status indicator instead of glued to
// the last line of the response. The blank lives ONLY in the live tail — it must never reach
// the flushed prefix (renderLiveStatus returns "" once the turn seals), so a flushed row is
// never re-committed.
func TestLiveStatus_BlankLineAboveIt(t *testing.T) {
	// A completed paragraph ("HELLOLINE") and the still-growing final paragraph ("GROWING") both
	// render live; the live status sits a blank line below the in-flight prose.
	turn := &TurnCell{ID: "turn_x", State: TurnActive, Phase: domain.PhaseGenerating, StartedAt: 1, LastActivityAt: domain.NowMS(),
		Steps: []TurnStep{proseStep("HELLOLINE completed paragraph.\n\nGROWING still typing", true)}}
	m := armedModel(turn)

	out := ansi.Strip(renderTurn(m.theme, m.md, turn, m.chromeW(), m.contentW(), m.expanded, 0, domain.NowMS()))
	lines := strings.Split(out, "\n")
	wi := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Writing") {
			wi = i
			break
		}
	}
	if wi < 0 {
		t.Fatalf("live Writing status not rendered:\n%s", out)
	}
	if wi == 0 || strings.TrimSpace(lines[wi-1]) != "" {
		t.Fatalf("the live status needs a blank line above it; prev line = %q:\n%s", lines[wi-1], out)
	}
	if !strings.Contains(out, "HELLOLINE") {
		t.Fatalf("the completed paragraph must stay above the live status:\n%s", out)
	}
	// The still-growing paragraph must render LIVE in the footer (the whole point of the change),
	// sitting between the completed paragraph and the live status.
	if !strings.Contains(out, "GROWING") {
		t.Fatalf("the still-growing paragraph must render live above the status:\n%s", out)
	}
	// ...yet it must NOT enter the flushable prefix (commit-bound, paragraph-by-paragraph), and
	// the blank+status are tail-only: the flushed prefix must not end in a blank row, or the
	// flush↔seal reconciliation would re-commit it.
	final := m.activeTurnFinalRows(turn)
	if strings.Contains(ansi.Strip(strings.Join(final, "\n")), "GROWING") {
		t.Fatalf("the still-growing paragraph must be withheld from the flushable prefix:\n%q", final)
	}
	if n := len(final); n > 0 && strings.TrimSpace(ansi.Strip(final[n-1])) == "" {
		t.Fatalf("the flushed prefix must not gain a trailing blank from the live-status separator:\n%q", final)
	}
}

// TestLiveStatus_GluedToMarkerWhenNoBody proves the blank separator is gated on a non-empty
// body: during the silent "⠋ Analyzing request" gap (no rendered step yet), the status glues
// to the "◆ DAINTREE" marker rather than leaving an empty hole between them.
func TestLiveStatus_GluedToMarkerWhenNoBody(t *testing.T) {
	turn := &TurnCell{ID: "turn_y", State: TurnActive, Phase: domain.PhaseAnalyzing, StartedAt: 1, LastActivityAt: domain.NowMS()}
	m := armedModel(turn)

	out := ansi.Strip(renderTurn(m.theme, m.md, turn, m.chromeW(), m.contentW(), m.expanded, 0, domain.NowMS()))
	lines := strings.Split(out, "\n")
	wi := -1
	for i, ln := range lines {
		if strings.Contains(ln, "Analyzing request") {
			wi = i
			break
		}
	}
	if wi <= 0 {
		t.Fatalf("the Analyzing status must render below the marker:\n%s", out)
	}
	if strings.TrimSpace(lines[wi-1]) == "" {
		t.Fatalf("with no body the status must glue to the marker (no blank hole above):\n%s", out)
	}
	if !strings.Contains(lines[wi-1], "DAINTREE") {
		t.Fatalf("the line above the bodyless status should be the ◆ DAINTREE marker:\n%s", out)
	}
}

// TestFlush_ActiveToolNotFlushed proves an ACTIVE (mutating) tool row is never frozen: the
// completed content before it flushes incrementally (so the footer doesn't accumulate), but
// the open tool group stays live in the footer until it settles + closes.
func TestFlush_ActiveToolNotFlushed(t *testing.T) {
	turn := &TurnCell{ID: "turn_1", State: TurnActive, Steps: []TurnStep{
		proseStep("WORKINGPROSE on it.", false), // completed (a tool follows) → flushes
		toolStep("a1", "memory.list", "TOOLDETAILX", ActActive),
	}}
	m := armedModel(turn)

	final := m.activeTurnFinalRows(turn)
	flushedChunk := ansi.Strip(strings.Join(final[turn.FlushedRows:], "\n"))
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("the completed prose before an active tool must still flush")
	}
	if !strings.Contains(flushedChunk, "WORKINGPROSE") {
		t.Errorf("completed prose should flush:\n%s", flushedChunk)
	}
	if strings.Contains(flushedChunk, "TOOLDETAILX") {
		t.Errorf("an ACTIVE tool row must NOT be flushed (it still mutates):\n%s", flushedChunk)
	}
	// The active tool stays in the live footer.
	if !strings.Contains(ansi.Strip(m.liveCellsView(m.contentW())), "TOOLDETAILX") {
		t.Error("the active tool must remain in the live footer")
	}
}

// TestFlush_ToolOnlyTurnKeepsMarkerAndRows is the regression for the seal marker mismatch:
// a turn that produces ONLY tool steps (no prose) flushes its preamble WITH the "◆ DAINTREE"
// marker, so the SEALED render must also carry the marker (hasResponded, not hasProse) — else
// sealTail's row-count fallback strips one row too many and drops the first tool row.
func TestFlush_ToolOnlyTurnKeepsMarkerAndRows(t *testing.T) {
	turn := &TurnCell{ID: "turn_1", UserText: "QUESTIONX", State: TurnActive, Steps: []TurnStep{
		toolStep("a1", "memory.list", "TOOLDETAILX", ActDone),
	}}
	m := armedModel(turn)

	final := m.activeTurnFinalRows(turn)
	flushedChunk := ansi.Strip(strings.Join(final[turn.FlushedRows:], "\n"))
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("a tool-only turn must at least flush its preamble")
	}
	turn.State = TurnComplete
	blk := m.sealedBlock(0)
	combined := flushedChunk + "\n" + ansi.Strip(blk.Rendered)

	if !strings.Contains(combined, "DAINTREE") {
		t.Errorf("tool-only turn lost the marker (tool tree orphaned in scrollback):\n%s", combined)
	}
	if n := strings.Count(combined, "TOOLDETAILX"); n != 1 {
		t.Errorf("tool detail appears %d times across flush+seal, want exactly 1 (row-count fallback dropped/duped it):\n%s", n, combined)
	}
	if n := strings.Count(combined, "QUESTIONX"); n != 1 {
		t.Errorf("user text appears %d times, want 1:\n%s", n, combined)
	}
}

// TestFlush_GatedOnMastheadCommitted proves no row flushes ABOVE the masthead.
func TestFlush_GatedOnMastheadCommitted(t *testing.T) {
	m, _ := streamingProse("ALPHALINE done.\nBETALINE live")
	m.queue.headerDone = false
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Error("flushActiveTurn must return nil before the masthead is committed")
	}
	m.queue.headerDone = true
	m.commitArmed = false
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Error("flushActiveTurn must return nil before commits are armed")
	}
}

// TestFlush_HoldsForUncommittedEarlierCell proves the active turn's preamble never
// LEAPFROGS an earlier transcript cell still waiting in the commit queue. This is the
// boot-banner ordering bug: the one-time "Connected to Daintree MCP" note is appended
// under the still-empty transcript, the user's first turn is appended immediately after,
// and the flush — which fires on EVERY afterStateChange while the queue commits sealed
// cells one-per-ack — must NOT print the turn's "◆ DAINTREE" marker before the queue has
// committed the note. Otherwise the note commits BELOW the marker, mid-turn, instead of
// directly under the masthead where it belongs.
func TestFlush_HoldsForUncommittedEarlierCell(t *testing.T) {
	m := testModel(80)
	m.commitArmed = true
	m.queue.headerDone = true
	m.queue.committed = 0 // the leading "Connected" note has NOT committed yet
	turn := &TurnCell{ID: "turn_1", UserText: "QUESTIONX", State: TurnActive, Phase: domain.PhaseReceived}
	m.transcript = []TranscriptCell{
		{Note: &NoteCell{ID: "note_1", Level: NoteSuccess, Text: "Connected to Daintree MCP"}},
		{Turn: turn},
	}
	m.activeTurn = turn.ID

	// committed (0) < activeTurnIndex (1): the flush must HOLD, leaving the note ahead of the
	// turn so the queue commits it first (under the masthead).
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Fatal("flush must hold while an earlier cell is still uncommitted (the note would be leapfrogged below the marker)")
	}
	if turn.FlushedRows != 0 {
		t.Errorf("no rows may flush while holding, got FlushedRows=%d", turn.FlushedRows)
	}

	// Once the queue commits the note (committed advances to the turn's index), the flush proceeds.
	m.queue.committed = 1
	if cmd := m.flushActiveTurn(); cmd == nil {
		t.Fatal("flush must proceed once the queue frontier reaches the active turn")
	}
	if turn.FlushedRows == 0 {
		t.Error("the turn's immutable preamble should have flushed once the note committed")
	}
}

// TestFlush_LeftPadConsistent proves prose shown in the footer (before it flushes) sits at the
// same LeftPad inset as the committed scrollback — so it never jumps a column on seal. Both the
// completed paragraph (ALPHAWORD) and the still-growing one (BETAWORD) render live in the footer.
func TestFlush_LeftPadConsistent(t *testing.T) {
	m, turn := streamingProse("ALPHAWORD here is a completed answer line.\n\nBETAWORD still going")
	m.rows = 30
	m.inFlight = true
	foot := ansi.Strip(m.footer())
	var liveLine string
	for _, l := range strings.Split(foot, "\n") {
		if strings.Contains(l, "ALPHAWORD") {
			liveLine = l
			break
		}
	}
	if liveLine == "" {
		t.Fatalf("completed paragraph not found in footer:\n%s", foot)
	}
	var betaLine string
	for _, l := range strings.Split(foot, "\n") {
		if strings.Contains(l, "BETAWORD") {
			betaLine = l
			break
		}
	}
	if betaLine == "" {
		t.Errorf("the in-progress paragraph must stream live in the footer:\n%s", foot)
	} else if !strings.HasPrefix(betaLine, strings.Repeat(" ", LeftPad)) {
		t.Errorf("the streaming in-progress paragraph is not LeftPad-inset (%d spaces): %q", LeftPad, betaLine)
	}
	if !strings.HasPrefix(liveLine, strings.Repeat(" ", LeftPad)) {
		t.Errorf("streaming prose is not LeftPad-inset (%d spaces): %q", LeftPad, liveLine)
	}
	turn.sealProse()
	turn.State = TurnComplete
	blk := m.sealedBlock(0)
	for _, l := range strings.Split(ansi.Strip(blk.Rendered), "\n") {
		if strings.Contains(l, "ALPHAWORD") && !strings.HasPrefix(l, strings.Repeat(" ", LeftPad)) {
			t.Errorf("committed prose is not LeftPad-inset: %q", l)
		}
	}
}
