package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// attention_ported_test.go ports tests/ui/useAttentionSignal.test.tsx onto the Go
// cockpit: one BEL ding per fresh attention batch (none on an empty batch), and the
// OSC-2 window title badge that carries the inbox count and resets to plain when it
// drains. The live cockpit emits the BEL as a tea.Cmd (bellCmd) and the title via
// View.WindowTitle, so we assert the reducer's command + the title text.

func TestAttention_DingsOncePerNonEmptyBatch(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true // busy → no drain, just the ding + bookkeeping
	next, cmd := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{{Title: "Tests failed"}}})
	nm := next.(Model)
	if cmd == nil {
		t.Error("a fresh non-empty attention batch must ring the BEL (return a cmd)")
	}
	// The burst is queued for the wake reactor and the attention count bumps.
	if len(nm.pendingWake) != 1 {
		t.Errorf("attention burst must feed the wake queue: %d", len(nm.pendingWake))
	}
	if nm.attentionN == 0 {
		t.Error("attention count must bump on a fresh batch")
	}
}

func TestAttention_MultiEventBatchRingsOnce(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, cmd := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "a"}, {Title: "b"}, {Title: "c"},
	}})
	if cmd == nil {
		t.Error("a multi-event batch still rings exactly once (one cmd)")
	}
	if got := len(next.(Model).pendingWake); got != 3 {
		t.Errorf("all events feed the wake queue: %d, want 3", got)
	}
}

func TestAttention_EmptyBatchNoDing(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	_, cmd := m.onAttention(AttentionBatchMsg{Events: nil})
	if cmd != nil {
		t.Error("an empty attention batch must NOT ring the BEL")
	}
}

func TestWindowTitle_BadgeWithCountAndResetToPlain(t *testing.T) {
	m := liveModel(80)
	// No attention → plain title.
	if got := m.windowTitle(); got != "Daintree" {
		t.Errorf("idle title = %q, want Daintree", got)
	}
	// Inbox count surfaces in the OSC-2 title badge.
	m.attentionN = 2
	got := m.windowTitle()
	if !strings.Contains(got, "Daintree") || !strings.Contains(got, "2") {
		t.Errorf("title badge must carry the count: %q", got)
	}
	// Drains to zero → back to the plain title.
	m.attentionN = 0
	m.dashboard.Inbox = nil
	if got := m.windowTitle(); got != "Daintree" {
		t.Errorf("drained title = %q, want plain Daintree", got)
	}
}
