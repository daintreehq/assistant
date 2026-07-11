package ui

import (
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// commit_barrier_test.go locks the render-barrier commit protocol (scrollback.go
// commitCmd → Update's scrollbackCommitReadyMsg → printCommitCmd) against the two ways
// the 60ms barrier window can go wrong:
//
//  1. GEOMETRY GOING STALE ACROSS THE BARRIER. The live footer can change while the
//     barrier waits (a paste wrapping the composer, a question/approval card appearing
//     or closing, an attention note). A chunk taller than the rows above the footer at
//     print time makes Bubble Tea's insertAbove CursorUp clamp at the viewport top —
//     freezing a copy of the footer into scrollback and erasing the live one (#1613).
//     The intermittent "footer flashes and disappears" bug. Neither endpoint alone is
//     safe (growth invalidates the selection-time bound; shrink invalidates a fresh
//     print-time bound, because the renderer's cell buffer still holds the tall
//     footer), so the print chunks to min(selection bound, print-time bound).
//
//  2. A DROPPED BARRIER STRANDING THE QUEUE. When the barrier closes inside a resize's
//     disarm window its print must not land — but the queue's in-flight claim belonged
//     to that barrier, and holding a claim with no print (and therefore no ack) coming
//     would stall the commit queue forever. The drop must release the claim; a STALE
//     (pre-reset generation) barrier must instead leave the queue alone, because the
//     reset already re-claimed it for the new generation's commit.

// flattenCmd executes cmd and recursively expands Bubble Tea's sequence/batch wrapper
// messages (unexported []tea.Cmd types) into the flat, ordered leaf messages. Calling a
// barrier cmd blocks for its real tick (~60ms) — fine at test scale.
func flattenCmd(t *testing.T, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if msg == nil {
		return nil
	}
	v := reflect.ValueOf(msg)
	cmdType := reflect.TypeOf((*tea.Cmd)(nil)).Elem()
	if v.Kind() == reflect.Slice && v.Type().Elem() == cmdType {
		var out []tea.Msg
		for i := 0; i < v.Len(); i++ {
			sub, _ := v.Index(i).Interface().(tea.Cmd)
			out = append(out, flattenCmd(t, sub)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// runBarrierPrint drives the REAL commit chain for one tall sealed block: selection via
// scheduleCommit (which stamps the selection-time bound into the barrier), the barrier
// tick, an optional footer mutation while the barrier waits, then the ready message
// through the real Update. It returns the number of chunk Printlns the print emitted.
func runBarrierPrint(t *testing.T, footerAtSelection, footerAtPrint int, rendered string) int {
	t.Helper()
	m := testModel(100) // rows = 40
	m.footerRows = new(int)
	m.commitArmed = true
	m.queue.headerDone = true
	m.transcript = []TranscriptCell{
		{Note: &NoteCell{ID: "note_1", Level: NoteInfo, Text: "sealed"}},
	}
	*m.footerRows = footerAtSelection

	// SELECTION under the selection-time footer, exactly as scheduleCommit does it —
	// same queue call, same selection-bound stamp — but with a pinned block render so
	// the expected chunk counts are exact regardless of note styling.
	barrier := m.queue.nextCommit(
		m.transcript,
		func(int) ScrollbackBlock { return ScrollbackBlock{ID: "note_1", Kind: BlockNote, Rendered: rendered} },
		m.headerBlock,
		m.scrollbackChunkRows(),
	)
	if barrier == nil || !m.queue.inFlight {
		t.Fatal("selection must claim the queue and return the barrier")
	}
	msgs := flattenCmd(t, barrier) // waits out the real render barrier tick
	if len(msgs) != 1 {
		t.Fatalf("barrier yielded %d messages, want exactly the ready message", len(msgs))
	}
	ready, ok := msgs[0].(scrollbackCommitReadyMsg)
	if !ok {
		t.Fatalf("barrier produced %T, want scrollbackCommitReadyMsg", msgs[0])
	}

	// The footer mutates while the barrier waits.
	*m.footerRows = footerAtPrint

	next, cmd := m.Update(ready)
	if _, ok := next.(Model); !ok {
		t.Fatalf("Update returned %T, want ui.Model", next)
	}
	if cmd == nil {
		t.Fatal("a still-current barrier must print")
	}
	leafs := flattenCmd(t, cmd)
	if len(leafs) < 2 {
		t.Fatalf("print command yielded %d messages, want chunk prints + ack", len(leafs))
	}
	ack, ok := leafs[len(leafs)-1].(ScrollbackCommittedMsg)
	if !ok {
		t.Fatalf("last message = %T, want the ScrollbackCommittedMsg ack", leafs[len(leafs)-1])
	}
	if ack.ID != "note_1" || ack.Gen != ready.Block.Gen {
		t.Fatalf("ack = %+v, want id note_1 gen %d", ack, ready.Block.Gen)
	}
	return len(leafs) - 1
}

// TestCommitBarrier_FooterGrowsDuringBarrier is the regression for the intermittent
// disappearing footer: the footer GROWS during the barrier window (a paste, a question
// or approval card, an attention note), and the print must chunk against the grown
// footer — a bound frozen at selection time would emit one over-tall insertAbove, whose
// CursorUp clamp freezes a copy of the footer into scrollback and erases the live one.
func TestCommitBarrier_FooterGrowsDuringBarrier(t *testing.T) {
	tall := strings.TrimRight(strings.Repeat("row\n", 30), "\n")
	// Selection: footer 3 → bound 36, the block fits ONE chunk (the unsafe stale count).
	// Print: footer 30 → bound 9 → min(36, 9) = 9 → ceil(30/9) = 4 chunks.
	prints := runBarrierPrint(t, 3, 30, tall)
	if prints != 4 {
		t.Fatalf("printed %d chunks, want 4 — the bound must be re-measured at PRINT time, not frozen at selection (stale bound would print 1 over-tall chunk)", prints)
	}
}

// TestCommitBarrier_FooterShrinksDuringBarrier is the mirror regression: the footer
// SHRINKS during the barrier window (a question card closes). footerRows updates the
// instant View() is written, but Bubble Tea's renderer keeps the TALLER footer in its
// cell buffer until the next ticker flush — and insertAbove uses the buffer's height.
// A print-time-only measurement would trust the new short footer and emit one over-tall
// chunk into a screen still holding the tall one. The selection-time bound must cap it.
func TestCommitBarrier_FooterShrinksDuringBarrier(t *testing.T) {
	tall := strings.TrimRight(strings.Repeat("row\n", 30), "\n")
	// Selection: footer 30 → bound 9. Print: footer 3 → fresh bound 36; min(9, 36) = 9
	// → 4 chunks, each safe under EITHER footer height.
	prints := runBarrierPrint(t, 30, 3, tall)
	if prints != 4 {
		t.Fatalf("printed %d chunks, want 4 — the selection-time bound must cap a print-time measurement taken after the footer shrank (fresh-only bound would print 1 over-tall chunk)", prints)
	}
}

// TestCommitBarrier_StaleGenerationNeverReleasesNewClaim proves a barrier that outlived
// a queue reset is dropped WITHOUT touching the queue: the reset already cleared the old
// claim and the new generation's commit holds the current one.
func TestCommitBarrier_StaleGenerationNeverReleasesNewClaim(t *testing.T) {
	m := testModel(100)
	m.commitArmed = true

	// The masthead goes in flight; its barrier carries the current generation.
	barrier := m.scheduleCommit()
	if barrier == nil || !m.queue.inFlight {
		t.Fatal("masthead selection must claim the queue")
	}
	msgs := flattenCmd(t, barrier) // waits out the real render barrier
	if len(msgs) != 1 {
		t.Fatalf("barrier yielded %d messages, want exactly the ready message", len(msgs))
	}
	ready, ok := msgs[0].(scrollbackCommitReadyMsg)
	if !ok {
		t.Fatalf("barrier produced %T, want scrollbackCommitReadyMsg", msgs[0])
	}
	if ready.Block.ID != headerID || ready.Block.Gen != m.queue.gen {
		t.Fatalf("barrier block = %+v, want the masthead under gen %d", ready.Block, m.queue.gen)
	}

	// A /clear-style reset lands while the barrier waits, then the NEW generation's
	// masthead commit claims the queue.
	m.clearNonce++
	m.queue.applyResetKey(m.clearNonce + m.redrawNonce)
	if m.scheduleCommit() == nil || !m.queue.inFlight {
		t.Fatal("the post-reset recommit must claim the queue")
	}

	// The STALE barrier closes now: no print, and the new claim must survive.
	next, cmd := m.Update(ready)
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("Update returned %T, want ui.Model", next)
	}
	if cmd != nil {
		t.Fatal("a stale-generation barrier must never print")
	}
	if !nm.queue.inFlight {
		t.Fatal("a stale barrier must not release the new generation's in-flight claim")
	}
}

// TestCommitBarrier_ResizeDisarmDropsPrintReleasesClaimAndRecovers drives the real
// reducer sequence of a resize landing inside a barrier window: the print is dropped,
// the queue claim is released (no permanent stall), and the debounced redraw then
// recommits the whole transcript — including the dropped block — under the new
// generation.
func TestCommitBarrier_ResizeDisarmDropsPrintReleasesClaimAndRecovers(t *testing.T) {
	m := bootToSteadyState(t) // 100x40, masthead + boot notes committed
	base := len(m.transcript)

	// Seal a fresh note; afterStateChange puts its commit in flight (barrier pending).
	m, cmd := step(t, m, LogMsg{Level: NoteInfo, Text: "adopted 2 watchers"})
	if !m.queue.inFlight {
		t.Fatal("a sealed note must claim the queue")
	}
	var ready *scrollbackCommitReadyMsg
	for _, msg := range flattenCmd(t, cmd) {
		if r, ok := msg.(scrollbackCommitReadyMsg); ok {
			ready = &r
			break
		}
	}
	if ready == nil {
		t.Fatal("afterStateChange must schedule the note's commit barrier")
	}

	// A resize lands inside the barrier window: commits disarm for the debounce.
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	if m.commitArmed || !m.redrawPending {
		t.Fatal("a resize must disarm commits and mark the redraw pending")
	}

	// The barrier closes inside the disarm window: the print must be dropped AND the
	// claim released — a held claim with no ack coming would stall the queue forever.
	var dropCmd tea.Cmd
	m, dropCmd = step(t, m, *ready)
	if dropCmd != nil {
		t.Fatal("a barrier closing in the disarm window must not print")
	}
	if m.queue.inFlight {
		t.Fatal("the dropped barrier must release the queue's in-flight claim (no stall)")
	}

	// Recovery: debounced redraw → re-arm → the WHOLE transcript recommits at 90x30.
	genBefore := m.queue.gen
	m, _ = step(t, m, RedrawMsg{Nonce: m.resizePending})
	if m.queue.gen == genBefore {
		t.Fatal("the redraw must bump the commit generation")
	}
	if m.redrawPending {
		t.Fatal("the redraw must clear redrawPending")
	}
	m, _ = step(t, m, CommitArmMsg{})
	if !m.commitArmed {
		t.Fatal("the redraw's arm tick must re-arm commits")
	}
	m = drainCommits(t, m)
	if !m.queue.headerDone {
		t.Fatal("recovery must recommit the masthead")
	}
	if m.queue.committed != base+1 {
		t.Fatalf("recovery committed %d cells, want %d — every cell including the dropped note must recommit",
			m.queue.committed, base+1)
	}
}
