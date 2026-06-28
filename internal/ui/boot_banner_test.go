package ui

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// The one-time boot "Connected to Daintree MCP" banner must commit directly under the
// masthead, never below a turn the user already ran. The boot connect is async and the 8s
// bootCap can drop the cockpit in before it resolves, so a turn (with a SUCCESSFUL MCP
// call) can precede the late MCPConnectedMsg. The banner is therefore gated on an
// empty-of-work transcript, not on connect timing.
func TestMCPConnectedBannerGatedToTopOfTranscript(t *testing.T) {
	// Fresh transcript (the normal/idle case — connect resolved before any turn): the banner
	// is added, degraded clears, and the boot gate half (mcpResolved) flips.
	m := harnessModel()
	m, _ = step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	if !hasConnectedNote(m) {
		t.Error("with no turn yet, MCPConnectedMsg should add the 'Connected to Daintree MCP' note")
	}
	if m.degraded {
		t.Error("MCPConnectedMsg should clear the degraded flag")
	}
	if !m.mcpResolved {
		t.Error("MCPConnectedMsg should mark mcpResolved (the boot-gate half)")
	}

	// A turn already sits in the transcript (the bootCap let it start before MCP resolved):
	// a late connect must NOT append the banner below that turn — but must still clear
	// degraded and resolve the gate.
	m2 := harnessModel()
	m2.degraded = true
	m2.transcript = append(m2.transcript, TranscriptCell{Turn: &TurnCell{ID: domain.NewID("turn_"), State: TurnActive}})
	m2, _ = step(t, m2, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	if hasConnectedNote(m2) {
		t.Error("with a turn already in the transcript, a late MCPConnectedMsg must NOT add the banner")
	}
	if m2.degraded {
		t.Error("MCPConnectedMsg must clear degraded even when the banner is suppressed")
	}
	if !m2.mcpResolved {
		t.Error("MCPConnectedMsg must resolve the boot gate even when the banner is suppressed")
	}
}

// transcriptHasWork ignores notes but trips on a slash-command cell, so a command run
// before the connect resolves also suppresses the late banner.
func TestTranscriptHasWork_NotesVsWork(t *testing.T) {
	m := harnessModel()
	m.addNote(NoteSuccess, "some status note")
	if m.transcriptHasWork() {
		t.Error("a note alone is not work — the banner should still be allowed")
	}
	m.transcript = append(m.transcript, TranscriptCell{Command: &CommandCell{}})
	if !m.transcriptHasWork() {
		t.Error("a command cell counts as work — the banner must be suppressed")
	}
}

// TestBootBanner_CommitsAboveFirstTurn is the END-TO-END regression for the user-visible bug:
// the "Connected to Daintree MCP" banner appearing BELOW the ◆ DAINTREE marker of the first
// turn instead of under the masthead. It drives the REAL reducer — connect resolves (note added
// under the empty transcript), the user's first turn starts, then the commit queue drains
// one-block-per-ack. The two scrollback paths race: the queue commits the note while the
// incremental flush prints the active turn's preamble. The flush MUST hold until the note has
// committed, so the note lands in scrollback AHEAD of the turn's "◆ DAINTREE" marker — never
// mid-turn. (The unit-level gate is locked by TestFlush_HoldsForUncommittedEarlierCell; this
// proves the wiring — afterStateChange ordering + ack-driven re-flush — end to end.)
func TestBootBanner_CommitsAboveFirstTurn(t *testing.T) {
	m := harnessModel()
	m.ctx = context.Background() // startTurn → controller.runTurn needs a non-nil ctx
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 100, Height: 40})
	m, _ = step(t, m, CommitArmMsg{})

	// Connect resolves while the transcript is still empty → the banner is appended.
	m, _ = step(t, m, MCPConnectedMsg{Transport: "http", ToolCount: 3})
	if !hasConnectedNote(m) {
		t.Fatal("MCPConnectedMsg should add the connected note under the empty transcript")
	}

	// The user's first turn starts before the (still-uncommitted) note has drained — exactly the
	// bootCap race. The note now sits AHEAD of the active turn in the transcript.
	next, _ := m.startTurn("ask three agents for a fact")
	m = next.(Model)
	turn := m.activeTurnCell() // shared *TurnCell — its FlushedRows tracks across step() copies
	if turn == nil {
		t.Fatal("startTurn did not create an active turn")
	}
	turnIdx := m.activeTurnIndex()
	noteIdx := -1
	for i, c := range m.transcript {
		if c.Note != nil && c.Note.Text == "Connected to Daintree MCPs" {
			noteIdx = i
		}
	}
	if noteIdx < 0 || noteIdx >= turnIdx {
		t.Fatalf("note (idx %d) must sit AHEAD of the turn (idx %d)", noteIdx, turnIdx)
	}

	// Drain the commit queue one ack at a time (mirrors the program loop). The masthead commits
	// first, then the note. Until committed reaches the turn's index, the turn must NOT have
	// flushed — its preamble would otherwise print ABOVE the not-yet-committed note (mid-turn
	// banner). This is the assertion that fails WITHOUT the flush gate.
	for i := 0; i < 32; i++ {
		if !m.queue.inFlight {
			_ = m.scheduleCommit()
		}
		if !m.queue.inFlight {
			break // queue quiescent (it blocks at the non-sealed active turn)
		}
		if m.queue.committed < turnIdx && turn.FlushedRows != 0 {
			t.Fatalf("turn flushed (FlushedRows=%d) before the note committed (committed=%d, turnIdx=%d) — banner would land mid-turn",
				turn.FlushedRows, m.queue.committed, turnIdx)
		}
		id := headerID
		if m.queue.headerDone {
			id = m.transcript[m.queue.liveStart(len(m.transcript))].ID()
		}
		m, _ = step(t, m, ScrollbackCommittedMsg{ID: id, Gen: m.queue.gen})
	}

	// Every cell ahead of the turn (masthead + the connected note) is now in scrollback, ABOVE
	// the marker — and only now does the turn's preamble flush.
	if m.queue.committed < turnIdx {
		t.Fatalf("queue never reached the turn (committed=%d, turnIdx=%d)", m.queue.committed, turnIdx)
	}
	if turn.FlushedRows == 0 {
		t.Error("the turn's preamble should flush once all earlier cells (incl. the note) committed")
	}
}

// hasConnectedNote reports whether the transcript carries the boot connect banner.
func hasConnectedNote(m Model) bool {
	for _, c := range m.transcript {
		if c.Note != nil && c.Note.Text == "Connected to Daintree MCPs" {
			return true
		}
	}
	return false
}
