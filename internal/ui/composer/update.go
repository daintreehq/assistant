package composer

import (
	"strings"

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
	// owned by the root. The composer does NOT handle
	// cancellation itself; it just reports the gesture UP.
	Cancel bool
}

// AcceptSubmit finalizes a submit the parent accepted: record the prompt into
// history (collapsing duplicates, capping at 200) and clear the buffer
// the buffer. The parent calls this only when sendUserMessage did NOT
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
// without inserting text.
func (m *Model) Update(msg tea.Msg) Outcome {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		// Bracketed paste arrives whole (the terminal does the chunking). A SMALL
		// paste inserts VERBATIM at the cursor with \r\n? → \n normalization, NOT run
		// through the per-key chord logic. A LARGE paste would grow the composer past
		// the terminal height and collapse the bottom band to the one-line "terminal
		// too small" fallback (internal/ui/view.go), so instead we stash the real text
		// and INSERT a one-line placeholder token at the cursor; the real text is
		// substituted back on submit. Inserting (rather than replacing the buffer) is
		// what lets a paste coexist with whatever the user already typed and with other
		// pastes — each gets its own token. A whitespace-only paste is never stashed:
		// submitText() would trim it to "" and a stashed placeholder would then silently
		// swallow Enter, stranding the user. Insert it verbatim instead (the pre-existing
		// whitespace semantics).
		if norm := normalizeNewlines(msg.Content); isLargePaste(norm) && strings.TrimSpace(norm) != "" {
			m.stashLargePaste(norm)
		} else {
			m.insert(msg.Content)
		}
		return Outcome{}
	case keyMsg:
		return m.handleKey(msg)
	}
	return Outcome{}
}

// handleKey implements the full keymap. ORDER MATTERS: Enter
// is matched before the ctrl/meta editing chords so modifier+Enter combos aren't
// swallowed by a chord branch; cursor motion / history is matched before the
// ctrl/meta passthrough so modified arrows aren't swallowed.
func (m *Model) handleKey(k keyMsg) Outcome {
	mod := k.Mod
	ctrl := mod&tea.ModCtrl != 0
	alt := mod&tea.ModAlt != 0
	shift := mod&tea.ModShift != 0
	super := mod&tea.ModSuper != 0

	// 0) Reverse-i-search (Ctrl-R) owns ALL keys while active.
	if m.searching {
		return m.handleSearchKey(k)
	}

	// 1) Enter / newline — handled FIRST.
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
		// Slash palette open → plain Enter ACCEPTS the highlighted row, then submits it
		// (the "Enter run" hint). Without this a partial like "/cl" would submit RAW and
		// the registry would reject it as unknown even though "/clear" is highlighted
		// right above the input. We rewrite only the command token and keep any args the
		// user already typed ("/aud 5" → "/audit 5"), so accepting never drops arguments.
		if sugg := m.activeSuggestions(); len(sugg) > 0 {
			sel := clampInt(m.paletteSel, 0, len(sugg)-1)
			m.acceptSuggestion(sugg[sel].Name)
		}
		// Plain Enter submits the trimmed text; the parent rejects empties too. We
		// suppress an empty submit here as a no-op. submitText() substitutes a stashed
		// large paste back in, so the placeholder string is never what gets sent.
		text := m.submitText()
		if text == "" {
			return Outcome{}
		}
		return Outcome{Submit: &SubmitResult{Text: text, OK: true}}
	}

	// 2) Escape. The composer assigns meaning:
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

	// 3) Slash-palette navigation. While the palette is open, ↑/↓ and
	// Shift-Tab move the highlighted row and Tab ACCEPTS it (fills "<cmd> "); when the palette
	// is closed these keys fall through to history/line motion below.
	if sugg := m.activeSuggestions(); len(sugg) > 0 {
		switch {
		case k.Code == tea.KeyUp || (k.Code == tea.KeyTab && shift):
			m.paletteSel = paletteWrap(m.paletteSel-1, len(sugg))
			return Outcome{}
		case k.Code == tea.KeyDown:
			m.paletteSel = paletteWrap(m.paletteSel+1, len(sugg))
			return Outcome{}
		case k.Code == tea.KeyTab && mod == 0:
			// Tab completes the highlighted command and parks a trailing space so the
			// user can keep typing args; it intentionally REPLACES the whole buffer.
			// Route through setBuffer so any active paste stash is cleared too.
			sel := clampInt(m.paletteSel, 0, len(sugg)-1)
			rs := runesOf(sugg[sel].Name + " ")
			m.setBuffer(rs, len(rs))
			m.paletteSel = 0
			return Outcome{}
		}
	}
	// A Tab with no open palette is consumed (never inserts a literal tab).
	if k.Code == tea.KeyTab && mod == 0 {
		return Outcome{}
	}

	// 4) Cursor motion & history recall — before ctrl/meta
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
			// Ctrl-D = forward delete a rune.
			m.killForwardChar()
			return Outcome{}
		case 'w':
			// Ctrl-W = kill the previous WORD (whitespace-delimited / unix-word-rubout), so a
			// single kill wipes a whole path or URL back to the preceding space.
			m.killRange(prevBigWord(runesOf(m.buffer), m.cursor), m.cursor)
			return Outcome{}
		case 'r':
			// Ctrl-R = reverse-i-search through prompt history.
			m.startSearch()
			return Outcome{}
		case 'k':
			// Ctrl-K = kill to end of line; AT EOL it eats the '\n' (joins the next
			// line).
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

	// 6) Alt/Meta chords (Emacs word motion + word kill).
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
	// escape sequence can never leak.
	if k.Text != "" && isPrintable(k.Text) {
		m.insert(k.Text)
	}
	return Outcome{}
}

