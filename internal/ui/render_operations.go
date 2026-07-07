package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_operations.go renders the operations deck: five sections
// in strict human-priority order; empty sections vanish entirely so urgency owns
// the space. A non-nil activePanel filters to one section ("focus" is a filter, not
// a scroll).

// PanelKey filters the ops deck to one section.
type PanelKey string

const (
	PanelNone     PanelKey = ""
	PanelWatchers PanelKey = "watchers" // → AGENTS
	PanelInbox    PanelKey = "inbox"    // → NEEDS ATTENTION
	PanelTimers   PanelKey = "timers"   // → SCHEDULED
	PanelAudit    PanelKey = "audit"    // → RECENT
	PanelHelp     PanelKey = "help"     // help view (rendered separately)
)

// renderOperations renders the deck filtered by panel (PanelNone = full deck).
func renderOperations(th theme.Theme, d Dashboard, panel PanelKey, now int64, width int) string {
	var sections []string

	show := func(p PanelKey) bool { return panel == PanelNone || panel == p }

	// A focused-but-empty section shows ONLY the honest placeholder, never the section
	// title (the title would imply data that isn't there).
	section := func(title string, lines []string, focused bool) string {
		if focused && len(stripBlank(lines)) == 0 {
			return th.Dim().Render("Nothing here yet.")
		}
		return renderSection(th, title, lines)
	}

	// NOW — the single most-active agent (or "Standing by").
	if show(PanelWatchers) {
		sections = append(sections, renderSection(th, "NOW", []string{nowLine(th, d, now, width)}))
	}
	// NEEDS ATTENTION — urgent queue events.
	if show(PanelInbox) {
		sections = append(sections, section("NEEDS ATTENTION", inboxLines(th, d, width, false), panel == PanelInbox))
	}
	// WORKFLOWS — open workflow-intelligence execution graphs (cap 3). Rides the
	// watchers panel: a tracked workflow and its supervised agents are one
	// "work in flight" story to the human. Vanishes entirely when the feature is
	// off (no rows) — the deck is byte-identical to before.
	if show(PanelWatchers) {
		sections = append(sections, renderSection(th, "WORKFLOWS", workflowGraphLines(th, d, width)))
	}
	// AGENTS — every supervised agent (cap 6).
	if show(PanelWatchers) {
		sections = append(sections, section("AGENTS", agentLines(th, d, width, false), panel == PanelWatchers))
	}
	// ASYNC — live async futures the runtime is polling (cap 4). Rides the
	// watchers panel: to the human, async work and supervised agents are one
	// "background things in flight" story.
	if show(PanelWatchers) {
		sections = append(sections, renderSection(th, "ASYNC", asyncLines(th, d, now, width)))
	}
	// SCHEDULED — upcoming timers (cap 4).
	if show(PanelTimers) {
		sections = append(sections, section("SCHEDULED", timerLines(th, d, width, false), panel == PanelTimers))
	}
	// RECENT — recent audit (cap 5).
	if show(PanelAudit) {
		sections = append(sections, section("RECENT", auditLines(th, d, width, false), panel == PanelAudit))
	}

	// Drop empty sections (nil body) — they vanish so urgency owns the space.
	var out []string
	for _, s := range sections {
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, "\n\n")
}

// stripBlank returns the non-blank (ANSI-stripped) lines of a section body.
func stripBlank(lines []string) []string {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(stripAnsi(l)) != "" {
			out = append(out, l)
		}
	}
	return out
}

// renderSection renders a titled section; returns "" when the body has no lines
// (empty sections vanish). A focused-but-empty section emits an honest line.
func renderSection(th theme.Theme, title string, lines []string) string {
	// Strip empty lines.
	var body []string
	for _, l := range lines {
		if strings.TrimSpace(stripAnsi(l)) != "" {
			body = append(body, l)
		}
	}
	if len(body) == 0 {
		return ""
	}
	head := th.Muted().Render(title)
	return head + "\n" + strings.Join(body, "\n")
}

