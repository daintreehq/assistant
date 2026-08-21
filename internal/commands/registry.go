// Package commands is the slash-command subsystem: the pure-data COMMAND_REGISTRY
// (single source of truth for names/syntax/palette/help — drives suggestions, help
// blobs, and both handler surfaces) plus the two handlers (REPL prints via render;
// UI returns structured cards) and the rich /doctor checklist.
package commands

import "strings"

// CommandMeta is one row of the registry.
type CommandMeta struct {
	// Name is the bare command word (no leading slash).
	Name string
	// Palette is the short composer-suggestion blurb.
	Palette string
	// Syntax is the help-line usage form ("/audit [n]").
	Syntax string
	// Help is the one-line description.
	Help string
}

// COMMAND_REGISTRY is the ordered, canonical command table. Aliases (? ⇐ help;
// exit/q ⇐ quit) are handled by the parsers but intentionally NOT listed here.
// Order is a product contract (drives the help/palette ordering).
var COMMAND_REGISTRY = []CommandMeta{
	{Name: "status", Syntax: "/status", Palette: "runtime and connections", Help: "backend, MCP, project, session, tier"},
	{Name: "inbox", Syntax: "/inbox [sev]", Palette: "items requiring attention", Help: "watcher/timer events (info|attention|urgent|blocked)"},
	{Name: "tools", Syntax: "/tools [query]", Palette: "list / search tools", Help: "list/search available tools"},
	{Name: "timers", Syntax: "/timers", Palette: "scheduled operations", Help: "scheduled timers"},
	{Name: "watchers", Syntax: "/watchers", Palette: "supervised agents", Help: "active watchers"},
	{Name: "grants", Syntax: "/grants", Palette: "live automation grants", Help: "live automation grants (actor, uses, expiry)"},
	{Name: "workflows", Syntax: "/workflows [status]", Palette: "workflow runs", Help: "workflow runs (default active; status or 'all')"},
	{Name: "workflow", Syntax: "/workflow [id|sub]", Palette: "workflow graph detail", Help: "graph detail; resume | reconcile <id> | cancel <id>"},
	{Name: "launches", Syntax: "/launches", Palette: "recent agent launches", Help: "recent agent-spawn sagas (newest first)"},
	{Name: "audit", Syntax: "/audit [n]", Palette: "recent tool calls · export", Help: "recent calls (def 15); export <json|csv>"},
	{Name: "explain", Syntax: "/explain [runId]", Palette: "reconstruct a run's timeline", Help: "replay a run; no id lists recent runs"},
	{Name: "models", Syntax: "/models", Palette: "backend-owned model routing", Help: "show backend-owned model routing"},
	{Name: "cost", Syntax: "/cost", Palette: "what this session has spent", Help: "session spend on your own upstream key"},
	{Name: "backend", Syntax: "/backend [target]", Palette: "which backend answers", Help: "switch backend (remembered); no arg lists, 'default' forgets"},
	{Name: "routing", Syntax: "/routing", Palette: "which endpoints serve you", Help: "endpoint routing: privacy filter and ranking"},
	{Name: "permissions", Syntax: "/permissions [tier]", Palette: "supervisor | operator | system", Help: "show or set tier (supervisor|operator|system)"},
	{Name: "approvals", Syntax: "/approvals [clear]", Palette: "cockpit tool approvals", Help: "cockpit session approvals; clear resets them"},
	{Name: "memory", Syntax: "/memory [sub]", Palette: "list · pin · unpin · forget", Help: "list | pin <id> | unpin <id> | forget <id>"},
	{Name: "compact", Syntax: "/compact", Palette: "summarize the conversation", Help: "summarize + reset the conversation"},
	{Name: "clear", Syntax: "/clear", Palette: "reset the conversation", Help: "drop the conversation — start fresh"},
	{Name: "doctor", Syntax: "/doctor", Palette: "environment check", Help: "check MCP / config / project (with fixes)"},
	{Name: "reconnect", Syntax: "/reconnect", Palette: "retry the Daintree connection", Help: "retry the Daintree MCP connection"},
	{Name: "help", Syntax: "/help", Palette: "all commands and keys", Help: "this help (alias: /?)"},
	{Name: "quit", Syntax: "/quit", Palette: "exit", Help: "exit (aliases: /q, /exit)"},
}