// handleSearchKey drives reverse-i-search (Ctrl-R) while active: typed runes filter prompt
// history (newest-first), Ctrl-R steps to the next older match, Backspace edits the query,
// Esc cancels (restoring the pre-search draft), Enter accepts the match and submits it, and
// any other key exits search KEEPING the match and is then processed normally.
func (m *Model) handleSearchKey(k keyMsg) Outcome {
	switch {
	case k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		m.buffer = m.searchPrevBuf
		m.cursor = clampInt(m.searchPrevCursor, 0, m.runeLen())
		m.pastes = clonePastes(m.searchPrevPastes) // keep buffer tokens and stashes in sync
		m.pasteSeq = m.searchPrevSeq
		m.endSearch()
		return Outcome{}
	case isEnter(k):
		m.endSearch()
		text := m.submitText()
		if text == "" {
			return Outcome{}
		}
		return Outcome{Submit: &SubmitResult{Text: text, OK: true}}
	case isCtrlRune(k, 'r'):
		if next := m.reverseSearch(m.searchQuery, m.searchHit); next >= 0 {
			m.searchHit = next
			m.recall(m.history[next])
		}
		return Outcome{}
	case k.Code == tea.KeyBackspace:
		if r := []rune(m.searchQuery); len(r) > 0 {
			m.searchQuery = string(r[:len(r)-1])
			m.applySearch()
		}
		return Outcome{}
	}
	if k.Text != "" && isPrintable(k.Text) {
		m.searchQuery += k.Text
		m.applySearch()
		return Outcome{}
	}
	// Any other key (arrows, ctrl chords) leaves search keeping the match, then re-routes.
	m.endSearch()
	return m.handleKey(k)
}

// isCtrlRune reports whether k is Ctrl+<r> (lowercase rune), tolerating the upper-case and
// control-code encodings terminals use.
func isCtrlRune(k keyMsg, r rune) bool {
	return k.Mod&tea.ModCtrl != 0 && (k.Code == r || k.Code == r-32)
}

// handleBackspace dispatches Backspace by modifier precedence (super → killLine,
// Alt → kill prev word, else plain char delete).
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

// acceptSuggestion rewrites the buffer's leading command token with the chosen
// command name, PRESERVING anything the user already typed after the first
// whitespace (the args). This is the Enter-to-run accept: "/cl" → "/clear",
// "/aud 5" → "/audit 5". Unlike Tab it adds no trailing space, since the next
// step is an immediate submit. The token boundary matches suggestionsFor's
// (space or tab) so the part we keep is exactly the part the palette ignored
// when filtering.
func (m *Model) acceptSuggestion(name string) {
	rest := ""
	if i := strings.IndexAny(m.buffer, " \t"); i >= 0 {
		rest = m.buffer[i:]
	}
	// Route through setBuffer so any active paste stash is cleared too.
	rs := runesOf(name + rest)
	m.setBuffer(rs, len(rs))
	m.paletteSel = 0
}

// activeSuggestions returns the palette rows visible right now: only when the
// composer is focused, not busy, not in reverse-i-search, and the draft opens
// with "/". Suppressing it while searching matters because handleSearchKey owns
// ALL keys then (section 0) — ↑/↓/Tab/Enter drive the search, not the palette —
// so a palette claiming "Enter run" over a "/"-prefixed match would be a lie.
func (m *Model) activeSuggestions() []Command {
	if !m.focus || m.busy || m.searching {
		return nil
	}
	return suggestionsFor(m.commands, m.buffer)
}

// isEnter reports whether a key counts as Enter: Return, keypad-Enter, or a bare
// line-feed all submit.
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
