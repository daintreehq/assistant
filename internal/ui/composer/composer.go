package composer

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// historyLimit caps the recallable prompt history kept for ↑/↓ during a session
// History is session-scoped and never persisted.
const historyLimit = 200

// Model is the dedicated composer editor. The buffer is a single flat string
// with embedded '\n'; the cursor is a flat RUNE offset clamped to [0, len] on
// every mutation. History, the single-slot kill-ring, and the slash palette all
// live here — the parent never reaches in except via the one sanctioned
// Restore(text) path (the pull-back).
type Model struct {
	buffer string
	cursor int // flat rune offset, always clamped to [0, runeLen(buffer)]

	// Session prompt history (oldest first) for ↑/↓ recall. histIndex == -1 means
	// "editing the live draft"; otherwise it is an index into history. draft holds
	// the in-progress text so ↓ past the newest entry restores it.
	history   []string
	histIndex int
	draft     string

	// killRing is a SINGLE slot (the last killed text), not an Emacs ring. Ctrl-Y
	// inserts it verbatim with no rotation.
	killRing string

	// pasteText holds the raw content of a LARGE bracketed paste while m.buffer
	// shows only a one-line placeholder (largePastePlaceholder). It is the real
	// text the submit + history paths use. INVARIANT: pasteText != "" implies
	// m.buffer is that single-line placeholder (no '\n'), so the composer stays one
	// row tall and never trips the "terminal too small" fallback. Every buffer EDIT
	// dissolves the stash by clearing pasteText (setBuffer does this for all
	// mutation paths), so it is self-healing: edit the placeholder and it becomes
	// ordinary text; submit it and the real paste is sent in full.
	pasteText string

	// commands is the slash-palette source, injected as data.
	commands []Command

	// busy/focus drive palette visibility and the busy cue; they do NOT gate
	// editing — the composer stays editable while a turn runs.
	busy  bool
	focus bool

	// paletteSel is the highlighted slash-suggestion row (index into the active
	// suggestions); ↑/↓ and Tab/Shift-Tab move it, Tab/Enter accepts it.
	paletteSel int

	// reverse-i-search (Ctrl-R): while searching, typed runes filter prompt history and the
	// buffer shows the current match. searchPrev* restore the draft if the search is cancelled.
	searching        bool
	searchQuery      string
	searchHit        int // index into history of the current match, -1 = none
	searchPrevBuf    string
	searchPrevCursor int
	searchPrevPaste  string // pasteText snapshot, restored if the search is cancelled

	// lastWidth is the content width the composer last rendered at (set in View, read by the
	// visual-row-aware vertical motion in Update so ↑/↓ follow soft-wrapped rows).
	lastWidth int

	keys  keymap
	theme theme.Theme
}

// New builds an empty composer. The caller wires the command list (SetCommands),
// theme, and per-frame focus/busy state.
func New(th theme.Theme) Model {
	return Model{
		histIndex: -1,
		focus:     true,
		keys:      defaultKeymap(),
		theme:     th,
	}
}

// SetCommands injects the slash-palette command list (the registry's entries).
// The composer treats it as immutable data; pass a fresh slice to update.
func (m *Model) SetCommands(cmds []Command) { m.commands = cmds }

// SetTheme swaps the active theme (e.g. on a runtime theme change).
func (m *Model) SetTheme(th theme.Theme) { m.theme = th }

// SetFocus controls whether the composer accepts editing keys. The parent sets
// it to (view==home && pendingConfirm==nil) — crucially NOT gated on busy.
func (m *Model) SetFocus(f bool) { m.focus = f }

// SetBusy updates the busy flag (drives the busy cue and palette suppression).
func (m *Model) SetBusy(b bool) { m.busy = b }

// SetWidth records the content width the composer renders at, so visual-row vertical motion
// (↑/↓) follows soft-wrapped rows. The parent pushes it each reduction (syncComposer).
func (m *Model) SetWidth(w int) { m.lastWidth = w }

// Focused reports whether the composer currently owns keys.
func (m *Model) Focused() bool { return m.focus }

// Value returns the raw, untrimmed buffer.
func (m *Model) Value() string { return m.buffer }

// Cursor returns the current flat rune offset (for the renderer to place the
// caret by cell).
func (m *Model) Cursor() int { return m.cursor }

// Reset clears the buffer and cursor (e.g. after a submit) and leaves history
// navigation. It does NOT touch the history list.
func (m *Model) Reset() {
	m.buffer = ""
	m.cursor = 0
	m.histIndex = -1
	m.draft = ""
	m.pasteText = ""
}