// helpPad is the syntax column width (clears the widest syntax "/permissions [tier]").
const helpPad = 24

// PaletteEntries returns ["/name", palette] pairs for the composer palette.
func PaletteEntries() [][2]string {
	out := make([][2]string, 0, len(COMMAND_REGISTRY))
	for _, c := range COMMAND_REGISTRY {
		out = append(out, [2]string{"/" + c.Name, c.Palette})
	}
	return out
}

// PaletteRow is a palette entry carrying the usage Syntax form, so the composer can show an
// inline argument hint ("/audit [n]") for the highlighted command.
type PaletteRow struct{ Name, Desc, Syntax string }

// PaletteRows returns the full palette rows (name + palette blurb + syntax).
func PaletteRows() []PaletteRow {
	out := make([]PaletteRow, 0, len(COMMAND_REGISTRY))
	for _, c := range COMMAND_REGISTRY {
		out = append(out, PaletteRow{Name: "/" + c.Name, Desc: c.Palette, Syntax: c.Syntax})
	}
	return out
}

// HelpLines returns each command's "syntax<pad>help" line.
func HelpLines() []string {
	out := make([]string, 0, len(COMMAND_REGISTRY))
	for _, c := range COMMAND_REGISTRY {
		out = append(out, padRight(c.Syntax, helpPad)+c.Help)
	}
	return out
}

// suggestCommand returns the closest registry command name within edit distance 2 (a "did
// you mean?" for a mistyped slash command), or "" when nothing is close enough to suggest.
func suggestCommand(cmd string) string {
	cmd = strings.ToLower(strings.TrimSpace(cmd))
	if cmd == "" {
		return ""
	}
	best, bestD := "", 3 // strictly < 3, i.e. at most 2 edits
	for _, c := range COMMAND_REGISTRY {
		if d := levenshtein(cmd, c.Name); d < bestD {
			best, bestD = c.Name, d
		}
	}
	return best
}

// levenshtein is the classic edit distance (insertions/deletions/substitutions), rune-aware
// so multi-byte input doesn't skew the count.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// PanelKey identifies an on-demand UI panel.
type PanelKey string

const (
	PanelWatchers PanelKey = "watchers"
	PanelInbox    PanelKey = "inbox"
	PanelTimers   PanelKey = "timers"
	PanelAudit    PanelKey = "audit"
	PanelHelp     PanelKey = "help"
)

// parseCommand splits a slash line into (cmd, arg, rest). Identical to both TS
// handlers: `line.slice(1).trim().split(/\s+/)`.
func parseCommand(line string) (cmd string, arg string, rest []string) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(line, "/"))
	if trimmed == "" {
		return "", "", nil
	}
	fields := strings.Fields(trimmed)
	cmd = fields[0]
	rest = fields[1:]
	arg = strings.Join(rest, " ")
	return cmd, arg, rest
}

// canonical maps an alias to its registry name (? → help; exit/q → quit).
func canonical(cmd string) string {
	switch cmd {
	case "?":
		return "help"
	case "exit", "q":
		return "quit"
	default:
		return cmd
	}
}

// IsKnownCommand reports whether a slash line names a command the catalog actually has,
// aliases included. It answers a DIFFERENT question from UICommandResult.Handled, and
// the difference is deliberate: an unknown command is still "handled" — the handler
// consumed the line and produced an "Unknown command" card for the cockpit to render —
// so that field cannot tell a caller whether the command EXISTS.
//
// Only the JSONL stream needs the distinction, because only there is the reader a script
// rather than a person: a typo'd /claer in a test script must be machine-detectable, and
// on the wire it looks exactly like a command that ran and said nothing.
func IsKnownCommand(line string) bool {
	// Trimmed FIRST: parseCommand strips the leading "/" before it trims, so a padded
	// "  /help  " would otherwise reach it with the slash still buried behind spaces and
	// parse as the command "/help", which no registry row is named.
	cmd, _, _ := parseCommand(strings.TrimSpace(line))
	if cmd == "" {
		return false
	}
	name := canonical(cmd)
	for _, c := range COMMAND_REGISTRY {
		if c.Name == name {
			return true
		}
	}
	return false
}

// padRight pads s with spaces to at least width runes.
func padRight(s string, width int) string {
	n := len([]rune(s))
	if n >= width {
		return s
	}
	return s + strings.Repeat(" ", width-n)
}
