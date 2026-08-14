package ui

import (
	"os"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/theme"
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
	// AutoApprove raises the confirmation-bypass banner. It is on the masthead AND the
	// live status line for different reasons: the masthead is the permanent record in
	// scrollback (so a pasted transcript shows the session was running unattended), the
	// status line is what the user can actually see right now.
	AutoApprove bool
}

// renderMasthead renders the committed masthead. The closing rule
// spans the full chrome width (end-to-end). That is safe because a terminal resize now
// triggers a NUCLEAR REDRAW (onRedraw): the masthead is wiped and re-committed fresh at the
// new width, so the rule always matches the current width and never wraps on shrink.
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
	// DAINTREE_ASCII fallback flows through (the glyph set's `set.bullet`).
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
	if p.AutoApprove {
		// Its OWN row, not appended to the tier line. Appending put the safety text at the
		// right-hand end, where truncation eats it first — on a narrow terminal the tier
		// survived and the warning did not, which is exactly backwards. A left-anchored
		// row is the last thing to be cut, and it can carry the full sentence.
		//
		// It still sits next to the tier because it is a statement ABOUT the tier: the
		// gate is unchanged, but nothing inside it will ask first. Note the tier shown
		// here is the one at LAUNCH — `/permissions` can change it later, and this
		// masthead was already committed to scrollback by then.
		b.WriteByte('\n')
		b.WriteString(truncateCells(th.Danger().Render(g.Alert+" AUTO-APPROVE — mutating actions will not ask first"), width))
	}
	b.WriteString("")

	// 5. Full-width closing rule (end-to-end). Re-rendered fresh at the new width on every
	// resize via the nuclear redraw (onRedraw), so it can't wrap on shrink.
	b.WriteByte('\n')
	b.WriteString(th.Muted().Render(strings.Repeat(g.Rule, width)))

	// 6. Debug-log badge, BELOW the rule — the hollow "◌ logging" label in warning
	// yellow, then a dim " · <path>". The path is $HOME-collapsed to ~ and WRAPS across
	// rows rather than truncating with … so the full filename stays visible/copyable
	// (see renderLogBadge).
	if p.Logging {
		b.WriteByte('\n')
		b.WriteString(renderLogBadge(th, p.LogFile, width))
	}
	return b.String()
}

// renderLogBadge renders the debug-log badge: a hollow "◌ logging" label (warning
// amber) then a dim " · <path>". The path is $HOME-collapsed to a ~-relative form and
// HARD-WRAPS across rows when it can't fit — it used to truncate with an ellipsis,
// which hid the useful tail (the session id + .log). Continuation rows sit at the
// chrome's left edge (no extra indent) so the path stays clean to copy; every row is
// held to `width` cells so a narrow terminal never overflows.
func renderLogBadge(th theme.Theme, path string, width int) string {
	g := th.Glyphs
	label := th.Warning().Render(g.Active + " logging")
	if path == "" || width < 1 {
		return truncateCells(label, width)
	}
	disp := collapseHome(path)
	sep := " " + g.Bullet + " "
	headW := cellWidth(g.Active+" logging") + cellWidth(sep)
	// Pathologically narrow chrome (label+sep alone overflow the width): fall back to a
	// single truncated row rather than emit empty continuation rows.
	avail := width - headW
	if avail < 1 {
		return truncateCells(label+th.Dim().Render(sep+disp), width)
	}

	// Greedy hard-wrap by CELLS: the first row fills the space left after the label, the
	// continuation rows get the full width. Paths are space-less, so we break mid-token
	// (a word-wrapper would just truncate the whole path onto one row).
	//
	// We break when the rune won't fit AND there's somewhere better to put it: either the
	// row already holds something (curW > 0), or we're still on the narrow first row
	// (limit < width) and a full-width continuation row would give the rune more room.
	// That second clause defends the no-overflow invariant against a wide (2-cell)
	// leading grapheme whose width exceeds the tiny first-row budget — it lands on a
	// full-width row instead of spilling past `width`. (A lone grapheme wider than the
	// FULL width is only possible on a sub-2-column terminal, which is degenerate.)
	var rows []string
	var cur strings.Builder
	curW, limit := 0, avail
	for _, r := range disp {
		rw := cellWidth(string(r))
		if curW+rw > limit && (curW > 0 || limit < width) {
			rows = append(rows, cur.String())
			cur.Reset()
			curW, limit = 0, width
		}
		cur.WriteRune(r)
		curW += rw
	}
	if cur.Len() > 0 {
		rows = append(rows, cur.String())
	}

	var b strings.Builder
	for i, row := range rows {
		if i == 0 {
			b.WriteString(label + th.Dim().Render(sep+row))
			continue
		}
		b.WriteByte('\n')
		b.WriteString(th.Dim().Render(row))
	}
	return b.String()
}

// collapseHome rewrites a $HOME-prefixed absolute path to its ~-relative form for
// display (the home dir is noise and eats width). It collapses only on a true path-
// segment boundary, so a sibling like "/home/bobby" is never mangled when HOME is
// "/home/bob"; falls back to the raw path when HOME is unknown or unrelated.
func collapseHome(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(os.PathSeparator)) {
		return "~" + path[len(home):]
	}
	return path
}

// tierGloss is the dim one-liner after the tier name.
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

