package ui

import (
	"strings"
	"testing"
)

// fakeBlock builds a sealed/header block factory pair that records commit order and
// returns trivial blocks. The queue's nextCommit/ack are driven directly (no
// tea.Program needed) to prove the commit-queue protocol.

func TestScrollback_MastheadFirstThenOrdered(t *testing.T) {
	var q scrollbackQueue
	cells := []TranscriptCell{
		{Turn: &TurnCell{ID: "turn_1", State: TurnComplete}},
		{Turn: &TurnCell{ID: "turn_2", State: TurnComplete}},
		{Turn: &TurnCell{ID: "turn_3", State: TurnActive}}, // NOT sealed → blocks frontier
	}

	header := func() ScrollbackBlock { return ScrollbackBlock{ID: headerID, Kind: BlockMasthead, Rendered: "MAST"} }
	sealed := func(i int) ScrollbackBlock {
		return ScrollbackBlock{ID: cells[i].ID(), Kind: BlockTurn, Rendered: cells[i].ID()}
	}

	// Simulate the program loop: nextCommit returns a cmd (we don't run it; we ack
	// the in-flight head by id) until nothing is eligible.
	var order []string
	step := func() bool {
		cmd := q.nextCommit(cells, sealed, header)
		if cmd == nil {
			return false
		}
		// Determine which block is in flight by re-deriving the head deterministically:
		// it is the header until headerDone, else the cell at the committed cursor.
		var id string
		if !q.headerDone {
			id = headerID
		} else {
			id = cells[q.liveStart(len(cells))].ID()
		}
		order = append(order, id)
		q.ack(id, q.gen, len(cells))
		return true
	}

	for i := 0; i < 10 && step(); i++ {
	}

	want := []string{headerID, "turn_1", "turn_2"}
	if len(order) != len(want) {
		t.Fatalf("commit order length = %d (%v), want %d (%v)", len(order), order, len(want), want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("commit order[%d] = %q, want %q (full: %v)", i, order[i], want[i], order)
		}
	}

	// The active (unsealed) turn must remain LIVE — never committed past it.
	if q.committed != 2 {
		t.Errorf("committed cursor = %d, want 2 (the active turn stays in the footer)", q.committed)
	}
}

func TestScrollback_OneInFlight(t *testing.T) {
	var q scrollbackQueue
	cells := []TranscriptCell{{Turn: &TurnCell{ID: "turn_1", State: TurnComplete}}}
	header := func() ScrollbackBlock { return ScrollbackBlock{ID: headerID} }
	sealed := func(i int) ScrollbackBlock { return ScrollbackBlock{ID: cells[i].ID()} }

	// First commit starts (masthead) and marks inFlight.
	if cmd := q.nextCommit(cells, sealed, header); cmd == nil {
		t.Fatal("expected a masthead commit cmd")
	}
	if !q.inFlight {
		t.Fatal("queue should be inFlight after a commit starts")
	}
	// A SECOND nextCommit before the ack must return nil (one in flight only).
	if cmd := q.nextCommit(cells, sealed, header); cmd != nil {
		t.Error("nextCommit must return nil while a commit is in flight")
	}
	// Ack the header → frontier advances; the next commit (turn_1) is now eligible.
	q.ack(headerID, q.gen, len(cells))
	if cmd := q.nextCommit(cells, sealed, header); cmd == nil {
		t.Error("expected turn_1 commit after the masthead ack")
	}
}

func TestScrollback_ResetKeyReArms(t *testing.T) {
	var q scrollbackQueue
	q.headerDone = true
	q.committed = 3
	// A changed reset key (a /clear or resize redraw) re-arms the cursor + header.
	q.applyResetKey(5)
	if q.headerDone {
		t.Error("applyResetKey should re-arm headerDone to false")
	}
	if q.committed != 0 {
		t.Errorf("applyResetKey should reset committed to 0, got %d", q.committed)
	}
	// The SAME key is idempotent.
	q.headerDone = true
	q.committed = 2
	q.applyResetKey(5)
	if !q.headerDone || q.committed != 2 {
		t.Error("applyResetKey with an unchanged key must be a no-op")
	}
}

