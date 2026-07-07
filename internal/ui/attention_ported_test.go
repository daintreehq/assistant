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

// TestAttention_SingleEventEmitsFullNote is the surviving half of the #175 contract: a
// SINGLE fresh attention event still drops its full glanceable, /inbox-styled note into
// the transcript — "<Title> — [term/wt <id>]" — with the spine toned by severity. (The
// precise glyph renders on the spine via renderNoteCell, not baked into the text.)
func TestAttention_SingleEventEmitsFullNote(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true // busy → no drain; just the badge/BEL bookkeeping + the note
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "needs input", Severity: domain.SeverityUrgent, Target: &domain.EventTarget{TerminalID: "term_8"}},
	}})
	nm := next.(Model)
	notes := noteTexts(nm)
	if len(notes) != 1 || notes[0] != "needs input — [term term_8]" {
		t.Fatalf("a single event keeps its full note line, got %v", notes)
	}
	if lv := noteLevels(nm)[0]; lv != NoteError {
		t.Errorf("urgent spine = %d, want NoteError", lv)
	}
}

// TestAttention_MultiEventBatchCoalescesToOneNote is the anti-clutter contract: a batch
// of N>1 events (a cohort of watchers settling together) commits ONE compact note —
// "<N> updates — <most-severe title> · +<N-1> more (/inbox)" — toned by the batch's max
// severity, instead of N spaced-out near-identical lines that bury the conversation.
// The full detail stays in /inbox (and in the wake turn's own summary); the wake queue
// and badge still carry every event.
func TestAttention_MultiEventBatchCoalescesToOneNote(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "watch A: completed success", Severity: domain.SeverityAttention, Target: &domain.EventTarget{TerminalID: "term_1"}},
		{Title: "needs input", Severity: domain.SeverityUrgent, Target: &domain.EventTarget{TerminalID: "term_8"}},
		{Title: "watch B: completed success", Severity: domain.SeverityAttention},
	}})
	nm := next.(Model)

	notes := noteTexts(nm)
	if len(notes) != 1 {
		t.Fatalf("a multi-event batch must coalesce to ONE note, got %d: %v", len(notes), notes)
	}
	if want := "3 updates — needs input · +2 more (/inbox)"; notes[0] != want {
		t.Errorf("coalesced note = %q, want %q", notes[0], want)
	}
	// The spine tone tracks the MOST SEVERE event in the batch (urgent → NoteError).
	if lv := noteLevels(nm)[0]; lv != NoteError {
		t.Errorf("coalesced spine = %d, want NoteError (max severity)", lv)
	}
	// Coalescing is display-only: every event still feeds the wake queue and badge.
	if len(nm.pendingWake) != 3 || nm.attentionN != 3 {
		t.Errorf("wake queue/badge must carry all events: wake=%d badge=%d", len(nm.pendingWake), nm.attentionN)
	}
}

// TestAttention_CoalescedNoteSeverityTieKeepsFirst: with the batch's max severity tied,
// the FIRST event of that severity is the headline (strict > comparison — no reordering).
func TestAttention_CoalescedNoteSeverityTieKeepsFirst(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "watch A: completed success", Severity: domain.SeverityAttention},
		{Title: "watch B: completed success", Severity: domain.SeverityAttention},
	}})
	notes := noteTexts(next.(Model))
	if len(notes) != 1 || notes[0] != "2 updates — watch A: completed success · +1 more (/inbox)" {
		t.Fatalf("a severity tie must headline the FIRST event, got %v", notes)
	}
}

