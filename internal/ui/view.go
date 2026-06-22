package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// view.go renders ONLY the live footer: the active turn
// + LiveRunStatus (driven by domain.RunPhase) + approval sheet OR composer + status
// line. Everything sealed lives in native scrollback and never appears here. The
// View also carries the window title (attention count) and stays on the NORMAL
// screen buffer with NO mouse capture and bracketed paste ON.

// View composes the footer string and returns a tea.View with the inline-cockpit
// program options baked in.
func (m Model) View() tea.View {
	var v tea.View

	// NON-NEGOTIABLE inline-cockpit options:
	//   AltScreen = false           → normal screen buffer (host owns scroll).
	//   MouseMode = MouseModeNone    → never capture the mouse (zero value).
	//   DisableBracketedPasteMode    → false (bracketed paste ON).
	// Setting nothing for AltScreen/MouseMode leaves them at the safe defaults.
	v.SetContent(m.footer())
	v.WindowTitle = m.windowTitle()
	return v
}

// windowTitle mirrors the unresolved attention count:
// "Daintree ⚠ N" when N>0, else "Daintree".
func (m Model) windowTitle() string {
	n := m.attentionN
	if n <= 0 {
		n = len(m.dashboard.Inbox)
	}
	if n > 0 {
		return "Daintree ⚠ " + itoa(n)
	}
	return "Daintree"
}

// footer builds the live footer string. On the operations/help views the footer is
// the deck/help in place of the composer (single column).
func (m Model) footer() string {
	if m.quitting {
		return ""
	}
	w := m.contentW()

	// During boot the splash OWNS the whole live View — nothing else renders. This is
	// the intended layout (the animation plays alone while MCP connects + the project
	// name resolves; the masthead, rule and composer appear only AFTER the hand-off),
	// and it is also what keeps the inline renderer stable: a short, fixed-height View
	// repaints cleanly, whereas splash-stacked-above-composer overflowed the viewport
	// and the cursor math drifted. Once booting flips false the masthead commits to
	// scrollback and the composer takes the bottom of the footer for good.
	if m.booting {
		return m.splash.bootView(m.theme, m.columns, m.rows)
	}

	// Minimum-terminal-size floor. The fixed bottom band (composer, plus an optional status
	// line or a multi-row approval sheet) is emitted WHOLE — it is never height-clamped the
	// way the live tail and the ops/help decks are. On a hard-shrunk pane it alone outgrows
	// m.rows, making View() taller than the terminal; a flush/commit tea.Println then scrolls
	// a frozen partial copy into native scrollback and corrupts it (bubbletea#1613). So below
	// a small row floor — applied to EVERY view, not just home, since ops/help still emit a
	// body line + an "Esc back" line and overflow at 1-2 rows — collapse the whole footer to
	// one dim line. Exactly one row, so View() can never exceed the terminal. The home path
	// adds a second, content-aware check once the band's true height is known (below).
	tooSmall := indentLines(truncateCells(m.theme.Dim().Render("terminal too small — resize taller"), w), LeftPad)
	if m.rows < minCockpitRows {
		return tooSmall
	}

	var b strings.Builder
	switch m.view {
	case viewOperations:
		// Bound the deck like the help view so a long ops list can't make the inline View
		// taller than the terminal and scroll its top into native scrollback (#1613).
		ops := clampHeight(renderOperations(m.theme, m.dashboard, m.activePanel, domain.NowMS(), w), m.rows-2, m.theme)
		b.WriteString(indentLines(ops, LeftPad))
		b.WriteByte('\n')
		b.WriteString(indentLines(m.theme.Dim().Render("Esc back · ^O home"), LeftPad))
		return b.String()
	case viewHelp:
		// Bound the (now-reachable) help so a long list can't make the inline View taller
		// than the terminal — that would scroll the top into native scrollback (#1613).
		help := clampHeight(renderCommandCellText(m.theme, "Help", commands.HelpTextUI(), w), m.rows-2, m.theme)
		b.WriteString(indentLines(help, LeftPad))
		b.WriteByte('\n')
		b.WriteString(indentLines(m.theme.Dim().Render("Esc back"), LeftPad))
		return b.String()
	}

	// Home band order: the live in-flight turn → StatusLine → Composer.
	// The status/approval + composer form the FIXED bottom band (always whole); the live
	// turn sits above it.
	//
	// CRITICAL (the streaming "output stopped then repeated" bug): the live View must
	// NEVER be taller than the terminal. In inline mode, a View taller than the screen
	// makes the host scroll its top into native scrollback — freezing a PARTIAL copy of
	// the still-streaming turn there while the rest re-renders below (the duplicate). So
	// we render the bottom band whole and show only the TAIL of the in-flight turn that
	// fits above it. The FULL turn commits to scrollback the instant it seals, so nothing
	// is lost — the host then owns scrolling it, exactly as intended.
	bottom := m.bottomBand(w)
	live := m.liveCellsView(w)

	// Content-aware floor for the home view: even at m.rows >= minCockpitRows the bottom band
	// can be tall (a multi-row approval sheet in a ~5-row window), so verify the band itself
	// fits before composing. The minimum footer height is lineCount(bottom) + 1 for the blank
	// separator line; when a turn is in flight it is +2, because the live region is floored to
	// at least one row (budget < 1 → 1, below) plus that separator. If even that minimum
	// overflows, collapse to the one-liner instead of letting View() exceed the terminal.
	needed := lineCount(bottom) + 1
	if live != "" {
		needed = lineCount(bottom) + 2
	}
	if needed > m.rows {
		return tooSmall
	}

	if live != "" {
		// Hard-bound the live region so the footer is NEVER tall. Two reasons: (1) a View
		// taller than the terminal scrolls its own top into native scrollback (a frozen
		// partial copy); (2) even below that, a tea.Println (a flush/commit) fired while the
		// footer is tall makes Bubble Tea's insertAbove dump the whole footer into scrollback
		// off a stale cell-buffer height (charmbracelet/bubbletea#1613). Completed blocks have
		// already flushed to scrollback (flush.go), so the live tail is only the in-flight
		// remainder — capping it to maxLiveRows keeps the View small and the host calm.
		budget := m.rows - lineCount(bottom) - 2 // never exceed the terminal
		if budget > maxLiveRows {
			budget = maxLiveRows
		}
		if budget < 1 {
			budget = 1
		}
		// LeftPad the live region so a STREAMING turn sits at the same left inset as the
		// committed transcript (sealedBlock indents too). Without this the prose jumped one
		// column right the instant it sealed — the live footer was rendered flush-left while
		// scrollback was inset. Indenting here keeps the two states pixel-consistent.
		b.WriteString(indentLines(lastLines(live, budget), LeftPad))
		b.WriteString("\n\n")
	} else {
		// No live turn (idle, or the turn just sealed into scrollback). Still hold one blank
		// line above the bottom band so the status line reads as PART OF THE FOOTER — set off
		// from the committed response above it — instead of glued to the last line of prose.
		b.WriteByte('\n')
	}
	b.WriteString(bottom)

	return strings.TrimRight(b.String(), "\n")
}

