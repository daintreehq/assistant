package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// flush_test.go locks the incremental scrollback flush (flush.go): the active turn's
// completed rows commit to native scrollback AS THEY STREAM so the live View stays
// ~constant-height, and the seal commits ONLY the un-flushed tail so the flushed
// prefix is never duplicated (the "output stopped then repeated" bug).

// proseModel builds a steady-state-ish model with one active, streaming prose turn of
// the given text. commitArmed + headerDone are set so the flush gate is open.
func proseModel(text string, streaming bool) (Model, *TurnCell) {
	m := testModel(80)
	m.commitArmed = true
	m.queue.headerDone = true
	t := &TurnCell{
		ID:    "turn_1",
		State: TurnActive,
		Steps: []TurnStep{{Kind: StepProse, Text: text, Streaming: streaming}},
	}
	m.transcript = []TranscriptCell{{Turn: t}}
	m.activeTurn = "turn_1"
	return m, t
}

// longProse is a single paragraph that wraps to many rows at width 80 (greedy
// wrapCells — earlier rows stable as more text appends, the case the flush targets).
const longProse = "Alpha bravo charlie delta echo foxtrot golf hotel india juliet kilo lima mike november oscar papa quebec romeo sierra tango uniform victor whiskey xray yankee zulu one two three four five six seven eight nine ten eleven twelve thirteen fourteen fifteen sixteen seventeen eighteen"

// TestFlush_AdvancesAndFooterShrinks proves a streaming prose turn flushes its leading
// rows (FlushedRows advances) and the live footer then renders only the tail — never
// the whole turn — so the View can't grow unbounded.
func TestFlush_AdvancesAndFooterShrinks(t *testing.T) {
	m, turn := proseModel(longProse, true)

	rows := m.activeTurnRows(turn)
	if len(rows) < 3 {
		t.Fatalf("expected the long prose to wrap to several rows, got %d: %q", len(rows), rows)
	}

	cmd := m.flushActiveTurn()
	if cmd == nil {
		t.Fatal("flushActiveTurn returned nil — a streaming multi-row prose turn must flush")
	}
	if turn.FlushedRows == 0 {
		t.Fatal("FlushedRows did not advance after a flush")
	}
	// Conservative rule: all-but-the-last row flushes while streaming.
	if turn.FlushedRows != len(rows)-1 {
		t.Errorf("FlushedRows = %d, want %d (all-but-last while streaming)", turn.FlushedRows, len(rows)-1)
	}

	// The live footer now renders ONLY the tail (rows >= FlushedRows), not the whole turn.
	live := m.liveCellsView(m.contentW())
	liveRows := strings.Count(strings.TrimRight(live, "\n"), "\n") + 1
	if liveRows > 2 {
		t.Errorf("live footer is %d rows after flush; want <= 2 (the live tail only):\n%s", liveRows, ansi.Strip(live))
	}
}

// TestFlush_NoDuplicateOnSeal proves the flushed prefix is committed exactly once: the
// seal commits ONLY rows >= FlushedRows, so concatenating the flushed Println text with
// the sealed block reconstructs the whole turn with NO repeated rows.
func TestFlush_NoDuplicateOnSeal(t *testing.T) {
	m, turn := proseModel(longProse, true)

	// Stream-flush.
	_ = m.flushActiveTurn()
	flushedN := turn.FlushedRows
	if flushedN == 0 {
		t.Fatal("expected a flush to advance FlushedRows")
	}
	flushedText := turn.flushedRowsText

	// Seal the turn (prose finalizes), then build the sealed block.
	turn.sealProse()
	turn.State = TurnComplete
	blk := m.sealedBlock(0)

	// The sealed block must NOT contain the flushed prefix's first line (no double-commit).
	flushedFirst := strings.TrimSpace(ansi.Strip(strings.SplitN(flushedText, "\n", 3)[1])) // row 0 is the blank separator
	sealedPlain := ansi.Strip(blk.Rendered)
	if flushedFirst != "" && strings.Contains(sealedPlain, flushedFirst) {
		t.Errorf("sealed block re-commits an already-flushed line %q — duplication:\n%s", flushedFirst, sealedPlain)
	}

	// Reconstruct: flushed text + sealed tail must cover every wrapped row exactly once.
	combined := ansi.Strip(flushedText) + "\n" + sealedPlain
	for _, word := range []string{"Alpha", "eighteen"} {
		if n := strings.Count(combined, word); n != 1 {
			t.Errorf("word %q appears %d times across flush+seal, want exactly 1", word, n)
		}
	}
}

// TestFlush_HeldWhileToolActive proves the conservative rule: a turn with a not-done
// tool flushes NOTHING (tool rows mutate), so nothing is frozen prematurely.
func TestFlush_HeldWhileToolActive(t *testing.T) {
	m := testModel(80)
	m.commitArmed = true
	m.queue.headerDone = true
	turn := &TurnCell{
		ID:    "turn_1",
		State: TurnActive,
		Steps: []TurnStep{
			{Kind: StepProse, Text: "Working on it.\n", Streaming: false},
			{Kind: StepTool, Activity: &Activity{ID: "a1", Name: "memory.list", State: ActActive}},
		},
	}
	m.transcript = []TranscriptCell{{Turn: turn}}
	m.activeTurn = "turn_1"

	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Error("flushActiveTurn must return nil while a tool is active (tool rows mutate)")
	}
	if turn.FlushedRows != 0 {
		t.Errorf("FlushedRows = %d, want 0 while a tool is active", turn.FlushedRows)
	}
}

// TestFlush_GatedOnMastheadCommitted proves no prose flushes ABOVE the masthead: the
// flush is held until commitArmed && headerDone.
func TestFlush_GatedOnMastheadCommitted(t *testing.T) {
	m, _ := proseModel(longProse, true)
	m.queue.headerDone = false // masthead not yet committed
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Error("flushActiveTurn must return nil before the masthead is committed (no prose above the masthead)")
	}
	m.queue.headerDone = true
	m.commitArmed = false // commits not yet armed
	if cmd := m.flushActiveTurn(); cmd != nil {
		t.Error("flushActiveTurn must return nil before commits are armed")
	}
}

var _ = domain.PhaseReceived
