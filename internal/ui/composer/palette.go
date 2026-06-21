package composer

import "strings"

// paletteCap bounds the slash palette to 5 rows (ui-input.md §1.9), so a long
// command list never pushes the input off-screen.
const paletteCap = 5

// Command is one palette entry: the slash command name (with leading "/") and a
// short intent description. The composer takes the command list as DATA — the
// caller (root model) passes the registry's entries in via Model.SetCommands so
// the palette can't drift from the handlers that actually accept them (the Go
// analog of `paletteEntries()` being the single source of truth, ui-input.md
// §1.9). The composer never hardcodes commands.
type Command struct {
	Name string // e.g. "/clear"
	Desc string // e.g. "wipe the transcript and host scrollback"
}

// suggestionsFor filters the command list for a draft. A palette is meaningful
// only when the draft starts with "/". Filter matches when the command name
// (sans leading "/") has the query as a PREFIX, OR the description CONTAINS the
// query (case-insensitive). Capped at 5. Ported from Composer.tsx
// `suggestionsFor`.
func suggestionsFor(cmds []Command, value string) []Command {
	if !strings.HasPrefix(value, "/") {
		return nil
	}
	q := strings.ToLower(value[1:])
	out := make([]Command, 0, paletteCap)
	for _, c := range cmds {
		name := strings.TrimPrefix(c.Name, "/")
		if strings.HasPrefix(strings.ToLower(name), q) ||
			strings.Contains(strings.ToLower(c.Desc), q) {
			out = append(out, c)
			if len(out) == paletteCap {
				break
			}
		}
	}
	return out
}
