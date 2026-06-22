package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_chrome.go renders the masthead, status line, note/command cells.

// mastheadParams is the (frozen) masthead snapshot committed once to scrollback.
type mastheadParams struct {
	Version     string
	ProjectName string
	Tier        domain.Tier
	Logging     bool
	LogFile     string
	Destructive bool // a git/system action awaiting confirmation → red tier
}

// renderMasthead renders the committed masthead (ui-transcript.md §7). The closing
// rule is a FIXED-WIDTH run of "─" snapshotted at the commit width — never a
// fill-to-terminal-width rule, which the host would reflow on a narrow resize.
func renderMasthead(th theme.Theme, p mastheadParams, width int) string {
	g := th.Glyphs
	var b strings.Builder

	// 1. Identity line.
	id := th.Body().Bold(true).Render("Daintree Assistant")
	if p.Version != "" {
		id += th.Dim().Render(" v" + p.Version)
	}
	b.WriteString(truncateCells(id, width))

	// 2. Project name.
	if p.ProjectName != "" {
		b.WriteByte('\n')
		b.WriteString(th.Dim().Render(truncateCells(p.ProjectName, width)))
	}

	// 4. Tier line — QUIET (dim) at rest for every tier; red ONLY when a
	// destructive action awaits confirmation. The tier→gloss separator is the
	// active glyph set's bullet (· unicode / - ascii), NOT a hardcoded "·", so the
	// DAINTREE_ASCII fallback flows through (mirrors Header.tsx `set.bullet`).
	b.WriteByte('\n')
	tierStyle := th.Dim()
	if p.Destructive {
		tierStyle = th.Danger()
	}
	gloss := tierGloss(p.Tier)
	tierLine := th.Dim().Render("tier ") + tierStyle.Render(string(p.Tier))
	if gloss != "" {
		tierLine += th.Dim().Render(" " + g.Bullet + " " + gloss)
	}
	b.WriteString(truncateCells(tierLine, width))

	// 5. Fixed-width closing rule.
	b.WriteByte('\n')
	b.WriteString(th.Muted().Render(strings.Repeat(g.Rule, width)))

	// 6. Debug-log badge, BELOW the rule. "◌ logging" label in warning yellow (the
	// active glyph), then a dim " · <path>". The label is pinned (never clipped to
	// "loggin"); only the path truncates — truncateCells keeps left-priority cells.
	if p.Logging {
		b.WriteByte('\n')
		badge := th.Warning().Render(g.Active+" logging") + th.Dim().Render(" "+g.Bullet+" "+p.LogFile)
		b.WriteString(truncateCells(badge, width))
	}
	return b.String()
}

// tierGloss is the dim one-liner after the tier name (§7).
func tierGloss(t domain.Tier) string {
	switch t {
	case domain.TierSupervisor:
		return "read & UI only"
	case domain.TierOperator:
		return "terminals, projects, external"
	case domain.TierSystem:
		return "full access (git, system)"
	default:
		return ""
	}
}

// statusParams is the live status rollup's input (ui-transcript.md §6).
//
// The active-agent badge is carried as its INGREDIENTS, not a pre-formatted
// string: StatusLine.tsx inlines the StateBadge as a tone-tinted "<glyph> LABEL"
// span (color from the badge tone, glyph from the same tone) followed by a DIM run
// of " id [· goal] [duration]". Passing a flat string would lose the tone color,
// the leading state glyph, and the dim styling of the id — so we rebuild it here.
type statusParams struct {
	ContextPct  int // CTX %; <0 means "no usage yet" (hidden)
	HasUsage    bool
	Cost        float64
	Model       string
	AttentionN  int
	TopSeverity domain.Severity
	Agents      int
	Degraded    bool

	// Active-agent badge ingredients ("" ActiveLabel ⇒ no agent working). ActiveTone
	// is a semantic tone name as understood by styleFor/toneGlyphFor ("active",
	// "success", "warning", "danger", "blocked", "neutral").
	ActiveTone     string
	ActiveLabel    string // UPPERCASE label, e.g. "WORKING"
	ActiveID       string // terminal/watcher id, e.g. "term_8" (rendered dim)
	ActiveGoal     string // optional goal/title (rendered dim, after a " · ")
	ActiveDuration string // optional elapsed token, e.g. "18s" (rendered dim)

	// ActiveAgent is a back-compat fallback: a pre-formatted badge string used only
	// when ActiveLabel is empty (legacy callers / tests). Prefer the ingredients.
	ActiveAgent string
}