// Restore is the ONE sanctioned parent→composer write: the
// pull-back pushes the original text back into the buffer for editing, cursor
// parked at end. Modeled as a direct method (the parent owns the composer as an
// embedded struct); a RestoreDraftMsg in Update is the alternative seam.
func (m *Model) Restore(text string) {
	norm := normalizeNewlines(text)
	if isLargePaste(norm) {
		// A pulled-back large message re-stashes behind its placeholder, otherwise
		// it would re-grow the composer past the terminal height — the original bug.
		m.stashLargePaste(norm)
	} else {
		m.pasteText = ""
		m.buffer = norm
		m.cursor = len([]rune(m.buffer))
	}
	m.histIndex = -1
	m.draft = ""
}

// recordHistory appends an accepted prompt for recall, collapsing an immediate
// duplicate and capping at historyLimit (keep the newest 200).
func (m *Model) recordHistory(trimmed string) {
	if n := len(m.history); n > 0 && m.history[n-1] == trimmed {
		return
	}
	m.history = append(m.history, trimmed)
	if len(m.history) > historyLimit {
		m.history = m.history[len(m.history)-historyLimit:]
	}
}

// liveText is the real editable content: the stashed large paste when active,
// otherwise the raw buffer. The draft snapshot and submit use it so a large paste
// survives history navigation and is sent in full — the placeholder string is
// never treated as content.
func (m *Model) liveText() string {
	if m.pasteText != "" {
		return m.pasteText
	}
	return m.buffer
}

// submitText is the trimmed text a submit / history-record should use: the real
// paste when one is stashed, never the placeholder.
func (m *Model) submitText() string { return trim(m.liveText()) }

// stashLargePaste parks the real (already-normalized) text in pasteText and shows
// its one-line placeholder in the buffer, cursor at end. It assigns the buffer
// DIRECTLY rather than via setBuffer, which would clear the stash, so the
// pasteText/placeholder invariant holds.
func (m *Model) stashLargePaste(normalized string) {
	m.pasteText = normalized
	m.buffer = largePastePlaceholder(normalized)
	m.cursor = m.runeLen()
}

// --- low-level buffer mutation (all in rune space) ---

// runeLen returns the buffer length in runes.
func (m *Model) runeLen() int { return len([]rune(m.buffer)) }

// setBuffer replaces the buffer from a rune slice and clamps the cursor.
func (m *Model) setBuffer(rs []rune, cursor int) {
	// Any buffer mutation dissolves an active large-paste placeholder: the text is
	// now authored, not a stand-in, so the stash must not shadow it on submit.
	m.pasteText = ""
	m.buffer = string(rs)
	m.cursor = clampInt(cursor, 0, len(rs))
}

// insert places text at the cursor (normalizing newlines) and advances past it.
func (m *Model) insert(text string) {
	text = normalizeNewlines(text)
	if text == "" {
		return
	}
	m.paletteSel = 0 // typing re-filters the palette → reset the highlight to the top match
	rs := runesOf(m.buffer)
	ins := runesOf(text)
	cur := clampInt(m.cursor, 0, len(rs))
	next := make([]rune, 0, len(rs)+len(ins))
	next = append(next, rs[:cur]...)
	next = append(next, ins...)
	next = append(next, rs[cur:]...)
	m.setBuffer(next, cur+len(ins))
}

// killRange normalizes order, no-ops if empty, stores the removed slice into the
// single-slot kill-ring, removes it, and parks the cursor at the lower bound.
func (m *Model) killRange(from, to int) {
	rs := runesOf(m.buffer)
	a, b := from, to
	if a > b {
		a, b = b, a
	}
	a = clampInt(a, 0, len(rs))
	b = clampInt(b, 0, len(rs))
	if a == b {
		return
	}
	m.killRing = string(rs[a:b])
	next := make([]rune, 0, len(rs)-(b-a))
	next = append(next, rs[:a]...)
	next = append(next, rs[b:]...)
	m.setBuffer(next, a)
}

// killLine deletes the entire logical line the cursor sits on (Ctrl-U).
func (m *Model) killLine() {
	rs := runesOf(m.buffer)
	m.killRange(lineStartOf(rs, m.cursor), lineEndOf(rs, m.cursor))
}

