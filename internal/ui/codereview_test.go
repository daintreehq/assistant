package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// codereview_test.go locks in the CODE-REVIEW concurrency/ordering fixes:
// in-stream completion + ordering barrier (#1), coalescer order vs non-token events
// (#2), ordered /clear & resize sequence (#3), generation-safe scrollback reset (#4),
// /clear rejected mid-turn (#5), one wake retry (#9), and boot input (#10).

// --- #2: a non-token event can never land before the trailing token chunk ---

// TestPump_TokensOrderedBeforeNonTokenEvent drives the pump exactly the way the
// runtime does — buffer tokens, then emit an end — and drains the channel. Because
// drain+enqueue is serialized through one lock + a single sender, the coalesced
// pumpTokens must arrive BEFORE the pumpEnd, never after.
func TestPump_TokensOrderedBeforeNonTokenEvent(t *testing.T) {
	for iter := 0; iter < 200; iter++ {
		p := newEventPump()
		// Stream a few tokens (they buffer), then a non-token event flushes them first.
		p.AssistantToken("Hello ")
		p.AssistantToken("world")
		p.AssistantEnd("Hello world", "")

		var kinds []pumpKind
		// Drain until we see the end (the only terminal event here).
		for {
			ev := <-p.ch
			kinds = append(kinds, ev.kind)
			if ev.kind == pumpEnd {
				break
			}
		}
		// Tokens must come first, end last — never end then tokens.
		if len(kinds) < 2 || kinds[0] != pumpTokens || kinds[len(kinds)-1] != pumpEnd {
			t.Fatalf("iter %d: kinds = %v, want [pumpTokens … pumpEnd]", iter, kinds)
		}
		for i, k := range kinds[:len(kinds)-1] {
			if k != pumpTokens {
				t.Fatalf("iter %d: non-token event at %d before end: %v", iter, i, kinds)
			}
		}
	}
}

// --- #1: completion rides the ordered stream and never overtakes events ---

// TestPump_CompletionAfterQueuedEvents proves a Complete enqueued after a token
// burst drains AFTER those tokens (completion can't overtake queued AgentEventMsgs).
func TestPump_CompletionAfterQueuedEvents(t *testing.T) {
	p := newEventPump()
	p.AssistantToken("partial answer")
	p.Complete(completionPayload{runID: "turn_1", reply: "done"})

	first := <-p.ch
	if first.kind != pumpTokens || first.text != "partial answer" {
		t.Fatalf("expected the buffered tokens first, got %v", first.kind)
	}
	second := <-p.ch
	if second.kind != pumpComplete || second.completion.runID != "turn_1" {
		t.Fatalf("expected completion second, got %v", second.kind)
	}
}

// TestOnTurnComplete_OrderingBarrierRejectsStale proves a completion tagged with a
// run id that is no longer the active turn does NOT seal/promote the wrong turn.
func TestOnTurnComplete_OrderingBarrierRejectsStale(t *testing.T) {
	m := liveModel(80)
	// A fresh turn is active under "turn_new".
	next, _ := m.startTurn("new task")
	m = next.(Model)
	activeBefore := m.activeTurn
	cell := m.activeTurnCell()

	// A stale completion for an OLD turn arrives — barrier must ignore it.
	next, _ = m.onTurnComplete(TurnCompleteMsg{RunID: "turn_OLD", Reply: "stale"})
	m = next.(Model)
	if m.activeTurn != activeBefore {
		t.Fatalf("stale completion cleared the active turn: %q", m.activeTurn)
	}
	if !m.inFlight {
		t.Error("stale completion must not free the single-flight lock")
	}
	if cell.Phase == domain.PhaseComplete {
		t.Error("stale completion must not seal the live turn")
	}

	// The matching completion does seal it.
	next, _ = m.onTurnComplete(TurnCompleteMsg{RunID: activeBefore, Reply: "real"})
	m = next.(Model)
	if m.activeTurn != "" || m.inFlight {
		t.Error("matching completion must seal + free the lock")
	}
}