// renderStatusLine renders the ≤56-cell compact rollup; it speaks ONLY when it has
// something to say (renders "" otherwise — no "Standing by" placeholder, §6).
func renderStatusLine(th theme.Theme, p statusParams, width int) string {
	g := th.Glyphs
	cap := width
	if cap > LiveChromeMaxWidth {
		cap = LiveChromeMaxWidth
	}
	var segs []string

	// MCP connection — surfaced BY EXCEPTION only. A healthy link is announced once as a
	// top status note when it settles ("● Connected to Daintree MCP", see update.go), so the
	// always-visible footer stays quiet while connected; it speaks only when the link is DOWN
	// (a persistent condition the operator must see), never a steady-state healthy badge.
	if p.Degraded {
		segs = append(segs, th.Warning().Render("▲ Daintree MCP unavailable"))
	}

	// Active-agent badge: a tone-tinted "<glyph> LABEL" run (color + glyph from the
	// tone), then a DIM " id [· goal] [duration]" run — exactly as StatusLine.tsx
	// inlines the StateBadge. Falls back to the pre-formatted ActiveAgent string
	// only when no structured label is supplied (legacy callers).
	active := p.ActiveLabel != "" || p.ActiveAgent != ""
	if p.ActiveLabel != "" {
		badge := styleFor(th, p.ActiveTone, toneGlyphFor(g, p.ActiveTone)+" "+p.ActiveLabel)
		dimTail := " " + p.ActiveID
		if p.ActiveGoal != "" {
			dimTail += " " + g.Bullet + " " + p.ActiveGoal
		}
		if p.ActiveDuration != "" {
			dimTail += " " + p.ActiveDuration
		}
		segs = append(segs, badge+th.Dim().Render(dimTail))
	} else if p.ActiveAgent != "" {
		segs = append(segs, th.Info().Render(p.ActiveAgent))
	}
	// CTX% — required when usage has arrived; tinted by pressure (≥90 red, ≥75 amber).
	if p.HasUsage && p.ContextPct >= 0 {
		ctx := "CTX " + itoa(p.ContextPct) + "%"
		switch {
		case p.ContextPct >= 90:
			ctx = th.Danger().Render(ctx)
		case p.ContextPct >= 75:
			ctx = th.Warning().Render(ctx)
		default:
			ctx = th.Dim().Render(ctx)
		}
		segs = append(segs, ctx)
	}
	// Cost + model are idle-only (no active agent).
	if !active {
		if p.Cost > 0 {
			segs = append(segs, th.Dim().Render(formatCost(p.Cost)))
		}
		if p.Model != "" && width >= 62 {
			segs = append(segs, th.Dim().Render(p.Model))
		}
	}
	if p.AttentionN > 0 {
		tone := severityTone(p.TopSeverity)
		segs = append(segs, styleFor(th, tone, "!"+itoa(p.AttentionN)))
	}
	if p.Agents > 0 {
		segs = append(segs, th.Dim().Render("agents "+itoa(p.Agents)))
	}

	if len(segs) == 0 {
		return ""
	}
	sep := th.Dim().Render(" · ")
	return truncateCells(strings.Join(segs, sep), cap)
}

// toneGlyphFor is the badge glyph that always accompanies a tone's color (mirrors
// theme.ts toneGlyph): active ◌, success ✓, danger ×, neutral ·. warning/blocked
// use the literal attention mark "!" (theme.ts `set.attention`), which has no
// dedicated GlyphSet field. ASCII fallback flows through the unicode set members.
func toneGlyphFor(g theme.GlyphSet, tone string) string {
	switch tone {
	case "active", "info":
		return g.Active
	case "success", "accent":
		return g.Done
	case "danger":
		return g.Failed
	case "warning", "blocked":
		return "!"
	default:
		return g.Bullet
	}
}

// badgeTone maps an agent badge to a render tone for the compact strip: the urgent
// states are tinted (red needs-input/failed, amber blocked/review), everything else
// is the neutral "active" cyan.
func badgeTone(badge string) string {
	switch badge {
	case "NEEDS INPUT", "FAILED":
		return "danger"
	case "BLOCKED", "REVIEW":
		return "warning"
	default:
		return "active"
	}
}

// severityTone maps a severity to a render tone for the attention count.
func severityTone(s domain.Severity) string {
	switch s {
	case domain.SeverityUrgent, domain.SeverityBlocked:
		return "danger"
	case domain.SeverityAttention:
		return "warning"
	default:
		return "muted"
	}
}

// renderNoteCell renders a standalone NoteCell (one line, leading blank owned by
// the cell, §3).
func renderNoteCell(th theme.Theme, n *NoteCell, width int) string {
	g := th.Glyphs
	glyph, tone := noteGlyph(th, n.Level)
	// Tone the │ continuation spine with the note tone (green info / red error)
	// instead of flat muted gray (§7) — matches the greenish MCP-connected note.
	cont := styleFor(th, tone, g.Continuation)
	return truncateCells(cont+styleFor(th, tone, glyph)+" "+th.Body().Render(n.Text), width)
}

// renderCommandCell renders a slash-command result into the transcript (§3).
func renderCommandCell(th theme.Theme, c *CommandCell, width int) string {
	var b strings.Builder
	if c.Title != "" {
		b.WriteString(th.Info().Bold(true).Render(truncateCells(c.Title, width)))
	}
	if c.Text != "" {
		for _, line := range strings.Split(c.Text, "\n") {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(th.Dim().Render(truncateCells(line, width)))
		}
	}
	return b.String()
}

// itoa is a tiny base-10 int formatter (avoids strconv in hot render paths).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
