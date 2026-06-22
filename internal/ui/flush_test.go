package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// flush_test.go locks the INCREMENTAL ROW FLUSH (flush.go): the active turn's STABLE
// completed-line rows commit to native scrollback AS THEY STREAM (auto-scroll) while the
// still-reflowing in-progress paragraph stays in the footer; flushed rows carry no caret
// and are byte-identical to the seal's render, so nothing is duplicated.

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
// and the in-progress paragraph stays in the short footer tail (auto-scroll, constant-ht).
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
	// The footer now holds only the in-progress paragraph (the caret tail) — short.
	live := ansi.Strip(m.liveCellsView(m.contentW()))
	if strings.Contains(live, "ALPHALINE") {
		t.Errorf("a flushed paragraph is still in the live footer:\n%s", live)
	}
	if !strings.Contains(live, "GAMMALIVE") {
		t.Errorf("the in-progress paragraph must remain in the footer:\n%s", live)
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
	// Seal: the paragraph commits exactly once.
	turn.sealProse()
	turn.State = TurnComplete
	blk := m.sealedBlock(0)
	if n := strings.Count(ansi.Strip(blk.Rendered), "ZEBRAONE"); n != 1 {
		t.Errorf("sealed paragraph appears %d times, want 1:\n%s", n, ansi.Strip(blk.Rendered))
	}
}

// TestFlush_FooterHeightCapped proves the live footer is never tall: even a huge in-progress
// paragraph (held in the footer, raw) is bounded to maxLiveRows so a flush/commit tea.Println
// can't dump a tall footer into scrollback (bubbletea#1613).
func TestFlush_FooterHeightCapped(t *testing.T) {
	huge := "BIGPARA " + strings.Repeat("word ", 400) // wraps to dozens of rows
	m, _ := streamingProse(huge)
	m.rows = 50 // a tall terminal — without the cap the footer would be dozens of rows
	m.inFlight = true
	foot := m.footer()
	n := strings.Count(foot, "\n") + 1
	if n > maxLiveRows+10 {
		t.Errorf("footer is %d rows; want <= %d (live cap %d + bottom band)", n, maxLiveRows+10, maxLiveRows)
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

// TestFlush_LeftPadConsistent proves the STREAMING footer prose sits at the same LeftPad
// inset as the committed scrollback — so it never jumps a column on seal.
func TestFlush_LeftPadConsistent(t *testing.T) {
	m, turn := streamingProse("ALPHAWORD here is a streaming answer line")
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
		t.Fatalf("streaming prose not found in footer:\n%s", foot)
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
