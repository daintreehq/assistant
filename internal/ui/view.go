package ui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/commands"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/ui/composer"
	"github.com/daintreehq/assistant/internal/ui/theme"
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
	s := m.footer()
	// Record the live View's rendered height for scrollbackChunkRows (bubbletea#1613 commit
	// safety). This is the renderer's cell-buffer height as of THIS frame; a commit fired in the
	// next Update uses exactly this height, so reserving it keeps a tea.Println safe even when the
	// footer grew tall to hold a withheld block. footerRows is nil only in headless tests.
	if m.footerRows != nil {
		*m.footerRows = lineCount(s)
	}
	// A raw host wipe erases pixels Bubble Tea still believes are present. Its v2
	// renderer skips a pending ClearScreen when the next View string and bounds match
	// the previous frame, leaving the composer blank until a keypress changes content.
	// redrawHostCmd sequences rendererRepaintMsg after the clear; toggling this
	// zero-cell tag changes string identity without changing width, height, or pixels.
	if m.rendererRepaintTag && s != "" {
		s += "\x1b[0m"
	}
	v.SetContent(s)
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
		// Bound the deck to a SCROLLABLE window of m.rows-2 lines so a long ops list can't
		// make the inline View taller than the terminal (#1613). ↑/↓/PgUp/PgDn move opsScroll
		// (onKey); clampWindow keeps the window height-safe and draws the scroll cues.
		ops := clampWindow(m.deckBody(w), m.opsScroll, m.rows-2, m.theme)
		b.WriteString(indentLines(ops, LeftPad))
		b.WriteByte('\n')
		// truncateCells keeps the cue inside the content width on a narrow pane (full key
		// list lives in the help view); ↑↓ signals the deck scrolls.
		b.WriteString(indentLines(truncateCells(m.theme.Dim().Render("Esc back · ^O home · ↑↓ scroll"), w), LeftPad))
		return b.String()
	case viewHelp:
		// Same scrollable-window treatment as the ops deck so the (now-reachable) help can't
		// overflow the terminal and scroll its top into native scrollback (#1613).
		help := clampWindow(m.deckBody(w), m.helpScroll, m.rows-2, m.theme)
		b.WriteString(indentLines(help, LeftPad))
		b.WriteByte('\n')
		b.WriteString(indentLines(truncateCells(m.theme.Dim().Render("Esc back · ↑↓ scroll"), w), LeftPad))
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
	if !bandFits(bottom, live, m.rows) {
		// A pending approval is BLOCKING a mutating tool call, and the generic notice gives
		// no hint that a decision is waiting behind it. Say so, and say which key is safe:
		// onApprovalKey refuses affirmatives while the sheet is illegible, so declining is
		// the only thing that still works from here.
		if m.pending != nil {
			return indentLines(truncateCells(
				m.theme.Warning().Render("terminal too small to show a pending approval — resize, or Esc declines"), w), LeftPad)
		}
		return tooSmall
	}

	if live != "" {
		// The live region fills the height AVAILABLE above the fixed bottom band. budget =
		// rows - band - 2 keeps the whole footer at rows-1 (one short of the terminal), so a View
		// taller than the terminal — which would scroll its own top into scrollback as a frozen
		// copy — can never happen. There is NO artificial sub-cap: a WITHHELD block (a bullet list,
		// which can't commit incrementally because glamour re-flows it) renders WHOLE here instead
		// of being tail-truncated by lastLines, which is exactly the "few-row window that scrolls
		// and flicks over at the end" churn. The other #1613 hazard — a tea.Println fired while the
		// footer is tall — is handled by scrollbackChunkRows reserving the ACTUAL rendered footer
		// height (m.footerRows, set in View), so the commit chunks to fit above the tall footer.
		// A block taller than the whole terminal still tail-truncates (unavoidable in inline mode).
		budget := liveBudgetFor(m.rows, lineCount(bottom))
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
	// Healthy MCP connectivity is the normal state and stays silent. A degraded
	// link makes the assistant's Daintree tools unusable, so surface a prominent
	// live warning without committing anything into terminal scrollback.
	if degraded := m.mcpDegradedView(w); degraded != "" {
		b.WriteString(indentLines(degraded, LeftPad))
		b.WriteString("\n\n")
	}
	// A pending multiple-choice question REPLACES the composer entirely: the sheet IS the
	// input surface until the user answers or cancels. (An approval and a question can't
	// both be pending — tool dispatch is sequential, so only one tool blocks at a time.)
	if m.pendingQuestion != nil {
		b.WriteString(indentLines(renderQuestion(m.theme, m.pendingQuestion, w), LeftPad))
		return b.String()
	}
	// The sign-in sheet likewise REPLACES the composer: its URL/key fields are the
	// input surface, and a visible composer beside them would be a second caret with
	// no way to tell which one has focus.
	//
	// It yields to a pending APPROVAL (`m.pending == nil` guard), and that guard is
	// load-bearing, not cosmetic. onKey routes to the approval FIRST, so rendering
	// sign-in on top of a live approval would put the user's keystrokes into an
	// invisible sheet — `y` blind-approving a mutating tool while the screen still
	// shows a key prompt. Render priority MUST mirror input priority. The sign-in
	// state survives underneath and reappears once the approval is answered.
	if m.pendingSignIn != nil && m.pending == nil {
		b.WriteString(indentLines(renderSignIn(m.theme, m.pendingSignIn, w), LeftPad))
		return b.String()
	}
	if m.pending != nil {
		b.WriteString(indentLines(renderApproval(m.theme, m.pending, w), LeftPad))
		b.WriteString("\n\n")
	} else if sl := m.statusView(w); sl != "" {
		b.WriteString(indentLines(sl, LeftPad))
		b.WriteString("\n\n")
	}
	// A running slash command's liveness line ("⠋ Compacting conversation… · 3s"):
	// commands run OUTSIDE the turn engine (no TurnCell, no live region entry), so
	// without this the model-backed /compact — two serial backend calls — left the
	// cockpit looking completely idle until its result card appeared.
	if cl := m.commandLiveView(w); cl != "" {
		b.WriteString(indentLines(cl, LeftPad))
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

// bandFits reports whether the fixed bottom band fits the terminal height: its own rows
// plus 1 for the blank separator, or plus 2 when a turn is in flight (the live region is
// floored to at least one row). Shared by footer() — which collapses to a one-liner rather
// than let View() exceed the terminal — and by the approval gate, which must know whether
// the sheet the user would be answering is actually on screen. One definition, because a
// second copy drifting from this one is how a blind approval gets through.
func bandFits(bottom, live string, rows int) bool {
	needed := lineCount(bottom) + 1
	if live != "" {
		needed = lineCount(bottom) + 2
	}
	return needed <= rows
}

// minApprovalCells is the content width below which the approval controls can no longer
// render the core "Y approve / N decline" pair whole (see renderActionRows): 9 + 2 + 11.
// Below it the sheet is not a decision surface, it is a smear.
const minApprovalCells = 22

// approvalLegible reports whether a pending approval is actually READABLE on screen —
// the band fits the height AND the pane is wide enough for the decision pair. When it is
// false the cockpit is showing the too-small notice instead of the sheet, so an affirmative
// keypress would approve a mutating action the user cannot see. Declining stays available;
// that is the fail-closed direction.
func (m Model) approvalLegible() bool {
	if m.pending == nil {
		return false
	}
	if m.rows < minCockpitRows || m.contentW() < minApprovalCells {
		return false
	}
	w := m.contentW()
	return bandFits(m.bottomBand(w), m.liveCellsView(w), m.rows)
}

// commandLiveView renders the in-flight slash command's status line — the same
// spinner + dim-label shape as a turn's renderLiveStatus. An active TURN keeps
// precedence (its own live status already narrates the footer; two spinners would
// read as two concurrent activities). Truncated to w: the footer's height budget
// counts "\n"-delimited rows (lineCount), so an over-wide row that soft-wraps in the
// terminal would silently undercount the band and let View outgrow the terminal
// (the #1613 class).
func (m Model) commandLiveView(w int) string {
	if m.commandsRunning == 0 || m.inFlight {
		return ""
	}
	label := m.commandStage
	if label == "" {
		label = "Running command…"
	}
	g := m.theme.Glyphs
	spin := g.Active
	if len(g.Spinner) > 0 {
		spin = g.Spinner[m.spinnerFrame%len(g.Spinner)]
	}
	line := m.theme.Body().Render(spin) +
		m.theme.Dim().Render(" "+label+elapsedToken(m.commandStartedAt, domain.NowMS()))
	return truncateCells(line, w)
}

func (m Model) mcpDegradedView(w int) string {
	if !m.degraded {
		return ""
	}
	// Name the recovery this cockpit actually has, and separate the causes — because they
	// have different fixes and only one of them is retryable.
	//
	// "Check the MCP server, then restart the assistant" named none of them: it pointed at
	// a server the tester does not run, and reached for the most destructive recovery
	// first. /reconnect fixes a dropped link in place. It CANNOT fix a session Daintree
	// closed or replaced, because that token is dead — only Daintree can inject a fresh
	// one, which it does when the panel is reopened. And it cannot fix an absent endpoint
	// at all, so offline / launched-outside-Daintree must not be told to run it.
	head, body := "Daintree MCP unavailable",
		"Run /reconnect. If Daintree closed or replaced this session, reopen the Assistant panel."
	if m.mcpUnconfigured {
		head, body = "Daintree MCP not configured",
			"Degraded local mode — no terminals, agents or worktrees. Launch the assistant from Daintree to enable them."
	}
	title := m.theme.Warning().Bold(true).Render(
		truncateCells(m.theme.Glyphs.Alert+" "+head, w),
	)
	return title + "\n" + m.theme.Warning().Render(wrapCells(body, w))
}

// deckBody renders the scrollable body for whichever footer deck is active (operations or
// help), at content width w. Shared by footer() (to display) and maxDeckScroll (to bound the
// scroll offset against the REAL rendered line count). Returns "" on the home view.
func (m Model) deckBody(w int) string {
	switch m.view {
	case viewOperations:
		return renderOperations(m.theme, m.dashboard, m.activePanel, domain.NowMS(), w)
	case viewHelp:
		return renderCommandCellText(m.theme, "Help", commands.HelpTextUI(), w)
	}
	return ""
}

// maxDeckScroll is the largest valid top-line offset for the active deck: total rendered
// lines minus the visible window height (m.rows-2), floored at 0. onKey clamps opsScroll /
// helpScroll to this so the last page always fills the window and scroll keys never run off
// the end into dead presses. Measured the SAME way clampWindow splits, so the two agree.
func (m Model) maxDeckScroll() int {
	visible := m.rows - 2
	if visible < 1 {
		visible = 1
	}
	total := len(strings.Split(m.deckBody(m.contentW()), "\n"))
	if max := total - visible; max > 0 {
		return max
	}
	return 0
}

// clampWindow renders a SCROLLABLE window of at most n lines from s, starting at top-line
// `offset`. It replaces the old clampHeight (which kept the first n lines and dead-ended with
// a "resize taller" marker): a long ops/help deck now SCROLLS instead of truncating.
//
// Height invariant (#1613): the result is ALWAYS <= n lines. The "↑ more"/"↓ more" scroll
// cues REPLACE the top/bottom row of the window — they consume a budget row, never add one —
// so the footer can't grow taller than the terminal. offset is clamped to [0, total-n] here
// defensively even though onKey already bounds it.
func clampWindow(s string, offset, n int, th theme.Theme) string {
	if n < 1 {
		n = 1
	}
	lines := strings.Split(s, "\n")
	total := len(lines)
	if total <= n {
		return s // fits whole — no scrolling, no cues
	}
	maxOff := total - n
	if offset < 0 {
		offset = 0
	}
	if offset > maxOff {
		offset = maxOff
	}
	window := append([]string(nil), lines[offset:offset+n]...)
	// Overwrite the boundary rows with scroll cues when content is hidden above/below. This
	// hides one real row at each truncated edge (the cost of a fixed-height viewport); the
	// hidden row scrolls into view as the offset moves, and the first/last lines are always
	// reachable at offset 0 / maxOff where their edge cue is absent. Only do this when the
	// window has a row to spare beyond the two cues (n >= 3): at the 1-2 row floor (rows<=4),
	// cues on both edges would hide EVERY interior line, so show the raw window instead — the
	// "↑↓ scroll" footer hint still signals it scrolls.
	if n >= 3 {
		if offset > 0 {
			window[0] = th.Dim().Render("↑ more")
		}
		if offset < maxOff {
			window[n-1] = th.Dim().Render("↓ more")
		}
	}
	return strings.Join(window, "\n")
}

// lineCount is the number of text lines in s (0 for empty). Footer content is pre-
// wrapped to the content width, so one "\n"-delimited line is one display row.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

// liveBudgetFor is the number of rows the live region may fill: the terminal height minus the
// fixed bottom band (bottomRows) minus a 1-row separator, floored at 1. This is the SINGLE source of
// the live-region height limit — there is no static cap, so a withheld block fills the available
// height (and is never tail-truncated by lastLines until it would exceed the terminal). footer()
// applies it; tests assert the un-committed tail fits it (fits = no churn).
func liveBudgetFor(rows, bottomRows int) int {
	if b := rows - bottomRows - 2; b >= 1 {
		return b
	}
	return 1
}

// liveBudget computes liveBudgetFor for the current model (recomputing the bottom band). For tests
// and callers without the band already in hand.
func (m Model) liveBudget() int {
	return liveBudgetFor(m.rows, lineCount(m.bottomBand(m.contentW())))
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
// turn + any sealed cell not yet claimed by a commit). Large committing cells leave
// at selection time so Println re-pins the already-short footer; short cells stay
// through the render barrier and leave at ack time so the renderer cannot skip the
// post-print repaint when the surrounding footer is otherwise unchanged.
func (m Model) liveCellsView(w int) string {
	cw := m.contentW()
	start := m.queue.liveStart(len(m.transcript))
	var parts []string
	for i := start; i < len(m.transcript); i++ {
		if m.queue.inFlight && m.queue.dropInFlightCell && i == m.queue.inFlightCell {
			continue // large committing cell: print against the already-short footer
		}
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
		case cell.Note != nil:
			s = renderNoteCell(m.theme, cell.Note, w)
		case cell.Command != nil:
			s = renderCommandCell(m.theme, cell.Command, w)
		}
		if strings.TrimSpace(stripAnsi(s)) != "" {
			parts = append(parts, s)
		}
	}
	// Separate live cells with a blank line — the SAME marginTop rule sealedBlock
	// applies when a cell commits to scrollback (update_handlers.go). Without it a
	// message submitted MID-TURN (a queued follow-up cell) renders flush against the
	// in-flight assistant prose, with no gap above its "YOU" card. The active turn's
	// own flushed-tail suffix is parts[0], so it never gets a spurious leading blank.
	return strings.Join(parts, "\n\n")
}

// composerView renders the composer with the current chrome (busy stage label,
// queue depth, attention flag, context hint).
func (m Model) composerView(w int) string {
	stage := ""
	cancelling := false
	if m.inFlight {
		if t := m.activeTurnCell(); t != nil {
			stage = runStageLabel(t.Phase)
			// A turn already tearing down cannot absorb a mid-turn follow-up: the Session
			// drains buffered injections into the NEXT turn instead. The composer needs to
			// know so its submit verb doesn't promise otherwise.
			cancelling = t.Phase == domain.PhaseCancelling
		}
		if stage == "" {
			stage = "Processing…"
		}
	}
	cancellable := m.inFlight
	// The field's PURPOSE changes mid-turn: idle it starts a request, busy it steers the
	// one already running (submitted text is folded into that same turn at the next safe
	// boundary, never queued as a second turn). A static "Ask Daintree…" makes it look
	// like an independent second request box.
	//
	// The slash cue is deliberately NOT repeated here: the hint row below already carries
	// "/ commands" and the palette opens the moment "/" is typed, so duplicating it only
	// makes the input itself noisier.
	placeholder := "Ask Daintree…"
	if m.inFlight {
		placeholder = "Add a follow-up…"
	}
	mcpStatus := composer.MCPConnecting
	if m.mcpResolved {
		if m.degraded {
			mcpStatus = composer.MCPDegraded
		} else {
			mcpStatus = composer.MCPConnected
		}
	}
	return m.composer.View(composer.ViewParams{
		Width:       w,
		Stage:       stage,
		QueueDepth:  m.pendingInject,
		Cancellable: &cancellable,
		Cancelling:  cancelling,
		Attention:   m.attentionN > 0,
		Placeholder: placeholder,
		MCPStatus:   mcpStatus,
		Cost:        m.sessionCostLine(),
	})
}

// sessionCostLine renders the session-spend row above the composer, or "" before
// anything has been billed (a session that has spent nothing has nothing to say).
//
// It reads the REAL ledger rather than a UI-side running sum, because the ledger is the
// only thing that sees the whole bill: utility tasks — summarize, extract,
// watcher-classify — fire from tools and background supervision without ever producing a
// turn event, and a busy session runs dozens of them. It also knows when the total is a
// LOWER BOUND, which a bare float cannot express and which this line must not hide.
func (m *Model) sessionCostLine() string {
	if m.app == nil {
		return ""
	}
	s := m.app.CostLedger.Snapshot()
	if s.Calls == 0 {
		return ""
	}
	// Nothing reported at all (an older backend): stay silent rather than show a "$0.00"
	// that would read as "this has been free".
	if s.Unreported == s.Calls {
		return ""
	}
	// No label: the figure sits at the right edge of the connection row, where a leading
	// "session" would spend cells on something the "$" already implies. `/cost` is the
	// surface that explains it.
	if s.LowerBound {
		return "≥ " + formatCost(s.Observed)
	}
	return formatCost(s.Observed)
}

// statusView renders the compact ≤56-cell status rollup (renders "" when idle with
// nothing to report).
func (m Model) statusView(w int) string {
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
		Cost:             m.cost,
		AttentionN:       m.attentionN,
		TopSeverity:      m.dashboard.topSeverity(),
		Degraded:         false, // rendered by the fixed-height MCP row above
		ModelRateLimited: m.modelRateLimited,
		ActiveTone:       aTone,
		ActiveLabel:      aLabel,
		ActiveGoal:       aGoal,
		AutoApprove:      m.autoApprove,
	}, w)
}

// renderCommandCellText is a tiny helper for the help view (title + body).
func renderCommandCellText(th theme.Theme, title, text string, w int) string {
	c := &CommandCell{Title: title, Text: text}
	return renderCommandCell(th, c, w)
}
