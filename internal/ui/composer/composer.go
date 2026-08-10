package composer

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/ui/theme"
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
	// the FULL in-progress editing state (typed text, cursor, and every inline paste
	// token with its stash) so ↓ past the newest entry restores the draft EXACTLY —
	// not a single re-stashed blob — even when it mixes typed text with pastes.
	history   []string
	histIndex int
	draft     draftState

	// killRing is a SINGLE slot (the last killed text), not an Emacs ring. Ctrl-Y
	// inserts it verbatim with no rotation.
	killRing string

	// pastes holds every stashed LARGE bracketed paste in insertion order. Each has a
	// unique single-line token (largePasteToken, no '\n') embedded INLINE in m.buffer,
	// so the buffer is the user's own typed text with those tokens interspersed:
	// typed content and one or more pasted blocks coexist, and the composer stays
	// short (each token is a single row) no matter how big the pastes are. The submit
	// and history paths EXPAND the buffer, substituting every surviving token with its
	// real text (expandPastes), so the placeholder string is never what gets sent.
	// INVARIANT: every element's token is a substring of m.buffer. reconcilePastes,
	// run after each buffer mutation, drops any stash whose token the user edited
	// apart — self-healing: a damaged token becomes literal text and is sent verbatim,
	// an untouched one keeps standing in for its paste.
	pastes []pasteStash
	// pasteSeq is a monotonic per-composer counter that makes each token unique even
	// when two pastes share the same summary ("[pasted 8 lines #1]" vs "#2"). It is
	// reset only when the buffer is fully REPLACED (Reset / recall / Restore), never
	// on a mid-buffer edit, so a surviving token's id can never collide with a later
	// paste's.
	pasteSeq int

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
	searchPrevPastes []pasteStash // pastes snapshot, restored if the search is cancelled
	searchPrevSeq    int          // pasteSeq snapshot, restored alongside searchPrevPastes

	// lastWidth is the content width the composer last rendered at (set in View, read by the
	// visual-row-aware vertical motion in Update so ↑/↓ follow soft-wrapped rows).
	lastWidth int

	keys  keymap
	theme theme.Theme
}

// pasteStash pairs a placeholder token (exactly as it appears inline in the
// buffer) with the real pasted text it stands for. Submit expands the buffer by
// substituting each surviving token with its text.
type pasteStash struct {
	token string
	text  string
}

// draftState is the full editable snapshot stashed when history recall begins, so
// stepping ↓ back past the newest entry restores the in-progress draft EXACTLY —
// typed text, cursor, and every inline paste token with its stash — rather than
// collapsing a mixed typed+paste draft into a single re-stashed blob.
type draftState struct {
	buffer string
	cursor int
	pastes []pasteStash
	seq    int
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
	m.draft = draftState{}
	m.pastes = nil
	m.pasteSeq = 0
}

// Restore is the ONE sanctioned parent→composer write: the
// pull-back pushes the original text back into the buffer for editing, cursor
// parked at end. Modeled as a direct method (the parent owns the composer as an
// embedded struct); a RestoreDraftMsg in Update is the alternative seam.
func (m *Model) Restore(text string) {
	norm := normalizeNewlines(text)
	// A pull-back REPLACES the whole buffer, so clear any prior content + stashes
	// first (also resets the token counter so the re-stash starts at #1).
	m.clearBuffer()
	if isLargePaste(norm) {
		// A pulled-back large message re-stashes behind a single placeholder, otherwise
		// it would re-grow the composer past the terminal height — the original bug.
		m.stashLargePaste(norm)
	} else {
		m.buffer = norm
		m.cursor = len([]rune(m.buffer))
	}
	m.histIndex = -1
	m.draft = draftState{}
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

// liveText is the real editable content: the buffer with every stashed paste's
// token substituted back for its full text. The draft snapshot and submit use it so
// pastes survive history navigation and are sent in full — the placeholder strings
// are never treated as content.
func (m *Model) liveText() string { return m.expandPastes(m.buffer) }

// submitText is the trimmed text a submit / history-record should use: the buffer
// with every stashed paste expanded, never the placeholder tokens.
func (m *Model) submitText() string { return trim(m.liveText()) }

// stashLargePaste registers a large (already-normalized) paste and INSERTS its
// one-line token into the buffer at the cursor, so a paste lands like typed text:
// surrounding content is preserved and several pastes coexist, each behind its own
// token. The real text is substituted back only on submit (expandPastes). Insertion
// goes through insert()→setBuffer→reconcilePastes; because the stash is appended
// FIRST, reconcile sees its freshly-inserted token and keeps it.
func (m *Model) stashLargePaste(normalized string) {
	token := m.newPasteToken(normalized)
	m.pastes = append(m.pastes, pasteStash{token: token, text: normalized})
	m.insert(token)
}

// newPasteToken builds the next unique inline token for a stash: the human summary
// ("8 lines" / "640 chars") plus a monotonic "#n" so two same-size pastes stay
// distinct.
func (m *Model) newPasteToken(normalized string) string {
	m.pasteSeq++
	return largePasteToken(normalized, m.pasteSeq)
}

// clearBuffer empties the buffer and drops every stash — the reset the full-buffer
// REPLACE paths (recall / Restore) run before laying down fresh content, so a new
// paste's token id starts from #1 and no stale token survives the replace.
func (m *Model) clearBuffer() {
	m.buffer = ""
	m.cursor = 0
	m.pastes = nil
	m.pasteSeq = 0
}

// expandPastes returns s with every stash token replaced by its real pasted text.
// It scans s ONCE left-to-right: a matched token is emitted as its real text and the
// scan resumes AFTER the token, so real text that happens to contain another token
// string is never re-expanded. Tokens are ASCII and end in ']', so no token is a
// prefix of another ("#1]" vs "#12]") and the first prefix match is unambiguous.
// With no stashes it returns s unchanged.
func (m *Model) expandPastes(s string) string {
	if len(m.pastes) == 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		matched := false
		for _, p := range m.pastes {
			if strings.HasPrefix(s[i:], p.token) {
				b.WriteString(p.text)
				i += len(p.token)
				matched = true
				break
			}
		}
		if !matched {
			b.WriteByte(s[i])
			i++
		}
	}
	return b.String()
}

