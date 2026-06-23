package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// flush_test.go locks the INCREMENTAL ROW FLUSH (flush.go): the active turn's STABLE
// completed-paragraph rows commit to native scrollback AS THEY STREAM (auto-scroll) while
// the still-growing final paragraph streams as a DIM LIVE PREVIEW in the footer (T6) but is
// never flushed — only completed "\n\n"-terminated paragraphs commit. Flushed rows carry no
// caret and are byte-identical to the seal's render, so nothing is duplicated.

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
	turn := &TurnCell{ID: "turn_1", UserText: "QUESTIONX", State: TurnActive, Steps: []TurnStep{proseStep(text, true)}}
	return armedModel(turn), turn
}

// TestFlush_StableParasFlow proves completed PARAGRAPHS flush to scrollback while streaming
// and the still-growing final paragraph is WITHHELD from the footer (paragraph-by-paragraph).
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
	// The flushed paragraphs leave the footer; the still-growing final paragraph (T6) streams
	// as a live preview in the footer but is never committed to scrollback.
	live := ansi.Strip(m.liveCellsView(m.contentW()))
	if strings.Contains(live, "ALPHALINE") {
		t.Errorf("a flushed paragraph is still in the live footer:\n%s", live)
	}
	if !strings.Contains(live, "GAMMALIVE") {
		t.Errorf("the in-progress paragraph must stream as a live preview in the footer:\n%s", live)
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

// TestFlush_SingleParagraphHeld proves a lone in-progress paragraph (no completed line) is
// NOT flushed — it is held whole in the footer until it seals, then committed once. This is
// the case that must never freeze a raw partial copy in scrollback.
func TestFlush_SingleParagraphHeld(t *testing.T) {
	m, turn := streamingProse("ZEBRAONE a single growing paragraph with no newline yet so it is all in progress GIRAFFEONE")
	_ = m.flushActiveTurn() // flushes only the preamble (YOU + marker), not the prose
	final := ansi.Strip(strings.Join(m.activeTurnFinalRows(turn), "\n"))
	if strings.Contains(final, "ZEBRAONE") {
		t.Errorf("an in-progress single paragraph must not be in the flushable prefix:\n%s", final)
	}
	// But the footer DOES stream it as a live preview (T6): the answer is visible while it
	// is still being written, not hidden behind a lone spinner.
	if !strings.Contains(ansi.Strip(m.footer()), "ZEBRAONE") {
		t.Errorf("a single growing paragraph must stream as a live preview in the footer:\n%s", ansi.Strip(m.footer()))
	}
	// Seal: the paragraph commits exactly once.
	turn.sealProse()
	turn.State = TurnComplete
	blk := m.sealedBlock(0)
	if n := strings.Count(ansi.Strip(blk.Rendered), "ZEBRAONE"); n != 1 {
		t.Errorf("sealed paragraph appears %d times, want 1:\n%s", n, ansi.Strip(blk.Rendered))
	}
}

// TestFlush_FooterHeightCapped proves the live footer is never tall: a huge in-progress
// paragraph is WITHHELD entirely (paragraph-by-paragraph commit), and the maxLiveRows cap
// (view.go) backstops any transient tall content so a flush/commit tea.Println can't dump a
// tall footer into scrollback (bubbletea#1613).
func TestFlush_FooterHeightCapped(t *testing.T) {
	huge := "BIGPARA " + strings.Repeat("word ", 400) // would wrap to dozens of rows if shown
	m, _ := streamingProse(huge)
	m.rows = 50 // a tall terminal — the withheld paragraph + cap keep the footer short anyway
	m.inFlight = true
	foot := m.footer()
	n := strings.Count(foot, "\n") + 1
	if n > maxLiveRows+10 {
		t.Errorf("footer is %d rows; want <= %d (live cap %d + bottom band)", n, maxLiveRows+10, maxLiveRows)
	}
	// The still-growing paragraph streams as a live preview, but the cap keeps it short: only
	// the TAIL rows show, so the height stays bounded (a flush tea.Println can't dump a tall
	// footer — bubbletea#1613).
	if !strings.Contains(ansi.Strip(foot), "word") {
		t.Errorf("the in-progress paragraph should stream a (bounded) live preview:\n%s", ansi.Strip(foot))
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

// TestFlush_LeftPadConsistent proves a completed paragraph shown in the footer (before it
// flushes) sits at the same LeftPad inset as the committed scrollback — so it never jumps a
// column on seal. (The still-growing final paragraph BETAWORD is withheld from the footer.)
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
	if !strings.Contains(foot, "BETAWORD") {
		t.Errorf("the in-progress paragraph must stream as a live preview in the footer:\n%s", foot)
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