// TestSplitRowChunks locks the viewport-chunking math: every chunk is at most maxRows
// "\n"-lines, order is preserved, and rejoining reproduces the input exactly — so a tall
// committed block (a large-paste YOU card) is split into pieces no single insertAbove can
// overflow, with nothing dropped or reordered (#1613).
func TestSplitRowChunks(t *testing.T) {
	build := func(n int) string {
		rows := make([]string, n)
		for i := range rows {
			rows[i] = "row" + itoa(i)
		}
		return strings.Join(rows, "\n")
	}
	cases := []struct {
		name    string
		text    string
		maxRows int
		want    int // expected chunk count
	}{
		{"fits exactly", build(8), 8, 1},
		{"fits under", build(5), 8, 1},
		{"splits evenly", build(20), 10, 2},
		{"splits with remainder", build(25), 10, 3},
		{"one row per chunk", build(4), 1, 4},
		{"maxRows below one floors to one", build(3), 0, 3},
		{"single line", "only", 5, 1},
		{"empty string is one chunk", "", 5, 1},
		{"trailing newline preserved", "a\nb\n", 1, 3},          // "a","b","" — blank trailing row kept
		{"interior blank lines preserved", "a\n\nb\n\nc", 2, 3}, // blank rows count toward the budget
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			maxRows := c.maxRows
			if maxRows < 1 {
				maxRows = 1
			}
			chunks := splitRowChunks(c.text, c.maxRows)
			if len(chunks) != c.want {
				t.Fatalf("chunk count = %d, want %d", len(chunks), c.want)
			}
			for i, ch := range chunks {
				if n := strings.Count(ch, "\n") + 1; n > maxRows {
					t.Errorf("chunk %d has %d lines, exceeds maxRows %d", i, n, maxRows)
				}
			}
			if got := strings.Join(chunks, "\n"); got != c.text {
				t.Errorf("rejoined chunks = %q, want %q (content/order not preserved)", got, c.text)
			}
		})
	}
}

// TestScrollbackChunkRows proves the per-Println row budget reserves the live-footer height
// (so insertAbove's CursorUp can't clamp past the top of the viewport) and floors at 1 on a
// terminal too short to spare any rows.
func TestScrollbackChunkRows(t *testing.T) {
	m := testModel(80) // rows = 40, view = home
	// Home: reserve the bounded live tail (maxLiveRows) + the blank separator + the bottom
	// band, so a max-sized chunk + the footer can never exceed the viewport (insertAbove's
	// CursorUp would otherwise clamp at the top).
	reserve := maxLiveRows + 1 + lineCount(m.bottomBand(m.contentW()))
	if got, want := m.scrollbackChunkRows(), m.rows-reserve; got != want {
		t.Fatalf("home scrollbackChunkRows() = %d, want rows(%d) - reserve(%d) = %d", got, m.rows, reserve, want)
	}
	if m.scrollbackChunkRows() <= 0 {
		t.Fatal("a normal-height terminal must yield a positive row budget")
	}
	// The ops/help decks fill nearly the whole footer, so a commit fired while one is open
	// must print one row at a time.
	for _, v := range []viewMode{viewOperations, viewHelp} {
		mv := m
		mv.view = v
		if got := mv.scrollbackChunkRows(); got != 1 {
			t.Fatalf("scrollbackChunkRows() in view %v = %d, want 1 (deck nearly fills the footer)", v, got)
		}
	}
	// A terminal shorter than the reserved footer can't spare any rows: the budget floors at
	// 1 (one Println per row) rather than going non-positive and producing a no-op slice.
	m.rows = 1
	if got := m.scrollbackChunkRows(); got != 1 {
		t.Fatalf("scrollbackChunkRows() on a tiny terminal = %d, want 1 (floor)", got)
	}
}

func TestScrollback_MastheadOwnsFirstSpacer(t *testing.T) {
	m := testModel(80)
	header := m.headerBlock()
	if !strings.HasSuffix(header.Rendered, "\n") {
		t.Fatalf("masthead block must reserve the first transcript spacer, got %q", header.Rendered)
	}

	m.transcript = []TranscriptCell{
		{Note: &NoteCell{ID: "note_1", Level: NoteSuccess, Text: "Connected to Daintree MCP"}},
		{Note: &NoteCell{ID: "note_2", Level: NoteWarn, Text: "Second note"}},
	}
	first := m.sealedBlock(0)
	if strings.HasPrefix(first.Rendered, "\n") {
		t.Fatalf("first transcript cell must use the masthead spacer, got %q", first.Rendered)
	}
	second := m.sealedBlock(1)
	if !strings.HasPrefix(second.Rendered, "\n") {
		t.Fatalf("later transcript cells must keep their leading spacer, got %q", second.Rendered)
	}
}