func nowLine(th theme.Theme, d Dashboard, now int64, width int) string {
	if len(d.Agents) == 0 {
		// No supervised agents, but live async work is still "something happening
		// NOW" — a "Standing by" above a populated ASYNC section would contradict it.
		if len(d.Async) > 0 {
			r := d.Async[0]
			line := th.Info().Render("ASYNC") + " " + th.Body().Render(asyncDeckTitle(r.Title))
			if r.CreatedAt > 0 {
				line += th.Muted().Render(" " + formatDuration(now-r.CreatedAt))
			}
			return truncateCells(line, width)
		}
		return th.Dim().Render("Standing by")
	}
	a := d.Agents[0]
	// Tone the badge by urgency (red needs-input/failed, amber blocked/review, else
	// cyan), matching the compact footer strip — the same "NEEDS INPUT" reads red in
	// both surfaces instead of flat cyan in the deck and red in the footer.
	line := styleFor(th, badgeTone(a.Badge), a.Badge) + " " + th.Body().Render(firstNonEmpty(a.Title, a.Goal))
	if a.StartedAt > 0 {
		line += th.Muted().Render(" " + formatDuration(now-a.StartedAt))
	}
	return truncateCells(line, width)
}

// asyncDeckTitle flattens an async invocation title to one safe deck line: the
// tool derives titles bounded and single-line, but a row written by an older
// dev build (or any unexpected writer) must not break the deck layout.
func asyncDeckTitle(title string) string {
	t := strings.Join(strings.Fields(title), " ")
	if t == "" {
		return "async operation"
	}
	return t
}

func inboxLines(th theme.Theme, d Dashboard, width int, focused bool) []string {
	if len(d.Inbox) == 0 {
		if focused {
			return []string{th.Dim().Render("Nothing here yet.")}
		}
		return nil
	}
	var out []string
	for _, e := range d.Inbox {
		tone := severityTone(e.Severity)
		title := e.Title
		if e.Count > 1 {
			title += " ×" + itoa(e.Count)
		}
		out = append(out, truncateCells(styleFor(th, tone, "• "+title), width))
		if e.Summary != "" {
			tag := epistemicTag(e.EpistemicKind)
			out = append(out, truncateCells(th.Dim().Render("  "+tag+e.Summary), width))
		}
	}
	return out
}

func agentLines(th theme.Theme, d Dashboard, width int, focused bool) []string {
	if len(d.Agents) == 0 {
		if focused {
			return []string{th.Dim().Render("Nothing here yet.")}
		}
		return nil
	}
	const cap = 6
	rows := d.Agents
	more := 0
	if len(rows) > cap {
		more = len(rows) - cap
		rows = rows[:cap]
	}
	var out []string
	for _, a := range rows {
		// Badge toned by urgency (see nowLine) so the deck's most-urgent agents read
		// red/amber, consistent with the footer strip rather than a flat cyan list.
		out = append(out, truncateCells(styleFor(th, badgeTone(a.Badge), a.Badge)+" "+th.Body().Render(firstNonEmpty(a.Title, a.Goal)), width))
		tag := epistemicTag(a.EpistemicKind)
		second := tag + a.ID
		if a.AgentState != "" {
			second += " · " + a.AgentState
		}
		if a.Preview != "" {
			second += " · " + a.Preview
		}
		out = append(out, truncateCells(th.Dim().Render("  "+second), width))
	}
	if more > 0 {
		out = append(out, th.Dim().Render("  +"+itoa(more)+" more"))
	}
	return out
}

