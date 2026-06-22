package composer

import (
	tea "charm.land/bubbletea/v2"
)

// Outcome is what Update reports back to the parent root model. Exactly one of
// the action flags is meaningful per key; the rest are zero. Editing keys
// produce a zero Outcome (the composer handled it internally).
type Outcome struct {
	// Submit is set when plain Enter accepted a non-empty buffer. The parent runs
	// the turn; on acceptance it calls AcceptSubmit (records history + clears).
	Submit *SubmitResult
	// Cancel signals Esc on an EMPTY buffer while busy — the pull-back / cancel is
	// owned by the root (ui-input.md §1.3, §1.6). The composer does NOT handle
	// cancellation itself; it just reports the gesture UP.
	Cancel bool
}

// AcceptSubmit finalizes a submit the parent accepted: record the prompt into
// history (collapsing duplicates, capping at 200) and clear the buffer
// (ui-input.md §1.7). The parent calls this only when sendUserMessage did NOT
// reject; on rejection it leaves the buffer untouched so the text isn't lost.
func (m *Model) AcceptSubmit(trimmed string) {
	m.recordHistory(trimmed)
	m.Reset()
}

// Update routes one Bubble Tea message to the composer. It handles key presses
// and bracketed paste; everything else is ignored (returns a zero Outcome). The
// parent gives the composer the key FIRST only when it is focused; the app
// chords (Ctrl-C/O/X, off-home Esc) are handled by the root, so any chord that
// is not a defined editing op falls through here without inserting text
// (ui-input.md §1.8, §2).
func (m *Model) Update(msg tea.Msg) Outcome {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		// Bracketed multi-line paste: inserted VERBATIM with \r\n? → \n
		// normalization, NOT run through the per-key chord logic (ui-input.md §1.8).
		m.insert(msg.Content)
		return Outcome{}
	case keyMsg:
		return m.handleKey(msg)
	}
	return Outcome{}
}

// handleKey implements the full keymap. ORDER MATTERS (ui-input.md §1.2): Enter
// is matched before the ctrl/meta editing chords so modifier+Enter combos aren't
// swallowed by a chord branch; cursor motion / history is matched before the
// ctrl/meta passthrough so modified arrows aren't swallowed.
func (m *Model) handleKey(k keyMsg) Outcome {
	mod := k.Mod
	ctrl := mod&tea.ModCtrl != 0
	alt := mod&tea.ModAlt != 0
	shift := mod&tea.ModShift != 0
	super := mod&tea.ModSuper != 0

	// 1) Enter / newline — handled FIRST (ui-input.md §1.2).
	if isEnter(k) {
		// Modifier+Enter (Shift/Alt/Ctrl) inserts a newline when the terminal
		// surfaces the modifier (e.g. kitty keyboard protocol).
		if shift || alt || ctrl {
			m.insert("\n")
			return Outcome{}
		}
		// Trailing-backslash + Enter newline fallback (terminal-independent): if the
		// rune immediately left of the cursor is '\', replace it with '\n' in place.
		rs := runesOf(m.buffer)
		if m.cursor > 0 && m.cursor <= len(rs) && rs[m.cursor-1] == '\\' {
			next := make([]rune, 0, len(rs))
			next = append(next, rs[:m.cursor-1]...)
			next = append(next, '\n')
			next = append(next, rs[m.cursor:]...)
			m.setBuffer(next, m.cursor) // cursor stays in place (now after the \n)
			return Outcome{}
		}
		// Plain Enter submits the RAW, untrimmed buffer; the parent trims and rejects
		// empties. We pre-trim here only to suppress an empty submit (no-op).
		if m.trimEmpty() {
			return Outcome{}
		}
		return Outcome{Submit: &SubmitResult{Text: trim(m.buffer), OK: true}}
	}

	// 2) Escape (ui-input.md §1.3). The composer assigns meaning:
	//    nonempty → clear; empty+busy → report Cancel UP; empty+idle → no-op.
	if k.Code == tea.KeyEscape || k.Code == tea.KeyEsc {
		if !m.trimEmpty() {
			m.Reset()
			return Outcome{}
		}
		if m.busy {
			return Outcome{Cancel: true}
		}
		return Outcome{}
	}

	// 3) Tab — slash-command completion of the TOP match (ui-input.md §1.9). When
	// the palette is non-empty, set the buffer to "<cmd> " (trailing space). No
	// cycle-through-matches. Only when focused & not busy (palette is suppressed
	// otherwise, matching suggestionsFor's gate in the parent).
	if k.Code == tea.KeyTab && mod == 0 {
		if s := m.activeSuggestions(); len(s) > 0 {
			m.buffer = s[0].Name + " "
			m.cursor = m.runeLen()
		}
		return Outcome{}
	}

	// 4) Cursor motion & history recall (ui-input.md §1.4) — before ctrl/meta
	// passthrough so modified arrows / editing chords aren't swallowed.
	switch k.Code {
	case tea.KeyLeft:
		if ctrl || alt {
			m.cursor = prevWord(runesOf(m.buffer), m.cursor)
		} else {
			m.cursor = clampInt(m.cursor-1, 0, m.runeLen())
		}
		return Outcome{}
	case tea.KeyRight:
		if ctrl || alt {
			m.cursor = nextWord(runesOf(m.buffer), m.cursor)
		} else {
			m.cursor = clampInt(m.cursor+1, 0, m.runeLen())
		}
		return Outcome{}
	case tea.KeyHome:
		m.cursor = lineStartOf(runesOf(m.buffer), m.cursor)
		return Outcome{}
	case tea.KeyEnd:
		m.cursor = lineEndOf(runesOf(m.buffer), m.cursor)
		return Outcome{}
	case tea.KeyUp:
		m.moveUp()
		return Outcome{}
	case tea.KeyDown:
		m.moveDown()
		return Outcome{}
	case tea.KeyBackspace:
		m.handleBackspace(super, alt)
		return Outcome{}
	case tea.KeyDelete:
		// Forward delete: remove the rune right of the cursor.
		m.killForwardChar()
		return Outcome{}
	}

	// 5) Ctrl chords (editing). Matched on the base Code with the Ctrl modifier.
	if ctrl && !alt {
		switch k.Code {
		case 'a':
			m.cursor = lineStartOf(runesOf(m.buffer), m.cursor)
			return Outcome{}
		case 'e':
			m.cursor = lineEndOf(runesOf(m.buffer), m.cursor)
			return Outcome{}
		case 'd':
			// Ctrl-D = forward delete a rune (ui-input.md §1.5).
			m.killForwardChar()
			return Outcome{}
		case 'w':
			// Ctrl-W = kill previous word.
			m.killRange(prevWord(runesOf(m.buffer), m.cursor), m.cursor)
			return Outcome{}
		case 'k':
			// Ctrl-K = kill to end of line; AT EOL it eats the '\n' (joins the next
			// line). Ported from MultilineInput.tsx (ui-input.md §1.5).
			rs := runesOf(m.buffer)
			end := lineEndOf(rs, m.cursor)
			to := end
			if end == m.cursor && m.cursor < len(rs) {
				to = m.cursor + 1
			}
			m.killRange(m.cursor, to)
			return Outcome{}
		case 'u':
			// Ctrl-U = kill the whole logical line.
			m.killLine()
			return Outcome{}
		case 'y':
			// Ctrl-Y = yank the single-slot kill-ring verbatim.
			m.insert(m.killRing)
			return Outcome{}
		}
		// Any other Ctrl chord (Ctrl-C/O/X, etc.) is NOT an editing op: leave it for
		// the app-level handlers. Never inserted as text.
		return Outcome{}
	}

	// 6) Alt/Meta chords (Emacs word motion + word kill, ui-input.md §1.4/§1.5).
	if alt && !ctrl {
		switch k.Code {
		case 'b':
			m.cursor = prevWord(runesOf(m.buffer), m.cursor)
			return Outcome{}
		case 'f':
			m.cursor = nextWord(runesOf(m.buffer), m.cursor)
			return Outcome{}
		case 'd':
			// Alt-D = kill next word.
			m.killRange(m.cursor, nextWord(runesOf(m.buffer), m.cursor))
			return Outcome{}
		}
		// Other Alt chords fall through without inserting.
		return Outcome{}
	}

	// 7) Printable input. Insert the glyph at the cursor. KeyPressMsg carries the
	// printable characters in Text; guard with isPrintable as defense so a raw
	// escape sequence can never leak (ui-input.md §1.8).
	if k.Text != "" && isPrintable(k.Text) {
		m.insert(k.Text)
	}
	return Outcome{}
}

