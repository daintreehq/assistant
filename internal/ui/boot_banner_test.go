package ui

import (
	"testing"

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

// hasConnectedNote reports whether the transcript carries the boot connect banner.
func hasConnectedNote(m Model) bool {
	for _, c := range m.transcript {
		if c.Note != nil && c.Note.Text == "Connected to Daintree MCP" {
			return true
		}
	}
	return false
}