// asyncLines renders the live async futures: a yellow dot, the title, the
// asy_… id, and a live elapsed — mirroring the transcript's async-pending row
// so the two surfaces read as one state.
func asyncLines(th theme.Theme, d Dashboard, now int64, width int) []string {
	if len(d.Async) == 0 {
		return nil
	}
	const cap = 4
	rows := d.Async
	more := 0
	if len(rows) > cap {
		more = len(rows) - cap
		rows = rows[:cap]
	}
	g := th.Glyphs
	var out []string
	for _, r := range rows {
		line := styleFor(th, "warning", g.Async) + " " + th.Body().Render(asyncDeckTitle(r.Title))
		if r.CreatedAt > 0 {
			line += th.Muted().Render(" " + formatDuration(now-r.CreatedAt))
		}
		out = append(out, truncateCells(line, width))
		out = append(out, truncateCells(th.Dim().Render("  "+r.ID+" · "+string(r.Status)), width))
	}
	if more > 0 {
		out = append(out, th.Dim().Render("  +"+itoa(more)+" more"))
	}
	return out
}

// workflowGraphLines renders the open execution graphs: a status marker, the
// goal, then a dim second line with progress and the recommended next action —
// the living-plan view, not a raw tool log.
func workflowGraphLines(th theme.Theme, d Dashboard, width int) []string {
	if len(d.WorkflowGraphs) == 0 {
		return nil
	}
	const cap = 3
	rows := d.WorkflowGraphs
	more := 0
	if len(rows) > cap {
		more = len(rows) - cap
		rows = rows[:cap]
	}
	g := th.Glyphs
	var out []string
	for _, w := range rows {
		// Healthy uses the shared filled dot; blocked keeps the ⚠ warning sign, both
		// with ASCII fallbacks so a non-unicode terminal doesn't render mojibake.
		tone, marker := "accent", g.Async
		if w.Blocked {
			tone, marker = "warning", "⚠"
			if !th.Unicode {
				marker = g.Alert
			}
		}
		goal := strings.Join(strings.Fields(w.Goal), " ")
		out = append(out, truncateCells(styleFor(th, tone, marker)+" "+th.Body().Render(goal)+th.Muted().Render("  "+w.Status), width))
		second := w.ID + " · " + w.Progress
		if w.Next != "" {
			second += " · next: " + strings.Join(strings.Fields(w.Next), " ")
		}
		out = append(out, truncateCells(th.Dim().Render("  "+second), width))
	}
	if more > 0 {
		out = append(out, th.Dim().Render("  +"+itoa(more)+" more"))
	}
	return out
}

func timerLines(th theme.Theme, d Dashboard, width int, focused bool) []string {
	if len(d.Timers) == 0 {
		if focused {
			return []string{th.Dim().Render("Nothing here yet.")}
		}
		return nil
	}
	const cap = 4
	rows := d.Timers
	if len(rows) > cap {
		rows = rows[:cap]
	}
	var out []string
	for _, t := range rows {
		out = append(out, truncateCells(th.Muted().Render(clockTime(t.FireAt))+" "+th.Body().Render(t.Title), width))
	}
	return out
}

func auditLines(th theme.Theme, d Dashboard, width int, focused bool) []string {
	if len(d.Audit) == 0 {
		if focused {
			return []string{th.Dim().Render("Nothing here yet.")}
		}
		return nil
	}
	const cap = 5
	rows := d.Audit
	if len(rows) > cap {
		rows = rows[:cap]
	}
	g := th.Glyphs
	var out []string
	for _, a := range rows {
		glyph := g.Done
		tone := "accent"
		if a.Outcome != "ok" && a.Outcome != "grant_ok" {
			glyph, tone = g.Failed, "danger"
		}
		line := styleFor(th, tone, glyph) + " " + th.Body().Render(presentTool(a.ToolName))
		if a.DurationMs > 0 {
			line += th.Muted().Render(" " + formatDuration(a.DurationMs))
		}
		out = append(out, truncateCells(line, width))
	}
	return out
}

// epistemicTag renders a 3-letter provenance tag ("" when absent, so legacy rows
// are unchanged). It is a trust signal, not decoration.
func epistemicTag(k domain.EpistemicKind) string {
	switch k {
	case domain.EpistemicObserved:
		return "[obs] "
	case domain.EpistemicInferred:
		return "[inf] "
	case domain.EpistemicUnverified:
		return "[unv] "
	default:
		return ""
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
