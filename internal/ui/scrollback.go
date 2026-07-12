package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"
)

// scrollback.go implements the commit-queue protocol. Sealed
// cells + the masthead become immutable ScrollbackBlocks committed to the host's
// native scrollback ONE at a time, via Bubble Tea's print-above-program command
// (tea.Println). Each commit is acked by a ScrollbackCommittedMsg that pops the
// queue head, advances the committed cursor (dropping the sealed cell from the live
// footer), and schedules the next. Masthead is committed first. NO portal/settle/
// replay machinery.

// ScrollbackKind tags an immutable block.
type ScrollbackKind int

const (
	BlockMasthead ScrollbackKind = iota
	BlockTurn
	BlockNote
	BlockCommand
)

// headerID is the stable masthead block id; it must land on top of scrollback.
const headerID = "__header__"

// ScrollbackBlock is one immutable thing committed to scrollback. Width is
// load-bearing: a block is rendered AT a width then frozen; a resize re-renders it
// fresh at the new width (never reflows in place).
type ScrollbackBlock struct {
	ID       string
	Kind     ScrollbackKind
	Rendered string // full-fidelity, styled, width-wrapped
	Plain    string // plain-text fallback (used if Rendered is empty)
	Width    int
	Gen      int // commit generation (#4): the queue's gen at emit time; the ack carries it back
}

// scrollbackQueue owns the commit frontier + the one-in-flight discipline. It is a
// plain struct mutated ONLY inside Update (never off the loop).
type scrollbackQueue struct {
	headerDone bool // masthead committed?
	committed  int  // # of transcript cells (from the front) now in scrollback
	resetKey   int  // clearNonce + redrawNonce; a change re-arms the cursor + header
	inFlight   bool // a commit is awaiting its ack
	// inFlightCell is the transcript index of the cell whose commit is in flight, or -1
	// (none, or the masthead). Turns, commands, and large notes leave the live View at
	// selection time so the footer shrinks BEFORE Println; short notes stay through the
	// barrier so their ack creates an observable post-print repaint (see below).
	inFlightCell int
	// dropInFlightCell selects the geometry-safe early-drop path. Short system notes
	// deliberately remain visible through the barrier: if an idle startup note is
	// removed before it ever renders, the footer View is identical before and after
	// Println, Bubble Tea skips the repaint, and the composer vanishes until a keypress.
	dropInFlightCell bool
	gen              int // commit generation (#4); bumped on every reset, stamped on each block
}

// release clears the in-flight claim (and the live-View cell drop that rides it).
// Every path that abandons or completes a commit must go through this so the two
// fields can never desync.
func (q *scrollbackQueue) release() {
	q.inFlight = false
	q.inFlightCell = -1
	q.dropInFlightCell = false
}

// applyResetKey re-arms the queue when the reset key changes (a /clear or resize
// redraw): committed=0, headerDone=false, and a bumped generation. Length alone
// can't detect a clear (a fresh confirmation card can make the new length equal the
// old committed count), so the monotonic key makes the reset deterministic.
//
// #4 generation safety: a commit emitted under a PRIOR generation may still ack
// AFTER the reset. Without the generation, that stale ack would mark the NEW
// header done or bump `committed` for a fresh transcript. So every reset bumps
// `gen`, every block carries the gen it was emitted under, and the ack ignores any
// block whose gen != current. `inFlight` is also cleared so the post-reset commit
// can re-emit from the top immediately rather than waiting on the doomed ack.
func (q *scrollbackQueue) applyResetKey(key int) {
	if key != q.resetKey {
		q.resetKey = key
		q.committed = 0
		q.headerDone = false
		q.gen++
		q.release() // the prior in-flight ack is now stale (wrong gen) → ignored
	}
}

// liveStart returns the index of the first transcript cell still LIVE in the footer
// (everything before it is in scrollback). Clamped to the transcript length.
func (q *scrollbackQueue) liveStart(n int) int {
	if q.committed > n {
		return n
	}
	if q.committed < 0 {
		return 0
	}
	return q.committed
}

// nextCommit decides the queue head and returns the tea.Cmd that commits it (a
// tea.Println of Rendered, falling back to Plain), or nil when nothing is eligible
// or a commit is already in flight. The block factory renders the sealed cell at
// index i at the current width. headerBlock renders the masthead.
//
// Commit order: masthead first, then sealed transcript cells in index order
// from the committed cursor forward. A cell is eligible only when isSealed.
// selectionBound is the Println chunk bound (Model.scrollbackChunkRows) measured at
// SELECTION time. It rides the barrier as a conservative CAP only — the print fires
// after the render barrier (commitCmd), so Update re-measures the geometry at PRINT
// time and chunks to the SMALLER of the two (see the scrollbackCommitReadyMsg case).
func (q *scrollbackQueue) nextCommit(
	cells []TranscriptCell,
	sealedBlock func(i int) ScrollbackBlock,
	headerBlock func() ScrollbackBlock,
	selectionBound int,
) tea.Cmd {
	if q.inFlight {
		return nil
	}
	// 1. Masthead first.
	if !q.headerDone {
		blk := headerBlock()
		blk.Gen = q.gen
		q.inFlight = true
		q.inFlightCell = -1 // the masthead is not a transcript cell — nothing leaves the footer
		q.dropInFlightCell = false
		return commitCmd(blk, selectionBound)
	}
	// 2. Sealed transcript cells in index order from the cursor.
	for i := q.liveStart(len(cells)); i < len(cells); i++ {
		if !cells[i].isSealed() {
			// The first non-sealed (active) cell blocks the frontier — its successors
			// can't commit until it seals (append-only, strict order).
			return nil
		}
		blk := sealedBlock(i)
		blk.Gen = q.gen
		q.inFlight = true
		q.inFlightCell = i
		text := blk.Rendered
		if text == "" {
			text = blk.Plain
		}
		// Turns and command cards can occupy far more live rows than their immutable
		// commit tail suggests, so they always take the early-drop geometry path.
		// Only genuinely short system notes stay for the distinct barrier frame.
		q.dropInFlightCell = cells[i].Note == nil || lineCount(text) > repinDebtCap
		return commitCmd(blk, selectionBound)
	}
	return nil
}

