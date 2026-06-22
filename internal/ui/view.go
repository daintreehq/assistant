package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// view.go renders ONLY the live footer (ui-transcript.md §1/§5/§6): the active turn
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

// windowTitle mirrors the unresolved attention count (ui-transcript.md §11):
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

	var b strings.Builder
	switch m.view {
	case viewOperations:
		b.WriteString(indentLines(renderOperations(m.theme, m.dashboard, m.activePanel, domain.NowMS(), w), LeftPad))
		b.WriteByte('\n')
		b.WriteString(indentLines(m.theme.Dim().Render("Esc back · ^O home"), LeftPad))
		return b.String()
	case viewHelp:
		b.WriteString(indentLines(renderCommandCellText(m.theme, "Help", commands.HelpTextUI(), w), LeftPad))
		b.WriteByte('\n')
		b.WriteString(indentLines(m.theme.Dim().Render("Esc back"), LeftPad))
		return b.String()
	}

	// Home (ControlRoom.tsx band order): live cells → StatusLine → Composer, with a
	// blank line of breathing room between bands. The composer is ALWAYS last — it is
	// the input the human types into, anchored at the bottom of the live footer. The
	// status band sits ABOVE it (StatusLine.tsx marginTop={1} + the height={1} spacer);
	// the approval sheet replaces the status band when a confirmation is pending.
	live := m.liveCellsView(w)
	if live != "" {
		b.WriteString(live)
	}

	switch {
	case m.pending != nil:
		if live != "" {
			b.WriteString("\n\n")
		}
		b.WriteString(indentLines(renderApproval(m.theme, m.pending.req, m.pending.showArgs, w), LeftPad))
		b.WriteString("\n\n")
	default:
		if sl := m.statusView(w); sl != "" {
			if live != "" {
				b.WriteString("\n\n")
			}
			b.WriteString(indentLines(sl, LeftPad))
			b.WriteString("\n\n")
		} else if live != "" {
			// A live turn sits above the composer with no status band — keep one blank.
			b.WriteString("\n\n")
		}
	}

	b.WriteString(indentLines(m.composerView(w), LeftPad))

	return strings.TrimRight(b.String(), "\n")
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
			s = renderTurn(m.theme, m.md, cell.Turn, w, cw, m.expanded, m.spinnerFrame, domain.NowMS())
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
		Placeholder: "Ask Daintree to supervise, delegate, or inspect…",
	})
}

// statusView renders the compact ≤56-cell status rollup (renders "" when idle with
// nothing to report).
func (m Model) statusView(w int) string {
	pct := -1
	if m.hasUsage {
		pct = m.contextPct
	}
	active := ""
	if len(m.dashboard.Agents) > 0 {
		a := m.dashboard.Agents[0]
		active = a.Badge + " " + a.ID
	}
	return renderStatusLine(m.theme, statusParams{
		ContextPct:  pct,
		HasUsage:    m.hasUsage,
		Cost:        m.cost,
		Model:       m.model,
		AttentionN:  m.attentionN,
		TopSeverity: m.dashboard.topSeverity(),
		Agents:      len(m.dashboard.Agents),
		Degraded:    m.degraded,
		ActiveAgent: active,
	}, w)
}

// renderCommandCellText is a tiny helper for the help view (title + body).
func renderCommandCellText(th theme.Theme, title, text string, w int) string {
	c := &CommandCell{Title: title, Text: text}
	return renderCommandCell(th, c, w)
}