// TestAttention_IdleMultiEventCoalescedNoteBeforeWakeTurn: the coalesced note obeys the
// same chronological contract as the single-event note — committed BEFORE the wake
// TurnCell the idle drain fires, with the whole burst carried into that wake.
func TestAttention_IdleMultiEventCoalescedNoteBeforeWakeTurn(t *testing.T) {
	m := liveModel(80) // idle: inFlight defaults to false
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "watch A: completed success", Severity: domain.SeverityAttention},
		{Title: "watch B: completed success", Severity: domain.SeverityAttention},
	}})
	nm := next.(Model)
	if len(nm.transcript) < 2 || nm.transcript[0].Note == nil {
		t.Fatalf("idle multi-event path must append the coalesced note first, got %+v", nm.transcript)
	}
	if got := nm.transcript[0].Note.Text; got != "2 updates — watch A: completed success · +1 more (/inbox)" {
		t.Errorf("coalesced note = %q", got)
	}
	foundTurnAfterNote := false
	for _, c := range nm.transcript[1:] {
		if c.Turn != nil {
			foundTurnAfterNote = true
		}
	}
	if !foundTurnAfterNote {
		t.Error("the wake TurnCell must follow the coalesced note")
	}
	if len(nm.activeWake) != 2 {
		t.Errorf("the wake burst must carry BOTH events, got %d", len(nm.activeWake))
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
	if len(notes) != 1 || notes[0] != "both set — [term term_2]" {
		t.Fatalf("terminal must win over worktree in the target suffix: %v", notes)
	}
}

// TestAttention_NoteUnknownSeverityFallsBackToInfo proves the ELSE-1 fallback: a severity
// the cockpit doesn't recognize still renders (neutral info tone), never blank. The glyph
// itself now lives on the spine; here we assert the text and the spine level.
func TestAttention_NoteUnknownSeverityFallsBackToInfo(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "mystery", Severity: domain.Severity("bogus")},
	}})
	nm := next.(Model)
	notes := noteTexts(nm)
	if len(notes) != 1 || notes[0] != "mystery" {
		t.Fatalf("unknown severity must fall back to the info glyph: %v", notes)
	}
	if lv := noteLevels(nm)[0]; lv != NoteInfo {
		t.Errorf("unknown severity spine = %d, want NoteInfo", lv)
	}
}

// TestAttention_NoteNilTargetHasNoSuffix: a targetless event renders just "<Title>"
// with no trailing " — " separator (the glyph is on the spine, not in the text).
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
	if notes[0] != "no target" {
		t.Errorf("note = %q, want %q", notes[0], "no target")
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

// TestAttention_NoteCoalesceCount: a queue-coalesced event (Count > 1) carries the "×N"
// suffix on its single-event note, matching the /inbox digest; Count of 0 or 1 shows
// nothing. (Delivered as two single-event batches — a multi-event batch coalesces to
// one summary line and drops the per-event suffixes.)
func TestAttention_NoteCoalesceCount(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "flapping", Severity: domain.SeverityAttention, Target: &domain.EventTarget{TerminalID: "term_5"}, Count: 3},
	}})
	next, _ = next.(Model).onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "single", Severity: domain.SeverityInfo, Count: 1},
	}})
	notes := noteTexts(next.(Model))
	want := []string{"flapping — [term term_5] (×3)", "single"}
	if len(notes) != len(want) {
		t.Fatalf("got %d notes: %v", len(notes), notes)
	}
	for i, w := range want {
		if notes[i] != w {
			t.Errorf("note[%d] = %q, want %q", i, notes[i], w)
		}
	}
}

// TestAttention_NoteNonNilEmptyTargetHasNoSuffix: a non-nil Target with both ids empty
// must render no target fragment (no "[term ]"/"[wt ]" and no " — " separator).
func TestAttention_NoteNonNilEmptyTargetHasNoSuffix(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "scopeless", Severity: domain.SeverityInfo, Target: &domain.EventTarget{}},
	}})
	notes := noteTexts(next.(Model))
	if len(notes) != 1 || notes[0] != "scopeless" {
		t.Fatalf("a non-nil empty target must render no suffix: %v", notes)
	}
}

// TestAttention_IdleEmitsNoteBeforeWakeTurn covers the production idle path: when nothing
// is in flight, onAttention drains the burst (firing the wake reactor turn). The note must
// land in the transcript BEFORE the wake TurnCell so the ledger reads chronologically.
func TestAttention_IdleEmitsNoteBeforeWakeTurn(t *testing.T) {
	m := liveModel(80) // idle: inFlight defaults to false
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{Title: "needs input", Severity: domain.SeverityUrgent, Target: &domain.EventTarget{TerminalID: "term_8"}},
	}})
	nm := next.(Model)
	if len(nm.transcript) < 2 {
		t.Fatalf("idle path must append a note AND a wake turn, got %d cells", len(nm.transcript))
	}
	if nm.transcript[0].Note == nil {
		t.Fatalf("first cell must be the attention note, got %+v", nm.transcript[0])
	}
	if nm.transcript[0].Note.Text != "needs input — [term term_8]" {
		t.Errorf("note text = %q", nm.transcript[0].Note.Text)
	}
	foundTurnAfterNote := false
	for _, c := range nm.transcript[1:] {
		if c.Turn != nil {
			foundTurnAfterNote = true
		}
	}
	if !foundTurnAfterNote {
		t.Error("the wake TurnCell must follow the note, not precede it")
	}
}

