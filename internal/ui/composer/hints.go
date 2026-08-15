package composer

import "github.com/charmbracelet/x/ansi"

// hints.go owns the state-dependent composer copy: what the NEXT Escape press will
// actually do, and how the buffered-follow-up cue reads at a given width.
//
// Both used to be spelled out inline at the render site, where they drifted from the
// behavior they describe. The hint row promoted a flat "Esc cancel" whenever a turn was
// cancellable, but Escape only cancels in ONE of the three busy states: with text in the
// buffer it clears the draft, and with a follow-up still buffered it pulls that follow-up
// back for editing. The hint was therefore wrong at exactly the moment a user leans on it.

// EscapeHintMode is what the next Escape press does, derived from the buffer, the busy
// state, and how many follow-ups are still buffered for the running turn. The renderer
// owns the copy; tests assert the MODE, so rewording a label can't silently break the
// state matrix, and a future behavior change can't leave stale parent-authored copy
// behind.
type EscapeHintMode int

const (
	// EscapeHintHidden — idle with an empty buffer: Escape does nothing, so advertising
	// it would promise an action that never happens.
	EscapeHintHidden EscapeHintMode = iota
	// EscapeHintClearDraft — the buffer holds a draft: Escape clears it. This wins over
	// every busy/queue case because handleKey checks it first.
	EscapeHintClearDraft
	// EscapeHintEditFollowup — busy, empty buffer, exactly one follow-up buffered:
	// Escape pulls it back into the composer.
	EscapeHintEditFollowup
	// EscapeHintEditLatest — busy, empty buffer, several buffered: Escape pulls back the
	// NEWEST (the retract is LIFO).
	EscapeHintEditLatest
	// EscapeHintCancelTurn — busy, empty buffer, nothing buffered: Escape cancels the turn.
	EscapeHintCancelTurn
)

// escapeState resolves the Escape hint for one frame. It folds in the two conditions that
// take Escape away from the composer ENTIRELY, so no surface can advertise a composer
// Escape action the composer will not receive:
//   - reverse-i-search (Ctrl+R) — handleSearchKey owns every key, and Escape cancels the
//     search rather than touching the buffer or the turn;
//   - an unfocused composer — an approval sheet is rendered above it and takes the keys,
//     where Escape DECLINES THE TOOL. Advertising "Esc cancel turn" beside a live approval
//     would be the most expensive lie in the cockpit.
//
// Everything downstream (the hint row AND the queued-follow-up cue) derives from this one
// value, so the two rows can never contradict each other.
func (m *Model) escapeState(p ViewParams) EscapeHintMode {
	if m.searching || !m.focus {
		return EscapeHintHidden
	}
	cancelActive := m.busy
	if p.Cancellable != nil {
		cancelActive = *p.Cancellable
	}
	return m.escapeHintMode(cancelActive, p.QueueDepth)
}

// escapeHintMode mirrors the branch order of the real Escape path in ONE place:
// handleKey clears a non-empty buffer first, then reports Cancel up on an empty buffer
// while busy, where the root's onEscWhileBusy retracts a buffered follow-up before it will
// cancel the turn.
//
// cancelActive is the RESOLVED busy state (ViewParams.Cancellable when the parent supplies
// it, else the composer's own flag) — the cockpit drives both from m.inFlight, and the
// resolved value is what the rest of the hint row already keys off.
func (m *Model) escapeHintMode(cancelActive bool, queueDepth int) EscapeHintMode {
	if !m.trimEmpty() {
		return EscapeHintClearDraft
	}
	if !cancelActive {
		return EscapeHintHidden
	}
	switch {
	case queueDepth == 1:
		return EscapeHintEditFollowup
	case queueDepth > 1:
		return EscapeHintEditLatest
	default:
		return EscapeHintCancelTurn
	}
}

// escapeHint turns a mode into its hint-row entry; ok is false for the hidden mode.
// "cancel" is reserved for cancelling actual work — clearing a draft never borrows it,
// or the one word means two different things a keypress apart.
func escapeHint(mode EscapeHintMode) (Hint, bool) {
	switch mode {
	case EscapeHintClearDraft:
		return Hint{Key: "Esc", Action: "clear draft"}, true
	case EscapeHintEditFollowup:
		return Hint{Key: "Esc", Action: "edit follow-up"}, true
	case EscapeHintEditLatest:
		return Hint{Key: "Esc", Action: "edit latest"}, true
	case EscapeHintCancelTurn:
		return Hint{Key: "Esc", Action: "cancel turn"}, true
	}
	return Hint{}, false
}

// queuedFollowupLabel renders the buffered-follow-up cue for a queue depth at a given
// width, or "" when nothing is buffered. It states what the old
// "N queued for next step · Esc edits last" left the reader to infer: the queued item is
// a user FOLLOW-UP, and it belongs to the RUNNING turn, not some separate future one.
//
// It is deliberately INFORMATIONAL ONLY — no key cue. The old line carried its own
// "Esc edits last", which was a second, unconditional source of truth about Escape sitting
// two rows above the hint row. The moment the user typed a second follow-up on top of the
// queued one, the screen said "Esc edits it" and "Esc clear draft" at the same time. The
// hint row is now state-derived and is the single place Escape is described.
//
// Width is absorbed by dropping the qualifier before reaching for an ellipsis: the cue is
// one explicit row in a fixed-height band, so it must never soft-wrap, and the count is
// the part that must survive.
func queuedFollowupLabel(n, width int) string {
	if n <= 0 {
		return ""
	}
	count := itoa(n) + " follow-up"
	if n > 1 {
		count += "s"
	}
	count += " queued"
	if full := count + " for this turn"; ansi.StringWidth(full) <= width {
		return full
	}
	if ansi.StringWidth(count) <= width {
		return count
	}
	return truncateCells(count, width)
}
