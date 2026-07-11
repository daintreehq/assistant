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
//  1. GEOMETRY GOING STALE ACROSS THE BARRIER. The live footer can grow while the
//     barrier waits (a paste wrapping the composer, a question/approval card, an
//     attention note). A chunk bound frozen at SELECTION time can then exceed the rows
//     above the now-taller footer, and Bubble Tea's insertAbove CursorUp clamps at the
//     viewport top — freezing a copy of the footer into scrollback and erasing the live
//     one (#1613). The intermittent "footer flashes and disappears" bug. The bound must
//     be measured at PRINT time.
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

// TestCommitBarrier_ChunkBoundMeasuredAtPrintTime is the regression for the intermittent
// disappearing footer: the footer grows during the barrier window, and the print must
// chunk against the GROWN footer, not the height frozen when the block was selected.
func TestCommitBarrier_ChunkBoundMeasuredAtPrintTime(t *testing.T) {
	m := testModel(100) // rows = 40
	m.footerRows = new(int)
	m.commitArmed = true
	m.queue.headerDone = true
	m.queue.inFlight = true

	// A 30-row immutable block, pre-wrapped like every committed render.
	tall := strings.TrimRight(strings.Repeat("row\n", 30), "\n")
	blk := ScrollbackBlock{ID: "note_1", Kind: BlockNote, Rendered: tall, Gen: m.queue.gen}

	// Selection-time geometry: a short footer, under which the whole block fits ONE
	// Println. If the stale bound were reused at print time, one over-tall insertAbove
	// would clamp and wipe the footer.
	*m.footerRows = 3
	if staleBound := m.scrollbackChunkRows(); len(splitRowChunks(tall, staleBound)) != 1 {
		t.Fatalf("setup: block must fit one chunk at selection-time geometry (bound %d)", staleBound)
	}

	// The footer GROWS while the barrier waits.
	*m.footerRows = 30

	next, cmd := m.Update(scrollbackCommitReadyMsg{Block: blk})
	nm, ok := next.(Model)
	if !ok {
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
	if ack.ID != blk.ID || ack.Gen != blk.Gen {
		t.Fatalf("ack = %+v, want id %q gen %d", ack, blk.ID, blk.Gen)
	}

	prints := len(leafs) - 1
	printBound := nm.scrollbackChunkRows() // rows(40) - (footer 30 + 1) = 9
	want := len(splitRowChunks(tall, printBound))
	if want < 2 {
		t.Fatalf("setup: the grown footer must force multiple chunks (bound %d)", printBound)
	}
	if prints != want {
		t.Fatalf("printed %d chunks, want %d — the chunk bound must be measured at PRINT time (footer now %d rows), not frozen at selection time",
			prints, want, *m.footerRows)
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