// reconcilePastes drops any stash whose token is no longer present intact in the
// buffer — the self-heal that runs after every buffer mutation (setBuffer). Editing
// into a token breaks it apart, so it stops being a substring; that paste then
// reverts to whatever literal text the edit left behind and is sent verbatim, while
// untouched tokens keep their stashes. Cheap: the composer holds a handful of pastes.
func (m *Model) reconcilePastes() {
	if len(m.pastes) == 0 {
		return
	}
	kept := make([]pasteStash, 0, len(m.pastes))
	for _, p := range m.pastes {
		if strings.Contains(m.buffer, p.token) {
			kept = append(kept, p)
		}
	}
	if len(kept) == 0 {
		m.pastes = nil
		return
	}
	m.pastes = kept
}

// clonePastes returns an independent copy of a pastes slice for the search snapshot,
// so a recall that reassigns m.pastes can't disturb the saved-for-cancel state.
func clonePastes(ps []pasteStash) []pasteStash {
	if len(ps) == 0 {
		return nil
	}
	out := make([]pasteStash, len(ps))
	copy(out, ps)
	return out
}

// snapshotDraft captures the whole editable state (buffer, cursor, stashes, seq) so
// history recall can restore the in-progress draft verbatim later.
func (m *Model) snapshotDraft() draftState {
	return draftState{
		buffer: m.buffer,
		cursor: m.cursor,
		pastes: clonePastes(m.pastes),
		seq:    m.pasteSeq,
	}
}

// restoreDraft puts a snapshotted draft back verbatim (the ↓-past-newest path). The
// buffer already holds one-line tokens, not expanded pastes, so the composer stays
// short — no re-stash and no re-grow.
func (m *Model) restoreDraft(d draftState) {
	m.buffer = d.buffer
	m.pastes = clonePastes(d.pastes)
	m.pasteSeq = d.seq
	m.cursor = clampInt(d.cursor, 0, m.runeLen())
}

// --- low-level buffer mutation (all in rune space) ---

// runeLen returns the buffer length in runes.
func (m *Model) runeLen() int { return len([]rune(m.buffer)) }

// setBuffer replaces the buffer from a rune slice and clamps the cursor. It is the
// choke point for every edit, so it reconciles the stashes: a token the edit broke
// apart is dropped (its text becomes the literal, authored content), while intact
// tokens keep standing in for their pastes. This is what makes paste stashes
// self-healing and per-paste rather than all-or-nothing.
func (m *Model) setBuffer(rs []rune, cursor int) {
	m.buffer = string(rs)
	m.cursor = clampInt(cursor, 0, len(rs))
	m.reconcilePastes()
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
// A recalled large entry re-stashes behind a single placeholder so navigating
// history never re-grows the composer past the terminal height. It clears the buffer
// + stashes first because recall is a full REPLACE, not an edit.
func (m *Model) recall(text string) {
	m.clearBuffer()
	if isLargePaste(text) {
		m.stashLargePaste(text)
		return
	}
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
	m.searchPrevPastes = clonePastes(m.pastes)
	m.searchPrevSeq = m.pasteSeq
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
		// Snapshot the FULL editing state (typed text + every inline token + cursor) so
		// an ↑/↓ round-trip back to the draft restores it exactly — mixed typed+paste
		// drafts keep their structure instead of collapsing to one re-stashed blob.
		m.draft = m.snapshotDraft()
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
		// Stepped past the newest entry: clear recall and restore the draft verbatim.
		m.histIndex = -1
		m.restoreDraft(m.draft)
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