// --- #8: a surfaced Send failure seals a FAILED turn with a note ---

func TestOnTurnComplete_FailedTurnSurfacesNote(t *testing.T) {
	m := liveModel(80)
	next, _ := m.startTurn("task")
	m = next.(Model)
	id := m.activeTurn
	notesBefore := countNotes(m.transcript)

	// Failed with a NON-sentinel reply (e.g. ErrTurnInProgress masqueraded as empty
	// normal completion) must seal Failed + add an error note rather than read clean.
	next, _ = m.onTurnComplete(TurnCompleteMsg{RunID: id, Reply: "ignored", Failed: true})
	m = next.(Model)
	var sealed *TurnCell
	for i := range m.transcript {
		if m.transcript[i].Turn != nil && m.transcript[i].Turn.ID == id {
			sealed = m.transcript[i].Turn
		}
	}
	if sealed == nil || sealed.State != TurnFailed {
		t.Fatalf("failed turn not sealed Failed: %+v", sealed)
	}
	if countNotes(m.transcript) != notesBefore+1 {
		t.Error("a surfaced Send failure must add a note")
	}
}

func countNotes(cells []TranscriptCell) int {
	n := 0
	for _, c := range cells {
		if c.Note != nil {
			n++
		}
	}
	return n
}

// --- #3: /clear & resize use an ordered Sequence (wipe before recommit) ---

func TestOnClear_OrderedSequenceWipeThenRecommit(t *testing.T) {
	m := liveModel(80)
	m.transcript = []TranscriptCell{{Turn: &TurnCell{ID: "t1", State: TurnComplete}}}
	_, cmd := m.onClear("Conversation cleared", "fresh")
	if cmd == nil {
		t.Fatal("onClear must return a wipe-then-recommit cmd")
	}
	// A Sequence is a single sequenceMsg-batched cmd; running it and asserting the
	// masthead recommits is covered by TestOnClear_MastheadRecommitsAfterClear. Here
	// we only assert a non-nil ordered cmd is returned (the type is unexported).
}

// TestOnClear_MastheadRecommitsAfterClear proves the scrollback queue re-arms so the
// masthead is committed again after a /clear (the reset bumps the key/generation and
// nextCommit re-emits the header first).
func TestOnClear_MastheadRecommitsAfterClear(t *testing.T) {
	m := liveModel(80)
	// Pretend the masthead was already committed once.
	m.queue.headerDone = true
	m.queue.committed = 1
	genBefore := m.queue.gen

	next, _ := m.onClear("Conversation cleared", "fresh")
	m = next.(Model)
	if m.queue.headerDone {
		t.Error("clear must re-arm headerDone=false so the masthead recommits")
	}
	if m.queue.committed != 0 {
		t.Errorf("clear must reset committed to 0, got %d", m.queue.committed)
	}
	if m.queue.gen == genBefore {
		t.Error("clear must bump the commit generation (#4)")
	}
	// Like onRedraw/completeBoot, clear DISARMS commits and re-arms them one render cycle out
	// (commitArmCmd → CommitArmMsg) so the masthead recommits above a re-flushed footer rather
	// than at a stale height (#1613). So nothing is in flight yet right after onClear.
	if m.commitArmed {
		t.Fatal("clear must disarm commits so the masthead recommit is deferred one cycle")
	}
	if m.queue.inFlight {
		t.Fatal("clear must DEFER the masthead recommit, not schedule it immediately")
	}
	// Simulate the CommitArmMsg tick (commitArmed flips true, afterStateChange → scheduleCommit):
	// the masthead commit goes in flight under the new generation. Acking it marks the masthead
	// done again — proving the masthead recommits after a clear.
	m.commitArmed = true
	if cmd := m.scheduleCommit(); cmd == nil {
		t.Fatal("once re-armed, clear must schedule the masthead recommit")
	}
	if !m.queue.inFlight {
		t.Fatal("the re-armed commit must be in flight")
	}
	m.queue.ack(headerID, m.queue.gen, len(m.transcript))
	if !m.queue.headerDone {
		t.Error("the masthead must recommit (and ack) after a clear")
	}
}