// bottomBand renders the FIXED bottom of the live footer (never truncated): the approval
// sheet when a confirmation is pending, else the status line (when it has content), then
// the composer — the input always anchored last.
func (m Model) bottomBand(w int) string {
	var b strings.Builder
	if m.pending != nil {
		b.WriteString(indentLines(renderApproval(m.theme, m.pending, w), LeftPad))
		b.WriteString("\n\n")
	} else if sl := m.statusView(w); sl != "" {
		b.WriteString(indentLines(sl, LeftPad))
		b.WriteString("\n\n")
	}
	// Staged-Ctrl+C cue: a single warning line directly above the composer while the
	// quit is armed, so the confirming second press is discoverable.
	if m.quitArmed {
		b.WriteString(indentLines(m.theme.Warning().Render("Press Ctrl+C again to exit"), LeftPad))
		b.WriteByte('\n')
	}
	b.WriteString(indentLines(m.composerView(w), LeftPad))
	return b.String()
}

// clampHeight keeps the first n lines of s, replacing the last kept line with a dim "…"
// marker when truncated, so a long STATIC view (help / ops deck) can never make the inline
// View taller than the terminal (which would scroll its top into native scrollback, #1613).
func clampHeight(s string, n int, th theme.Theme) string {
	if n < 1 {
		n = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	lines = lines[:n]
	lines[n-1] = th.Dim().Render("… (resize taller for the full list)")
	return strings.Join(lines, "\n")
}

// lineCount is the number of text lines in s (0 for empty). Footer content is pre-
// wrapped to the content width, so one "\n"-delimited line is one display row.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// lastLines keeps the last n lines of s (the tail), dropping the head. Used to bound the
// in-flight turn in the footer so the live View can't outgrow the terminal.
func lastLines(s string, n int) string {
	if n < 1 {
		n = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// liveCellsView renders the transcript cells still LIVE in the footer (the active
// turn + any sealed cell still draining this frame). Sealed cells leave the footer
// the frame their commit acks (committed cursor advances).
func (m Model) liveCellsView(w int) string {
	cw := m.contentW()
	start := m.queue.liveStart(len(m.transcript))
	var parts []string
	for i := start; i < len(m.transcript); i++ {
		cell := m.transcript[i]
		var s string
		switch {
		case cell.Turn != nil:
			// The active turn's already-flushed leading rows live in native scrollback (the
			// incremental row flush — flush.go), so render only the un-flushed TAIL here. As
			// more rows go final and flush, FlushedRows advances and the tail stays short, so
			// the footer never outgrows the terminal. We re-derive the SAME canonical row
			// slice the flush/seal use and join the suffix.
			if cell.Turn.ID == m.activeTurn && cell.Turn.FlushedRows > 0 {
				rows := m.activeTurnRows(cell.Turn)
				if cell.Turn.FlushedRows >= len(rows) {
					continue // everything rendered so far is already in scrollback
				}
				s = strings.Join(rows[cell.Turn.FlushedRows:], "\n")
			} else {
				s = renderTurn(m.theme, m.md, cell.Turn, w, cw, m.expanded, m.spinnerFrame, domain.NowMS())
			}
			if cell.Turn.Queued {
				// A dimmed queued follow-up turn.
				s = m.theme.Dim().Render(s)
			}
		case cell.Note != nil:
			s = renderNoteCell(m.theme, cell.Note, w)
		case cell.Command != nil:
			s = renderCommandCell(m.theme, cell.Command, w)
		}
		if strings.TrimSpace(stripAnsi(s)) != "" {
			parts = append(parts, s)
		}
	}
	return strings.Join(parts, "\n")
}

// composerView renders the composer with the current chrome (busy stage label,
// queue depth, attention flag, context hint).
func (m Model) composerView(w int) string {
	stage := ""
	if m.inFlight {
		if t := m.activeTurnCell(); t != nil {
			stage = runStageLabel(t.Phase)
		}
		if stage == "" {
			stage = "Processing…"
		}
	}
	cancellable := m.inFlight
	return m.composer.View(composer.ViewParams{
		Width:       w,
		Stage:       stage,
		QueueDepth:  len(m.queuedInput),
		Cancellable: &cancellable,
		Attention:   m.attentionN > 0,
		Placeholder: "Ask Daintree…  ·  / for commands",
	})
}

// statusView renders the compact ≤56-cell status rollup (renders "" when idle with
// nothing to report).
func (m Model) statusView(w int) string {
	pct := -1
	if m.hasUsage {
		pct = m.contextPct
	}
	// Surface an agent in the compact strip ONLY when it NEEDS ATTENTION, and by a HUMAN
	// label (its badge + title) — never the raw watcher id (e.g. "DONE wch_fa04bee6", which
	// reads as meaningless noise). A done / quietly-working watcher lives in the operations
	// view; the always-visible strip is for the MCP link + things that need the operator.
	var aLabel, aTone, aGoal string
	if len(m.dashboard.Agents) > 0 {
		a := m.dashboard.Agents[0]
		if a.NeedsAttention {
			aLabel = a.Badge
			aTone = badgeTone(a.Badge)
			aGoal = a.Title
		}
	}
	return renderStatusLine(m.theme, statusParams{
		ContextPct:       pct,
		HasUsage:         m.hasUsage,
		Cost:             m.cost,
		Model:            m.model,
		AttentionN:       m.attentionN,
		TopSeverity:      m.dashboard.topSeverity(),
		Degraded:         m.degraded,
		ModelRateLimited: m.modelRateLimited,
		ActiveTone:       aTone,
		ActiveLabel:      aLabel,
		ActiveGoal:       aGoal,
	}, w)
}

// renderCommandCellText is a tiny helper for the help view (title + body).
func renderCommandCellText(th theme.Theme, title, text string, w int) string {
	c := &CommandCell{Title: title, Text: text}
	return renderCommandCell(th, c, w)
}
