package composer

import (
	"sort"
	"strings"
)

// paletteCap bounds the RENDERED slash palette to 5 rows, so a long command
// list never pushes the input off-screen. suggestionsFor never applies that cap
// itself — whatever it returns stays fully navigable; paletteWindow selects the
// visible slice around paletteSel.
const paletteCap = 5

// Command is one palette entry: the slash command name (with leading "/"), a short intent
// description, and the usage Syntax form ("/audit [n]") shown inline for the highlighted row.
// The composer takes the command list as DATA — the caller (root model) passes the registry's
// entries via Model.SetCommands so the palette can't drift from the handlers that accept them.
type Command struct {
	Name   string // e.g. "/clear"
	Desc   string // e.g. "wipe the transcript and host scrollback"
	Syntax string // e.g. "/audit [n]" — the usage form (optional)
}

// suggestionsFor filters + ranks the command list for a draft. A palette is meaningful only
// when the draft starts with "/". It matches on the FIRST whitespace token (so the palette
// stays open while typing arguments) and ranks by a fuzzy hierarchy: exact name > name prefix
// > name subsequence > description substring. "/" alone shows the whole list. The result is
// intentionally NOT capped: keyboard navigation must be able to reach every matching command;
// View applies the five-row display cap with paletteWindow.
//
// The whitespace that ends the command token is a SEMANTIC boundary, not just a split point.
// While the token is still open the draft is a discovery query, so the loose tiers earn their
// keep: "/back" should surface /models too, because its description mentions the backend.
// Once whitespace closes an exact command name the draft is no longer a query — the user has
// committed to that command (Tab writes exactly "<name> ") and wants its usage hint, so a
// command that merely NAMES it in prose is noise. Hence a closed exact name owns the palette
// alone; every other state falls through to the unchanged ranking below.
func suggestionsFor(cmds []Command, value string) []Command {
	if !strings.HasPrefix(value, "/") {
		return nil
	}
	q := strings.ToLower(value[1:])
	// Match on the command token only — arguments after the first space don't filter, so the
	// palette stays visible (with its usage hint) as you type "/audit 5". Keep WHETHER the
	// separator was there: that bit is what distinguishes a committed command from a token
	// still being typed, and dropping it is what let description matches outlive Tab (#359).
	closed := false
	if i := strings.IndexAny(q, " \t"); i >= 0 {
		q = q[:i]
		closed = true
	}

	out := make([]Command, 0, len(cmds))
	if q == "" {
		// "/" — and "/ ", which is closed but names nothing — is still "show me everything".
		out = append(out, cmds...)
		return out
	}

	// A closed EXACT name collapses the palette to that one command. Gating on the separator
	// rather than on exactness alone is deliberate: "/workflow" is a live command AND a strict
	// prefix of "/workflows", so collapsing an open token would yank a reachable command away
	// mid-keystroke. Gating on registry topology instead ("collapse when nobody extends me")
	// would silently change a command's behaviour the day a longer sibling is added.
	if closed {
		for _, c := range cmds {
			if strings.ToLower(strings.TrimPrefix(c.Name, "/")) == q {
				return append(out, c)
			}
		}
		// No such command ("/inb urgent", "/nope ") — the user is still searching, so the
		// fuzzy tiers below stay in play and Enter can still complete "/inb" to "/inbox".
	}

	type scored struct {
		c     Command
		score int
		idx   int // registry order, the stable tiebreak
	}
	var matches []scored
	for i, c := range cmds {
		if s := fuzzyScore(strings.ToLower(strings.TrimPrefix(c.Name, "/")), q, strings.ToLower(c.Desc)); s > 0 {
			matches = append(matches, scored{c, s, i})
		}
	}
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].idx < matches[j].idx
	})
	for _, m := range matches {
		out = append(out, m.c)
	}
	return out
}

// paletteWindow returns the at-most-five rows that should be rendered for the current
// selection, plus the selection's LOCAL index within that slice. The window follows the
// highlight once it moves beyond the first page, keeping every reachable match visible.
// Navigation itself still wraps across the full suggestion list.
func paletteWindow(suggestions []Command, selected int) ([]Command, int) {
	if len(suggestions) == 0 {
		return nil, 0
	}
	selected = clampInt(selected, 0, len(suggestions)-1)
	if len(suggestions) <= paletteCap {
		return suggestions, selected
	}
	start := selected - paletteCap + 1
	if start < 0 {
		start = 0
	}
	maxStart := len(suggestions) - paletteCap
	if start > maxStart {
		start = maxStart
	}
	return suggestions[start : start+paletteCap], selected - start
}

// fuzzyScore ranks a command for query q (all lower-case): exact name (1000) > name prefix
// (500) > name subsequence (200) > description substring (50); 0 means no match.
func fuzzyScore(name, q, desc string) int {
	switch {
	case name == q:
		return 1000
	case strings.HasPrefix(name, q):
		return 500
	case subsequence(name, q):
		return 200
	case strings.Contains(desc, q):
		return 50
	}
	return 0
}

// subsequence reports whether q's runes appear in s in order (not necessarily adjacent) —
// the fuzzy-match test used by VS Code / fzf-style palettes (e.g. "tgl" matches "toggle").
func subsequence(s, q string) bool {
	rs := []rune(s)
	si := 0
	for _, qr := range q {
		for si < len(rs) && rs[si] != qr {
			si++
		}
		if si == len(rs) {
			return false
		}
		si++
	}
	return true
}

// paletteWrap returns i wrapped into [0, n) (modulo navigation that never gets stuck).
func paletteWrap(i, n int) int {
	if n <= 0 {
		return 0
	}
	return ((i % n) + n) % n
}