// ack pops the in-flight head: clear inFlight, then advance the cursor. The header
// ack sets headerDone; a cell ack advances committed past that index (so the sealed
// cell leaves the footer). id is the just-committed block id; gen is the generation
// the block was emitted under.
//
// #4: a stale ack (gen != current — its commit was emitted BEFORE a reset) is
// ignored entirely. Acting on it would mark the new header done or bump `committed`
// for a fresh transcript, desyncing the footer/scrollback boundary. The reset
// already cleared inFlight, so the next commit re-emits from the top.
func (q *scrollbackQueue) ack(id string, gen, n int) {
	if gen != q.gen {
		return // stale ack from a pre-reset commit — drop it
	}
	q.release()
	if id == headerID {
		q.headerDone = true
		return
	}
	// A sealed cell committed: advance the cursor by one (cells commit in order).
	if q.committed < n {
		q.committed++
	}
}

// commitCmd builds the pre-print render barrier for a block. Bubble Tea only flushes
// View on its FPS ticker, so the actual tea.Println is scheduled later, from Update's
// scrollbackCommitReadyMsg handler, after it re-validates the queue generation and
// resize state. Splitting barrier from print prevents a delayed stale command from
// landing after /clear or a resize reset.
func commitCmd(blk ScrollbackBlock, selectionBound int) tea.Cmd {
	// Bubble Tea's renderer is ticker-flushed. Without a barrier, a commit's Printlns
	// would execute against a renderer cell buffer that has not yet caught up with the
	// selection-time View change (the committing cell just left the footer —
	// scrollbackQueue.inFlightCell), and insertAbove's geometry desyncs against the
	// stale, taller frame. The boot-time variant of that desync erased the whole
	// composer until a keypress (the 2026-07-10 MCP-note regression).
	//
	// Hold the print command for several renderer ticks first. That lets the SHORT
	// footer — without the committing cell — flush to the terminal; the Printlns then
	// land above it and slide it back onto the terminal's bottom row (insertAbove
	// consumes the rows the shrink left behind), so no dead scroll area survives below
	// the composer after the commit.
	//
	// Geometry across the barrier: the live footer can CHANGE while the barrier waits
	// (typed/pasted composer lines, a question/approval card appearing or closing, an
	// attention note), and a chunk taller than the rows above the footer at print time
	// makes insertAbove's CursorUp clamp — freezing a copy of the footer into scrollback
	// (#1613). Neither endpoint alone is safe: a bound frozen at SELECTION time misses a
	// footer that GREW during the barrier, while a fresh PRINT-time measurement misses a
	// footer that just SHRANK (footerRows updates on View() write, but the renderer's
	// cell buffer keeps the taller footer until its next ticker flush — insertAbove uses
	// the buffer's height). So the selection-time bound rides along as a CAP and Update
	// chunks to min(selectionBound, print-time bound): growth is caught by the fresh
	// measurement, shrink by this cap. (The residual sub-frame window — the footer
	// mutating between Update's measurement and the Println's execution — is the same
	// one-frame lag the +1 reserve margin in scrollbackChunkRows absorbs.)
	return tea.Tick(rendererSettleDelay, func(time.Time) tea.Msg {
		return scrollbackCommitReadyMsg{Block: blk, SelectionBound: selectionBound}
	})
}

// printCommitCmd performs the now-safe print-above operation and emits its ack. The
// ready-message reducer calls this only while the originating generation is still live
// and commits are not disarmed by a pending redraw.
func printCommitCmd(blk ScrollbackBlock, maxRows int) tea.Cmd {
	text := blk.Rendered
	if text == "" {
		text = blk.Plain
	}
	// Split a tall block into viewport-sized prints (chunkPrintlns), THEN ack. The ack
	// fires last so the one-in-flight discipline still holds: the queue head doesn't pop
	// until every slice of this block is in scrollback.
	cmds := chunkPrintlns(text, maxRows)
	cmds = append(cmds, func() tea.Msg { return ScrollbackCommittedMsg{ID: blk.ID, Gen: blk.Gen} })
	return tea.Sequence(cmds...)
}

// scrollbackCommitReadyMsg closes the renderer barrier. Block is the immutable render
// captured when the queue head was selected. SelectionBound is the chunk bound measured
// at selection time — a conservative cap folded into the print-time measurement via
// min() (see commitCmd), never the sole source. Update checks the block's generation
// before printing.
type scrollbackCommitReadyMsg struct {
	Block          ScrollbackBlock
	SelectionBound int
}