// recall replaces the whole buffer (history recall) and parks the cursor at end.
// A recalled large entry re-stashes behind its placeholder so navigating history
// never re-grows the composer past the terminal height.
func (m *Model) recall(text string) {
	if isLargePaste(text) {
		m.stashLargePaste(text)
		return
	}
	m.pasteText = ""
	m.buffer = text
	m.cursor = m.runeLen()
}

// --- reverse-i-search (Ctrl-R) ---

// startSearch enters reverse history search; a no-op with no history.
func (m *Model) startSearch() {
	if len(m.history) == 0 {
		return
	}
	m.searching = true
	m.searchQuery = ""
	m.searchHit = -1
	m.searchPrevBuf = m.buffer
	m.searchPrevCursor = m.cursor
	m.searchPrevPaste = m.pasteText
}

// endSearch leaves search mode, keeping whatever is currently in the buffer (the match).
func (m *Model) endSearch() {
	m.searching = false
	m.searchQuery = ""
	m.searchHit = -1
	m.histIndex = -1
}

// reverseSearch returns the index of the most recent history entry strictly OLDER than
// `before` whose text contains query (case-insensitive), or -1. before == len(history)
// searches all of it; passing the current hit advances to the next older match.
func (m *Model) reverseSearch(query string, before int) int {
	if strings.TrimSpace(query) == "" {
		return -1
	}
	q := strings.ToLower(query)
	if before < 0 || before > len(m.history) {
		before = len(m.history)
	}
	for i := before - 1; i >= 0; i-- {
		if strings.Contains(strings.ToLower(m.history[i]), q) {
			return i
		}
	}
	return -1
}

// applySearch re-runs the search from the newest entry after the query changed, recalling
// the match into the buffer (leaving the buffer as-is on a failed search).
func (m *Model) applySearch() {
	hit := m.reverseSearch(m.searchQuery, len(m.history))
	m.searchHit = hit
	if hit >= 0 {
		m.recall(m.history[hit])
	}
}

// --- vertical motion + history recall ---

// moveUp moves up a logical line keeping the column; on the TOP line it walks
// backward through prompt history.
func (m *Model) moveUp() {
	rs := runesOf(m.buffer)
	rows := buildVisualRows(rs, m.wrapWidth())
	vr, vc := cursorVisual(rows, m.cursor)
	if vr > 0 {
		// Move by VISUAL row (soft-wrapped), not logical line, so ↑ on a wrapped paragraph
		// steps up one screen row instead of jumping straight into history recall.
		m.cursor = offsetAtVisual(rows, vr-1, vc)
		return
	}
	if len(m.history) == 0 {
		m.cursor = 0
		return
	}
	if m.histIndex < 0 {
		// Snapshot the REAL text (a stashed paste, not its placeholder) so an ↑/↓
		// round-trip back to the draft restores the full paste, not the summary.
		m.draft = m.liveText()
	}
	var idx int
	if m.histIndex < 0 {
		idx = len(m.history) - 1
	} else {
		idx = maxInt(0, m.histIndex-1)
	}
	m.histIndex = idx
	m.recall(m.history[idx])
}

// moveDown moves down a logical line keeping the column; on the BOTTOM line it
// walks forward through history, restoring the live draft past the newest entry.
func (m *Model) moveDown() {
	rs := runesOf(m.buffer)
	rows := buildVisualRows(rs, m.wrapWidth())
	vr, vc := cursorVisual(rows, m.cursor)
	if vr < len(rows)-1 {
		m.cursor = offsetAtVisual(rows, vr+1, vc)
		return
	}
	// On the bottom line: forward history.
	if m.histIndex < 0 {
		m.cursor = len(rs) // no recall in progress: just go to EOF.
		return
	}
	if m.histIndex >= len(m.history)-1 {
		// Stepped past the newest entry: clear recall and restore the draft.
		m.histIndex = -1
		m.recall(m.draft)
		return
	}
	m.histIndex++
	m.recall(m.history[m.histIndex])
}

// SubmitResult is what Update returns to the parent when a plain Enter submits.
type SubmitResult struct {
	// Text is the trimmed buffer (empty submits are suppressed before this fires).
	Text string
	// OK is true when a non-empty submit occurred.
	OK bool
}

// trimEmpty reports whether the buffer is whitespace-only (for Esc semantics: a
// stray space must not swallow the cancel gesture).
func (m *Model) trimEmpty() bool { return strings.TrimSpace(m.buffer) == "" }

// keyMsg is the only key event shape we handle (a key press). v2 also delivers
// release events; we ignore those for editing.
type keyMsg = tea.KeyPressMsg
