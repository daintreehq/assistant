package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// attention_ported_test.go exercises the attention signal on the Go
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

// noteTexts returns the text of every standalone NoteCell in the transcript, in order.
func noteTexts(m Model) []string {
	var out []string
	for _, c := range m.transcript {
		if c.Note != nil {
			out = append(out, c.Note.Text)
		}
	}
	return out
}

// noteLevels returns the level (spine tone) of every standalone NoteCell, in order.
func noteLevels(m Model) []NoteLevel {
	var out []NoteLevel
	for _, c := range m.transcript {
		if c.Note != nil {
			out = append(out, c.Note.Level)
		}
	}
	return out
}

// TestAttention_EmitsTranscriptNotePerEvent is the #175 contract: a fresh attention batch
// drops one glanceable, /inbox-styled note into the transcript per event — "<glyph> <Title>
// — [term/wt <id>]" — with the spine toned by severity. The events are delivered exactly
// once by the scheduler, so one batch ⇒ exactly len(Events) notes (no dedupe in the UI).
func TestAttention_EmitsTranscriptNotePerEvent(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true // busy → no drain; just the badge/BEL bookkeeping + the notes
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "needs input", Severity: domain.SeverityUrgent, Target: &domain.EventTarget{TerminalID: "term_8"}},
		{Title: "Deploy finished", Severity: domain.SeverityDone, Target: &domain.EventTarget{WorktreeID: "wt_3"}},
		{Title: "watcher armed", Severity: domain.SeverityAttention},
	}})
	nm := next.(Model)

	notes := noteTexts(nm)
	want := []string{
		"‼ needs input — [term term_8]",
		"✓ Deploy finished — [wt wt_3]",
		"! watcher armed",
	}
	if len(notes) != len(want) {
		t.Fatalf("expected one transcript note per event, got %d: %v", len(notes), notes)
	}
	for i, w := range want {
		if notes[i] != w {
			t.Errorf("note[%d] = %q, want %q", i, notes[i], w)
		}
	}
	// The spine tone tracks severity: urgent → NoteError, done → NoteSuccess, attention → NoteWarn.
	levels := noteLevels(nm)
	wantLevels := []NoteLevel{NoteError, NoteSuccess, NoteWarn}
	for i, w := range wantLevels {
		if levels[i] != w {
			t.Errorf("note[%d] level = %d, want %d", i, levels[i], w)
		}
	}
}

// TestAttention_NoteTerminalWinsOverWorktree mirrors queue.Format: when an event scopes to
// both a terminal and a worktree, the terminal id is the one that surfaces.
func TestAttention_NoteTerminalWinsOverWorktree(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "both set", Severity: domain.SeverityInfo, Target: &domain.EventTarget{TerminalID: "term_2", WorktreeID: "wt_9"}},
	}})
	notes := noteTexts(next.(Model))
	if len(notes) != 1 || notes[0] != "ℹ both set — [term term_2]" {
		t.Fatalf("terminal must win over worktree in the target suffix: %v", notes)
	}
}

// TestAttention_NoteUnknownSeverityFallsBackToInfo proves the ELSE-1 fallback: a severity
// the cockpit doesn't recognize still renders (the info glyph + neutral tone), never blank.
func TestAttention_NoteUnknownSeverityFallsBackToInfo(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "mystery", Severity: domain.Severity("bogus")},
	}})
	nm := next.(Model)
	notes := noteTexts(nm)
	if len(notes) != 1 || notes[0] != "ℹ mystery" {
		t.Fatalf("unknown severity must fall back to the info glyph: %v", notes)
	}
	if lv := noteLevels(nm)[0]; lv != NoteInfo {
		t.Errorf("unknown severity spine = %d, want NoteInfo", lv)
	}
}

// TestAttention_NoteNilTargetHasNoSuffix: a targetless event renders just "<glyph> <Title>"
// with no trailing " — " separator.
func TestAttention_NoteNilTargetHasNoSuffix(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "no target", Severity: domain.SeverityInfo},
	}})
	notes := noteTexts(next.(Model))
	if len(notes) != 1 || strings.Contains(notes[0], "—") {
		t.Fatalf("a targetless event must not render the ' — ' separator: %v", notes)
	}
	if notes[0] != "ℹ no target" {
		t.Errorf("note = %q, want %q", notes[0], "ℹ no target")
	}
}

// TestAttention_EmptyBatchEmitsNoNote: the empty-batch guard short-circuits before any note.
func TestAttention_EmptyBatchEmitsNoNote(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: nil})
	if got := len(noteTexts(next.(Model))); got != 0 {
		t.Errorf("an empty batch must not append any note, got %d", got)
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