// TestAttention_MaterialReDeliveryEmitsFreshNote locks in the escalation policy: the
// scheduler re-delivers an event id on a material change, and the UI deliberately emits a
// second note (carrying the escalated severity/title) rather than deduping it away.
func TestAttention_MaterialReDeliveryEmitsFreshNote(t *testing.T) {
	m := liveModel(80)
	m.inFlight = true
	tgt := &domain.EventTarget{TerminalID: "term_1"}
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{ID: "evt_1", Title: "waiting", Severity: domain.SeverityAttention, Target: tgt},
	}})
	nm := next.(Model)
	next2, _ := nm.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{
		{ID: "evt_1", Title: "merge conflict", Severity: domain.SeverityBlocked, Target: tgt},
	}})
	notes := noteTexts(next2.(Model))
	want := []string{"waiting — [term term_1]", "merge conflict — [term term_1]"}
	if len(notes) != len(want) {
		t.Fatalf("a material re-delivery must add a fresh note, got %d: %v", len(notes), notes)
	}
	for i, w := range want {
		if notes[i] != w {
			t.Errorf("note[%d] = %q, want %q", i, notes[i], w)
		}
	}
}

// TestAttention_GlyphAndLevelTable pins the full severity → (glyph, spine level) mapping so
// a future addition to domain.Severity can't silently fall through both helpers.
func TestAttention_GlyphAndLevelTable(t *testing.T) {
	gs := darkTheme().Glyphs
	cases := []struct {
		sev   domain.Severity
		glyph string
		level NoteLevel
	}{
		{domain.SeverityDebug, "·", NoteInfo},
		{domain.SeverityInfo, "ℹ", NoteInfo},
		{domain.SeverityDone, "✓", NoteSuccess},
		{domain.SeverityAttention, gs.Attention, NoteWarn}, // theme-aware: "»" unicode / "!" ASCII
		{domain.SeverityBlocked, "⛔", NoteError},
		{domain.SeverityUrgent, "‼", NoteError},
		{domain.SeverityError, "✗", NoteError},
		{domain.Severity("unknown"), "ℹ", NoteInfo},
	}
	for _, c := range cases {
		if g := attentionSeverityGlyph(gs, c.sev); g != c.glyph {
			t.Errorf("glyph(%q) = %q, want %q", c.sev, g, c.glyph)
		}
		if lv := severityToNoteLevel(c.sev); lv != c.level {
			t.Errorf("level(%q) = %d, want %d", c.sev, lv, c.level)
		}
	}
}

// TestRenderNoteCell_AttentionSingleGlyphNoDuplication is the regression guard for the
// old "! !" doubling: an attention NoteCell renders its precise severity glyph ONCE (on
// the spine), with glyph-free text — never the spine glyph plus a duplicate in the text.
func TestRenderNoteCell_AttentionSingleGlyphNoDuplication(t *testing.T) {
	th := darkTheme()
	g := th.Glyphs.Attention
	n := &NoteCell{Level: NoteWarn, Severity: domain.SeverityAttention, Text: "watch done — [term t1]"}
	out := stripAnsi(renderNoteCell(th, n, 120))
	if c := strings.Count(out, g); c != 1 {
		t.Fatalf("expected exactly one attention glyph %q, got %d in %q", g, c, out)
	}
	if strings.Contains(out, g+" "+g) {
		t.Fatalf("attention glyph must not be doubled: %q", out)
	}
	if !strings.Contains(out, "watch done — [term t1]") {
		t.Fatalf("note text missing from render: %q", out)
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