// statusParams is the live status rollup's input.
//
// The active-agent badge is carried as its INGREDIENTS, not a pre-formatted
// string: the StateBadge is inlined as a tone-tinted "<glyph> LABEL"
// span (color from the badge tone, glyph from the same tone) followed by a DIM run
// of " id [· goal] [duration]". Passing a flat string would lose the tone color,
// the leading state glyph, and the dim styling of the id — so we rebuild it here.
type statusParams struct {
	Cost        float64
	AttentionN  int
	TopSeverity domain.Severity
	Degraded    bool
	// ModelRateLimited surfaces a provider-throttled (429) health badge by exception,
	// mirroring Degraded; cleared once the model answers again.
	ModelRateLimited bool

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

	// AutoApprove raises the persistent confirmation-bypass badge.
	AutoApprove bool
}

// renderStatusLine renders the ≤56-cell compact rollup; it speaks ONLY when it has
// something to say (renders "" otherwise — no "Standing by" placeholder).
func renderStatusLine(th theme.Theme, p statusParams, width int) string {
	g := th.Glyphs
	cap := width
	if cap > LiveChromeMaxWidth {
		cap = LiveChromeMaxWidth
	}
	var segs []string

	// AUTO-APPROVE is stated ONCE, in the masthead, and deliberately not repeated here.
	//
	// It used to appear in both places on the theory that a scrolled-away masthead
	// leaves no warning. But the flag comes from an env var read at process start and
	// cannot change mid-session, so the masthead's statement is permanently accurate —
	// and a warning repeated on every frame beside the composer stops being read, which
	// costs more than the scrollback risk it was insuring against. `/status` and
	// `/permissions` both still report it on demand.
	//
	// Legacy/general callers may still supply degraded MCP state here. The cockpit's
	// primary path uses compact composer status plus the prominent warning in view.go.
	if p.Degraded {
		segs = append(segs, th.Warning().Render(g.Alert+" Daintree MCP unavailable"))
	}

	// Model health — surfaced BY EXCEPTION, same as the MCP link: a provider 429 that
	// outlasted the retry budget raises this badge; it clears on the next good round.
	if p.ModelRateLimited {
		segs = append(segs, th.Warning().Render(g.Alert+" Model rate-limited"))
	}

	// Active-agent badge: a tone-tinted "<glyph> LABEL" run (color + glyph from the
	// tone), then a DIM " id [· goal] [duration]" run — exactly as the inlined
	// StateBadge. Falls back to the pre-formatted ActiveAgent string
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
	// Cost is idle-only (no active agent). The model name is deliberately NOT shown:
	// the backend masks routing behind the constant public alias ("daintree-assistant"),
	// so echoing it here was a meaningless watermark, not information.
	if !active && p.Cost > 0 {
		segs = append(segs, th.Dim().Render(formatCost(p.Cost)))
	}
	if p.AttentionN > 0 {
		tone := severityTone(p.TopSeverity)
		segs = append(segs, styleFor(th, tone, th.Glyphs.Attention+itoa(p.AttentionN)))
	}

	if len(segs) == 0 {
		return ""
	}
	sep := th.Dim().Render(" · ")
	return truncateCells(strings.Join(segs, sep), cap)
}

// toneGlyphFor is the badge glyph that always accompanies a tone's color: active
// ◌, success ✓, danger ×, neutral ·. warning/blocked use the attention mark
// (GlyphSet.Attention — unicode "»" / ASCII "!").
func toneGlyphFor(g theme.GlyphSet, tone string) string {
	switch tone {
	case "active", "info":
		return g.Active
	case "success", "accent":
		return g.Done
	case "danger":
		return g.Failed
	case "warning", "blocked":
		return g.Attention
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

// attentionSeverityGlyph mirrors the /inbox severity glyphs (queue.severityIcon) so a
// transcript attention note reads the same as the inbox digest. internal/ui depends only
// on domain (never internal/queue), and the 7-glyph set is tiny and stable, so a local
// switch is cheaper than a package edge. The attention mark comes from the GlyphSet so it
// picks up the unicode "»" / ASCII "!" fallback; the rest are stable literals. Unknown
// severities fall back to the info glyph, matching queue.Format's ELSE-1 fallback.
func attentionSeverityGlyph(g theme.GlyphSet, sev domain.Severity) string {
	switch sev {
	case domain.SeverityDebug:
		return "·"
	case domain.SeverityDone:
		return "✓"
	case domain.SeverityAttention:
		return g.Attention
	case domain.SeverityBlocked:
		return "⛔"
	case domain.SeverityUrgent:
		return "‼"
	case domain.SeverityError:
		return "✗"
	default: // info + any unknown severity
		return "ℹ"
	}
}

// renderNoteCell renders a standalone NoteCell (one line, leading blank owned by
// the cell).
func renderNoteCell(th theme.Theme, n *NoteCell, width int) string {
	g := th.Glyphs
	glyph, tone := noteGlyph(th, n.Level)
	// An attention-routed note carries its precise severity: draw THAT one glyph (still
	// toned by Level) instead of the coarse level glyph PLUS a duplicate baked into the
	// text — the source of the old "! !" doubling. Ordinary notes leave Severity "" and
	// keep the level glyph.
	if n.Severity != "" {
		glyph = attentionSeverityGlyph(g, n.Severity)
	}
	// Tone the │ continuation spine with the note tone (green info / red error)
	// instead of flat muted gray — matches the greenish MCP-connected row.
	cont := styleFor(th, tone, g.Continuation)
	return truncateCells(cont+styleFor(th, tone, glyph)+" "+th.Body().Render(n.Text), width)
}

// renderCommandCell renders a slash-command result into the transcript.
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