// handleBackspace dispatches Backspace by modifier precedence (super → killLine,
// Alt → kill prev word, else plain char delete), matching MultilineInput.tsx
// (ui-input.md §1.5).
func (m *Model) handleBackspace(super, alt bool) {
	switch {
	case super:
		m.killLine()
	case alt:
		m.killRange(prevWord(runesOf(m.buffer), m.cursor), m.cursor)
	default:
		// Plain Backspace: delete the rune left of the cursor (no kill-ring store).
		if m.cursor == 0 {
			return
		}
		rs := runesOf(m.buffer)
		next := make([]rune, 0, len(rs)-1)
		next = append(next, rs[:m.cursor-1]...)
		next = append(next, rs[m.cursor:]...)
		m.setBuffer(next, m.cursor-1)
	}
}

// killForwardChar deletes the rune right of the cursor (Delete / Ctrl-D). Does
// not touch the kill-ring (a single-char forward delete, not a kill).
func (m *Model) killForwardChar() {
	rs := runesOf(m.buffer)
	if m.cursor >= len(rs) {
		return
	}
	next := make([]rune, 0, len(rs)-1)
	next = append(next, rs[:m.cursor]...)
	next = append(next, rs[m.cursor+1:]...)
	m.setBuffer(next, m.cursor)
}

// activeSuggestions returns the palette rows visible right now: only when the
// composer is focused, not busy, and the draft opens with "/" (ui-input.md §1.9).
func (m *Model) activeSuggestions() []Command {
	if !m.focus || m.busy {
		return nil
	}
	return suggestionsFor(m.commands, m.buffer)
}

// isEnter reports whether a key counts as Enter: Return, keypad-Enter, or a bare
// line-feed all submit (ui-input.md §1.2 item 4).
func isEnter(k keyMsg) bool {
	switch k.Code {
	case tea.KeyEnter, tea.KeyKpEnter:
		return true
	}
	// A bare line-feed (\n) sometimes arrives as a control rune.
	return k.Code == '\n' || k.Code == '\r'
}

func trim(s string) string {
	// Local trim that matches strings.TrimSpace but avoids re-import churn.
	start, end := 0, len(s)
	for start < end && isSpaceByte(s[start]) {
		start++
	}
	for end > start && isSpaceByte(s[end-1]) {
		end--
	}
	return s[start:end]
}

func isSpaceByte(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\v', '\f', '\r':
		return true
	}
	return false
}