// --- #4: a stale ack (pre-reset generation) is ignored ---

func TestScrollback_StaleAckAfterResetIgnored(t *testing.T) {
	var q scrollbackQueue
	cells := []TranscriptCell{{Turn: &TurnCell{ID: "t1", State: TurnComplete}}}
	header := func() ScrollbackBlock { return ScrollbackBlock{ID: headerID} }
	sealed := func(i int) ScrollbackBlock { return ScrollbackBlock{ID: cells[i].ID()} }

	// Commit the masthead at gen 0 (it goes in flight, carrying gen 0).
	if q.nextCommit(cells, sealed, header, 80) == nil {
		t.Fatal("expected a masthead commit")
	}
	staleGen := q.gen

	// A reset lands before the ack (a /clear or resize redraw): bump generation.
	q.applyResetKey(1)
	if q.inFlight {
		t.Fatal("reset must clear inFlight so the next commit re-emits from the top")
	}

	// The stale ack (gen 0) now arrives — it must be IGNORED: it must not mark the new
	// header done or bump committed for the fresh transcript.
	q.ack(headerID, staleGen, len(cells))
	if q.headerDone {
		t.Fatal("a stale (pre-reset) ack must not mark the new header done")
	}

	// The masthead still commits fresh under the new generation, and its ack lands.
	cmd := q.nextCommit(cells, sealed, header, 80)
	if cmd == nil {
		t.Fatal("the masthead must recommit after the reset")
	}
	q.ack(headerID, q.gen, len(cells))
	if !q.headerDone {
		t.Fatal("the fresh (current-gen) ack must mark the new masthead done")
	}
}

// --- #9: a failed wake retries exactly once ---

func TestWake_FailedRetriesExactlyOnce(t *testing.T) {
	m := liveModel(80)
	// A wake burst is pending and the model is idle → drainPending fires it.
	m.pendingWake = []domain.QueueEvent{{Title: "term_1 needs input"}}
	next, _ := m.drainPending()
	m = next.(Model)
	if !m.inFlight || m.activeTurn == "" {
		t.Fatal("drainPending must dispatch the wake turn")
	}
	if len(m.activeWake) != 1 {
		t.Fatalf("the active burst must be retained for retry, got %d", len(m.activeWake))
	}
	wakeID := m.activeTurn

	// The wake FAILS → the burst is requeued exactly once.
	next, _ = m.onWakeComplete(WakeCompleteMsg{RunID: wakeID, Failed: true})
	m = next.(Model)
	// drainPending re-fired the requeued burst (idle → immediate retry).
	if !m.inFlight {
		t.Fatal("a failed wake with budget must requeue + re-dispatch the burst")
	}
	if !m.wakeRetried {
		t.Error("wakeRetried must be set after the one retry")
	}
	retryID := m.activeTurn

	// The retry ALSO fails → no second retry (budget exhausted), settles idle.
	next, _ = m.onWakeComplete(WakeCompleteMsg{RunID: retryID, Failed: true})
	m = next.(Model)
	if m.inFlight {
		t.Error("a second failure must NOT retry (one-retry budget)")
	}
	if len(m.pendingWake) != 0 {
		t.Errorf("the burst must not requeue a second time: %+v", m.pendingWake)
	}
}

// --- #10: a keystroke during boot reaches the composer ---

func TestBoot_ComposerReceivesInput(t *testing.T) {
	m := liveModel(80)
	m.booting = true
	// A printable key during boot must reach the composer (not be swallowed).
	next, _ := m.onKey(tea.KeyPressMsg{Code: 'h', Text: "h"})
	nm := next.(Model)
	if nm.composer.Value() == "" {
		t.Fatal("a keystroke during boot must reach the composer (input not gated)")
	}
	if !strings.Contains(nm.composer.Value(), "h") {
		t.Errorf("composer value after boot keystroke = %q, want it to contain 'h'", nm.composer.Value())
	}
}
