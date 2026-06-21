package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_activity.go renders the activity tree — a turn's delegated work as a
// branch tree, the brand signature (ui-transcript.md §4). Each activity is one
// branch carrying a state glyph + a human verb + target + duration.

// presentTool maps an internal tool name to a human verb (ui-transcript.md §4
// verb map, abbreviated to the families that exist in the Go port). Unmapped names
// fall back to the raw name title-cased.
func presentTool(name string) string {
	switch name {
	case "fs.read":
		return "Read"
	case "fs.list":
		return "Listed"
	case "fs.search", "search":
		return "Searched"
	case "extract", "fs.extract":
		return "Extracted"
	case "agentTask.spawnForEdits", "agent.spawn":
		return "Delegated"
	case "watcher.create", "watcher.terminal.create", "watch":
		return "Watching"
	case "timer.create", "schedule":
		return "Scheduled"
	case "artifact.read", "artifactx.read":
		return "Read"
	case "snapshot", "context.snapshot":
		return "Snapshotted"
	case "daintree.call":
		return "Daintree"
	case "skill.find":
		return "Skill"
	case "queue.publish":
		return "Note"
	default:
		if name == "" {
			return "Tool"
		}
		// Title-case the leaf segment of a dotted name.
		leaf := name
		if i := strings.LastIndexByte(name, '.'); i >= 0 {
			leaf = name[i+1:]
		}
		if len(leaf) == 0 {
			return name
		}
		return strings.ToUpper(leaf[:1]) + leaf[1:]
	}
}

// activityGlyph returns the state glyph (animated spinner frame for active rows)
// and the lipgloss style for that row's tone (§4 glyphs/tones).
func activityGlyph(th theme.Theme, a Activity, spinnerFrame int) (string, string) {
	g := th.Glyphs
	switch a.State {
	case ActQueued:
		return g.Queued, "muted"
	case ActActive:
		// Animated spinner (not a static glyph) for the active row. Active is
		// CYAN (info) so a live row reads as "working", visually distinct from a
		// completed green ✓ (§4: cyan = live, green = done).
		frames := g.Spinner
		if len(frames) == 0 {
			return g.Active, "info"
		}
		return frames[spinnerFrame%len(frames)], "info"
	case ActDone:
		return g.Done, "accent"
	case ActFailed:
		return g.Failed, "danger"
	case ActWaiting:
		// Plain waiting (e.g. a watcher) → the clock glyph, warning tone (yellow);
		// the ◇/blocked diamond is reserved for an explicit approval-pending state.
		return g.Waiting, "warning"
	default:
		return g.Bullet, "muted"
	}
}

// styleFor maps a tone name to a lipgloss render of s.
func styleFor(th theme.Theme, tone, s string) string {
	switch tone {
	case "accent":
		return th.Accent().Render(s)
	case "danger":
		return th.Danger().Render(s)
	case "blocked":
		return th.Blocked().Render(s)
	case "warning":
		return th.Warning().Render(s)
	case "muted":
		return th.Muted().Render(s)
	case "info":
		return th.Info().Render(s)
	default:
		return th.Body().Render(s)
	}
}

// renderActivityRow renders one branch row: "<branch> <glyph> <verb> <detail> <dur>".
// last marks the final branch (└─ vs ├─). expanded reveals raw args. now drives
// the live elapsed on an active row.
func renderActivityRow(th theme.Theme, a Activity, last, expanded bool, spinnerFrame int, now int64, width int) string {
	g := th.Glyphs
	branch := g.BranchMid
	if last {
		branch = g.BranchLast
	}
	glyph, tone := activityGlyph(th, a, spinnerFrame)

	verb := padRight(presentTool(a.Name), labelWidth)

	// Detail: while active show the live in-tool substep ("launching terminal");
	// the settled summary takes over once done (§4). On failure show BOTH target and
	// the failure summary so the outcome isn't hidden by the original detail (§4 / §6).
	detail := a.Detail
	if a.State == ActActive && a.ProgressMsg != "" {
		detail = a.ProgressMsg
	}
	if a.State == ActFailed && a.Outcome != "" {
		if detail != "" {
			detail = detail + " — " + a.Outcome
		} else {
			detail = a.Outcome
		}
	}

	// Duration / queued marker on the right.
	right := ""
	switch a.State {
	case ActQueued:
		right = "queued"
	case ActActive:
		if a.StartedAt > 0 {
			right = formatDuration(now - a.StartedAt)
		}
	case ActDone, ActFailed:
		if a.StartedAt > 0 && a.EndedAt > 0 {
			right = formatDuration(a.EndedAt - a.StartedAt)
		}
	}

	// Right-align the duration / queued marker into a fixed gutter so every
	// "8ms"/"412ms" lines up in a clean column (§4: TS used space-between). We
	// reserve durationCols on the right, truncate the detail to what's left, then
	// pad so the muted duration sits flush-right of the row.
	var b strings.Builder
	b.WriteString(th.Muted().Render(branch))
	b.WriteByte(' ')
	b.WriteString(styleFor(th, tone, glyph))
	b.WriteByte(' ')
	b.WriteString(th.Body().Render(verb))

	// Width budget left of the duration gutter: row width minus the prefix
	// (branch+glyph spacing), the verb label, and the reserved duration column.
	detailCap := width - prefixCols - labelWidth - durationCols
	if detailCap < 0 {
		detailCap = 0
	}
	if detail != "" && detailCap > 0 {
		b.WriteByte(' ')
		// Account for the separating space we just wrote.
		b.WriteString(th.Dim().Render(truncateCells(detail, detailCap-1)))
	}

	if right != "" {
		// Pad the line out so the duration is flush against the right gutter.
		// cellWidth(b) already counts the styled spans correctly (ANSI-aware).
		used := cellWidth(b.String())
		target := width - cellWidth(right)
		if pad := target - used; pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		} else {
			b.WriteByte(' ')
		}
		b.WriteString(th.Muted().Render(right))
	}
	// Truncate the assembled row line to width BEFORE appending any expanded-args
	// line, so the multi-line truncate never clips the row's flush-right duration.
	row := truncateCells(b.String(), width)
	if expanded && a.Args != "" && a.Args != "{}" {
		row += "\n" + th.Muted().Render("   args "+truncateCells(a.Args, width-8))
	}
	return row
}

// labelWidth / prefixCols / durationCols are the column budgets the activity tree
// aligns to (ui-transcript.md §12 key constants).
const (
	labelWidth   = 11
	prefixCols   = 5
	durationCols = 8
)

// padRight right-pads s to at least w cells (cell-measured).
func padRight(s string, w int) string {
	if d := w - cellWidth(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}
