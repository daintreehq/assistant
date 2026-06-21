package ui

import (
	tea "charm.land/bubbletea/v2"
)

// scrollback.go implements the commit-queue protocol (ui-transcript.md §2). Sealed
// cells + the masthead become immutable ScrollbackBlocks committed to the host's
// native scrollback ONE at a time, via Bubble Tea's print-above-program command
// (tea.Println). Each commit is acked by a ScrollbackCommittedMsg that pops the
// queue head, advances the committed cursor (dropping the sealed cell from the live
// footer), and schedules the next. Masthead is committed first. NO portal/settle/
// replay — that OpenTUI machinery does not port.

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
	gen        int  // commit generation (#4); bumped on every reset, stamped on each block
}

// applyResetKey re-arms the queue when the reset key changes (a /clear or resize
// redraw): committed=0, headerDone=false, and a bumped generation. Length alone
// can't detect a clear (a fresh confirmation card can make the new length equal the
// old committed count), so the monotonic key makes the reset deterministic (§2).
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
		q.inFlight = false // the prior in-flight ack is now stale (wrong gen) → ignored
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
// Commit order (§2): masthead first, then sealed transcript cells in index order
// from the committed cursor forward. A cell is eligible only when isSealed.
func (q *scrollbackQueue) nextCommit(
	cells []TranscriptCell,
	sealedBlock func(i int) ScrollbackBlock,
	headerBlock func() ScrollbackBlock,
) tea.Cmd {
	if q.inFlight {
		return nil
	}
	// 1. Masthead first.
	if !q.headerDone {
		blk := headerBlock()
		blk.Gen = q.gen
		q.inFlight = true
		return commitCmd(blk)
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
		return commitCmd(blk)
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
	q.inFlight = false
	if id == headerID {
		q.headerDone = true
		return
	}
	// A sealed cell committed: advance the cursor by one (cells commit in order).
	if q.committed < n {
		q.committed++
	}
}

// commitCmd builds the print-above-program command for a block. It prefers the
// styled Rendered string and falls back to Plain when rendering produced nothing,
// then emits a ScrollbackCommittedMsg ack carrying the block id. tea.Println prints
// ABOVE the live program and persists across renders — exactly the native-scrollback
// commit (ui-transcript.md §1 mapping).
func commitCmd(blk ScrollbackBlock) tea.Cmd {
	text := blk.Rendered
	if text == "" {
		text = blk.Plain
	}
	return tea.Sequence(
		tea.Println(text),
		func() tea.Msg { return ScrollbackCommittedMsg{ID: blk.ID, Gen: blk.Gen} },
	)
}
