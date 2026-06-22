package ui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// update_handlers.go holds the per-message reducers split out of Update for
// readability: key routing, work serialization, completion, approval, attention,
// resize/redraw, bootstrap, ticks, and the scrollback block factories.

// --- periodic ticks ---

type spinnerTickMsg struct{}

// spinnerTickCmd advances the active-row spinner ~10fps.
func spinnerTickCmd() tea.Cmd {
	return tea.Tick(100*time.Millisecond, func(time.Time) tea.Msg { return spinnerTickMsg{} })
}

// dashboardTickCmd polls the operations deck ~1s.
func dashboardTickCmd() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return DashboardTickMsg{} })
}

// --- key routing (ui-input.md §2) ---

func (m Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C is ALWAYS live (including during splash).
	if isCtrl(k, 'c') {
		return m.onShutdown()
	}
	// #10: do NOT swallow keys during boot. The composer renders before MCP/project
	// resolve, so it must accept input during the splash (the splash is a pure
	// overlay, never an input gate). Only Ctrl+C is special above; everything else
	// falls through to the normal routing (which ends at the focused composer).

	// Approval sheet owns Y/N/V/Esc while up.
	if m.pending != nil {
		return m.onApprovalKey(k)
	}

	// Ctrl+O toggles the operations deck (clearing any active panel filter).
	if isCtrl(k, 'o') {
		if m.view == viewOperations {
			m.view = viewHome
		} else {
			m.activePanel = PanelNone
			m.view = viewOperations
		}
		return m.afterStateChange(nil)
	}
	// Ctrl+X toggles expanded raw-tool detail.
	if isCtrl(k, 'x') {
		m.expanded = !m.expanded
		return m.afterStateChange(nil)
	}
	// Off-home Esc returns home (home-Esc is the composer's — §2).
	if (k.Code == tea.KeyEscape || k.Code == tea.KeyEsc) && m.view != viewHome {
		m.view = viewHome
		m.activePanel = PanelNone
		return m.afterStateChange(nil)
	}

	// Home view: the focused composer gets the key first.
	if m.composerFocus() {
		out := m.composer.Update(k)
		if out.Submit != nil && out.Submit.OK {
			return m.onSubmit(out.Submit.Text)
		}
		if out.Cancel {
			// Esc-empty-while-busy → synchronous Cancelling…, then abort.
			return m.onCancel()
		}
		return m.afterStateChange(nil)
	}
	return m, nil
}

// isCtrl reports whether k is Ctrl+<r> (lowercase rune).
func isCtrl(k tea.KeyPressMsg, r rune) bool {
	return k.Mod&tea.ModCtrl != 0 && (k.Code == r || k.Code == r-32)
}

// --- work serialization (ui-input.md §6.3/§6.4) ---

// onSubmit accepts a non-empty submit: a slash command runs as a command; plain
// text starts a turn (if idle) or queues a visible dimmed follow-up (if busy).
func (m Model) onSubmit(text string) (tea.Model, tea.Cmd) {
	m.composer.AcceptSubmit(text)

	if strings.HasPrefix(text, "/") {
		// Slash command: run off the loop (some hit the model). Keep single-flight
		// independent — a command isn't a model turn.
		return m, m.controller.runCommand(m.ctx, text)
	}

	if m.inFlight {
		// Busy → queue a VISIBLE dimmed follow-up turn, promoted in place later.
		cell := &TurnCell{ID: domain.NewID("turn_"), UserText: text, State: TurnActive, Queued: true, Ts: domain.NowMS()}
		m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
		m.queuedInput = append(m.queuedInput, queuedTurn{prompt: text, cellID: cell.ID})
		return m.afterStateChange(nil)
	}

	return m.startTurn(text)
}

// startTurn creates the active TurnCell and dispatches Session.Send single-flight.
func (m Model) startTurn(text string) (tea.Model, tea.Cmd) {
	cell := &TurnCell{
		ID: domain.NewID("turn_"), UserText: text, State: TurnActive,
		Phase: domain.PhaseReceived, PhaseStartedAt: domain.NowMS(), Ts: domain.NowMS(),
	}
	m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
	m.activeTurn = cell.ID
	m.inFlight = true
	cmd := m.controller.runTurn(m.ctx, cell.ID, text, false)
	return m.afterStateChange(cmd)
}

// promoteQueued promotes an existing queued cell into the active turn IN PLACE
// (issue #95: never create a second entry). It returns the dispatch cmd.
func (m *Model) promoteQueued(q queuedTurn) tea.Cmd {
	for i := range m.transcript {
		if m.transcript[i].Turn != nil && m.transcript[i].Turn.ID == q.cellID {
			c := m.transcript[i].Turn
			c.Queued = false
			c.Phase = domain.PhaseReceived
			c.PhaseStartedAt = domain.NowMS()
			m.activeTurn = c.ID
			break
		}
	}
	m.inFlight = true
	return m.controller.runTurn(m.ctx, q.cellID, q.prompt, false)
}

// onCancel sets phase Cancelling SYNCHRONOUSLY (so the UI never looks frozen) then
// aborts the in-flight turn's context.
func (m Model) onCancel() (tea.Model, tea.Cmd) {
	if !m.inFlight {
		return m.afterStateChange(nil)
	}
	if t := m.activeTurnCell(); t != nil {
		t.Phase = domain.PhaseCancelling
		t.PhaseStartedAt = domain.NowMS()
	}
	m.controller.cancelTurn()
	return m.afterStateChange(nil)
}

// onTurnComplete seals the active turn and drains the next unit of work (FIFO user
// follow-up first, then an autonomous wake). queueDepth is read off len(queuedInput)
// BEFORE the drained item re-enters so it never momentarily reads "1 queued".
func (m Model) onTurnComplete(msg TurnCompleteMsg) (tea.Model, tea.Cmd) {
	// Ordering barrier (#1): only act on a completion for the CURRENTLY active turn.
	// A completion tagged with a stale cell id (the turn was already cleared, e.g. by
	// /clear, or a newer turn was promoted) must never seal/promote the wrong turn.
	if msg.RunID != "" && msg.RunID != m.activeTurn {
		return m.afterStateChange(nil)
	}
	if t := m.activeTurnCell(); t != nil {
		t.sealProse()
		// #8: a surfaced Send failure (e.g. ErrTurnInProgress) seals as a failed turn
		// with a note rather than masquerading as a clean completion.
		if msg.Failed && !isFailureReply(msg.Reply) {
			t.State = TurnFailed
			m.addNote(NoteError, "Turn could not start (a turn was already in progress).")
		} else {
			t.State = terminalTurnState(t.Phase, msg.Reply)
		}
		t.Phase = domain.PhaseComplete
	}
	m.activeTurn = ""
	m.inFlight = false
	return m.drainPending()
}

// onWakeComplete seals the wake turn; on failure re-queue the burst once (#9: the
// "one wake retry" contract — keep the active burst and requeue it exactly once,
// then give up).
func (m Model) onWakeComplete(msg WakeCompleteMsg) (tea.Model, tea.Cmd) {
	if msg.RunID != "" && msg.RunID != m.activeTurn {
		return m.afterStateChange(nil) // stale completion (barrier, #1)
	}
	if t := m.activeTurnCell(); t != nil {
		t.sealProse()
		if msg.Failed {
			t.State = TurnFailed
		} else {
			t.State = TurnComplete
		}
		t.Phase = domain.PhaseComplete
	}
	burst := m.activeWake
	m.activeWake = nil
	m.activeTurn = ""
	m.inFlight = false
	if msg.Failed && !m.wakeRetried && len(burst) > 0 {
		// Re-queue THIS wake burst once so a transient outage isn't stranded. Prepend
		// so the retry runs ahead of any newer burst that arrived meanwhile.
		m.wakeRetried = true
		m.pendingWake = append(append([]domain.QueueEvent{}, burst...), m.pendingWake...)
	}
	return m.drainPending()
}

// drainPending fires the next queued user follow-up (FIFO), else a pending wake.
func (m Model) drainPending() (tea.Model, tea.Cmd) {
	if m.inFlight {
		return m.afterStateChange(nil)
	}
	if len(m.queuedInput) > 0 {
		next := m.queuedInput[0]
		m.queuedInput = m.queuedInput[1:]
		cmd := m.promoteQueued(next)
		return m.afterStateChange(cmd)
	}
	if len(m.pendingWake) > 0 {
		// Build one wake reactor prompt over the pending burst, fed read-only. Keep the
		// burst in activeWake so a failed wake can requeue it once (#9).
		burst := m.pendingWake
		m.pendingWake = nil
		m.activeWake = burst
		prompt := wakePrompt(burst)
		cell := &TurnCell{ID: domain.NewID("turn_"), State: TurnActive, Phase: domain.PhaseReceived, PhaseStartedAt: domain.NowMS(), Ts: domain.NowMS()}
		m.transcript = append(m.transcript, TranscriptCell{Turn: cell})
		m.activeTurn = cell.ID
		m.inFlight = true
		cmd := m.controller.runWake(m.ctx, cell.ID, prompt)
		return m.afterStateChange(cmd)
	}
	m.wakeRetried = false // a settled-idle burst resets the retry budget
	return m.afterStateChange(nil)
}

// terminalTurnState maps the last phase + reply into a sealed turn state.
func terminalTurnState(phase domain.RunPhase, reply string) TurnState {
	if phase == domain.PhaseCancelling || reply == domain.CancelledReply {
		return TurnCancelled
	}
	if isFailureReply(reply) {
		return TurnFailed
	}
	return TurnComplete
}

// wakePrompt builds a read-only wake reactor prompt over a burst of attention events.
func wakePrompt(events []domain.QueueEvent) string {
	var b strings.Builder
	b.WriteString("A background signal needs your review. Inspect and report (read-only):\n")
	for _, e := range events {
		b.WriteString("- " + e.Title)
		if e.Summary != "" {
			b.WriteString(": " + e.Summary)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// --- slash command completion ---

func (m Model) onCommandComplete(msg CommandCompleteMsg) (tea.Model, tea.Cmd) {
	if msg.Quit {
		return m.onShutdown()
	}
	if msg.ClearTranscript {
		return m.onClear(msg.Title, msg.Text)
	}
	// Render the result as a CommandCell in the transcript.
	m.transcript = append(m.transcript, TranscriptCell{Command: &CommandCell{
		ID: domain.NewID("cmd_"), Title: msg.Title, Text: msg.Text, Ts: domain.NowMS(),
	}})
	return m.afterStateChange(nil)
}

// onClear runs /clear: drop the transcript, wipe host scrollback, re-arm the commit
// queue (bump clearNonce → resetKey change), and re-commit the masthead + the fresh
// confirmation card. The host wipe runs as a tea.Cmd AFTER the cleared tree commits.
func (m Model) onClear(title, text string) (tea.Model, tea.Cmd) {
	m.transcript = nil
	m.activeTurn = ""
	m.queuedInput = nil
	m.pendingWake = nil
	m.clearNonce++
	m.queue.applyResetKey(m.clearNonce + m.redrawNonce)
	// The "conversation cleared" confirmation card.
	m.transcript = append(m.transcript, TranscriptCell{Command: &CommandCell{
		ID: domain.NewID("cmd_"), Title: title, Text: text, Ts: domain.NowMS(),
	}})
	// #3: order the host wipe BEFORE the re-commit (tea.Sequence, NOT tea.Batch). With
	// an unordered Batch the host clear could wipe the freshly committed masthead/card,
	// or a stale commit could print after the clear. Sequence runs the wipe to
	// completion, then the commit cmd emits the fresh masthead + card.
	return m, tea.Sequence(hostClearCmd(), m.scheduleCommit())
}

// --- approval (ui-input.md §4) ---

func (m Model) onApprovalRequested(msg ApprovalRequestedMsg) (tea.Model, tea.Cmd) {
	m.pending = &pendingConfirm{req: msg.Request, resolve: msg.Resolve}
	if t := m.activeTurnCell(); t != nil {
		t.Phase = domain.PhaseAwaitingApproval
		t.PhaseStartedAt = domain.NowMS()
	}
	return m.afterStateChange(nil)
}

func (m Model) onApprovalKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.Code == 'y' || k.Code == 'Y':
		return m.resolveApproval(true)
	case k.Code == 'n' || k.Code == 'N' || k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		// Esc DECLINES (it does not just dismiss).
		return m.resolveApproval(false)
	case k.Code == 'v' || k.Code == 'V':
		if m.pending != nil {
			m.pending.showArgs = !m.pending.showArgs
		}
		return m.afterStateChange(nil)
	}
	return m, nil
}

func (m Model) resolveApproval(approved bool) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		return m, nil
	}
	// Route the decision to the blocked runtime goroutine.
	select {
	case m.pending.resolve <- approved:
	default:
	}
	m.pending = nil
	if t := m.activeTurnCell(); t != nil {
		t.Phase = domain.PhaseToolRunning
		t.PhaseStartedAt = domain.NowMS()
	}
	return m.afterStateChange(nil)
}

func (m Model) onApprovalResolved(msg ApprovalResolvedMsg) (tea.Model, tea.Cmd) {
	return m.resolveApproval(msg.Approved)
}

// --- attention (ui-transcript.md §11) ---

func (m Model) onAttention(msg AttentionBatchMsg) (tea.Model, tea.Cmd) {
	if len(msg.Events) == 0 {
		return m, nil
	}
	// Feed the wake queue + bump the attention count.
	m.pendingWake = append(m.pendingWake, msg.Events...)
	m.attentionN = len(m.dashboard.Inbox)
	if m.attentionN == 0 {
		m.attentionN = len(msg.Events)
	}
	// Ring the BEL once per fresh batch (on the event, not a count increment).
	cmd := bellCmd()
	// If idle, the drain fires the wake reactor.
	if !m.inFlight {
		m2, dcmd := m.drainPending()
		return m2, tea.Batch(cmd, dcmd)
	}
	return m.afterStateChange(cmd)
}

// --- resize / redraw (ui-transcript.md §11) ---

func (m Model) onResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.columns = msg.Width
	m.rows = msg.Height
	// THE inline-cockpit resize rule: do NOTHING but record the geometry. We must NOT
	// touch native scrollback on resize. Re-emitting committed content (a "nuclear
	// redraw" — clear scrollback + re-tea.Println everything) is an OpenTUI concept that
	// is actively wrong in Bubble Tea: the HOST terminal reflows prior scrollback itself,
	// and Bubble Tea re-renders the live footer at the new width automatically on this
	// very WindowSizeMsg. Re-committing instead fought BT's renderer and the host reflow —
	// it flashed the screen between the masthead and the footer and dropped the input
	// (the masthead's tea.Println lands at a stale cell-buffer height; repeated SIGWINCH
	// from the embedder turned that into a strobe). The footer just re-flows; that's it.
	return m, nil
}

// resizeRedrawDelay debounces resize drags (TS uses 150ms).
const resizeRedrawDelay = 150 * time.Millisecond

// onRedraw performs the settled-resize "nuclear redraw": re-commit the masthead +
// whole transcript fresh at the NEW width. Bump redrawNonce (folds into resetKey →
// committed=0, headerDone=false), wipe host scrollback, and re-run the commit queue.
// The transcript model is left intact (separate from /clear).
func (m Model) onRedraw(msg RedrawMsg) (tea.Model, tea.Cmd) {
	if msg.Nonce != m.resizePending {
		return m, nil // a newer resize superseded this one
	}
	m.redrawNonce++
	m.queue.applyResetKey(m.clearNonce + m.redrawNonce)
	// A resize changes the View dimensions, so the renderer's cell buffer is sized to
	// the OLD geometry until it re-flushes. Recommitting the masthead immediately would
	// tea.Println it at that stale height and wipe the footer (charmbracelet/bubbletea
	// #1613 — the same bug as the boot hand-off). So: clear the host + reset BT's buffer
	// (tea.ClearScreen), DISARM commits, and re-arm them one cycle out (commitArmCmd) —
	// by which point the footer has re-flushed at the new size and the masthead recommits
	// above a correctly-sized footer. See scheduleCommit.
	m.commitArmed = false
	return m, tea.Sequence(hostClearCmd(), tea.ClearScreen, commitArmCmd())
}

// --- bootstrap / dashboard / boot gate ---

// onMcpResolved records that the MCP connect settled (connected or degraded) — one
// half of the startupSettled gate — and recomputes whether startup has settled. It
// also kicks the authoritative project-name fetch (the third boot gate) and arms the
// 8s boot-cap backstop, both of which can only run once a connect attempt resolved.
func (m Model) onMcpResolved() (tea.Model, tea.Cmd) {
	m.mcpResolved = true
	gate := m.recomputeStartupSettled()
	// Fetch the real project name (gates the splash via projectSettled). The 8s boot-cap
	// backstop is already armed from launch (Init), so it isn't re-armed here.
	return m.afterStateChange(tea.Batch(gate, m.fetchProjectNameCmd()))
}

// recomputeStartupSettled flips startupSettled true once BOTH the MCP connect resolved
// and the first dashboard snapshot landed, then runs the boot gate. Idempotent.
func (m *Model) recomputeStartupSettled() tea.Cmd {
	if m.startupSettled {
		return nil
	}
	if m.mcpResolved && m.bootSnapshotIn {
		m.startupSettled = true
		return m.finishBootIfReady()
	}
	return nil
}

// fetchProjectNameCmd asks Daintree for the authoritative project name off the loop
// (a few bounded retries — right after connect the renderer may not have a project
// bound yet) and ALWAYS reports a ProjectNameMsg so the projectSettled gate closes
// even on a miss / offline link. Non-blocking; the 8s bootCap is the backstop.
func (m Model) fetchProjectNameCmd() tea.Cmd {
	a := m.app
	ctx := m.ctx
	return func() tea.Msg {
		for attempt := 0; attempt < projectNameRetries; attempt++ {
			if !a.MCP.IsConnected() {
				break
			}
			if name := a.MCP.FetchProjectName(ctx); name != "" {
				return ProjectNameMsg{Name: name}
			}
			select {
			case <-ctx.Done():
				return ProjectNameMsg{}
			case <-time.After(projectNameRetryDelay):
			}
		}
		return ProjectNameMsg{} // settle the gate with the provisional name
	}
}

// projectNameRetries / projectNameRetryDelay mirror the original's 4 × 1s fetch loop.
const (
	projectNameRetries    = 4
	projectNameRetryDelay = time.Second
)

// bootCapCmd is the hard safety cap (8000ms — the original's bootCap): if startup
// stalls, drop into the cockpit regardless of gate readiness. A BootCapMsg fires it.
func bootCapCmd() tea.Cmd {
	return tea.Tick(bootCap, func(time.Time) tea.Msg { return BootCapMsg{} })
}

// bootCap matches the original 8000ms backstop.
const bootCap = 8000 * time.Millisecond

// bootstrapCmd runs the async MCP connect + scheduler start off the loop and reports
// MCPConnected/Degraded + wires the attention callback. The boot→cockpit hand-off
// (masthead commit + redraw) is driven by the 3-gate lock, NOT here.
func (m Model) bootstrapCmd() tea.Cmd {
	a := m.app
	ctx := m.ctx
	pump := m.pump
	return func() tea.Msg {
		st := a.ConnectMcp(ctx)
		// Start the foreground daemon; attention events route through the pump's
		// program via a channel the scheduler callback writes (delivered as a
		// pump-adjacent attention batch). We surface them through the App log hook +
		// the AttentionBatch path below.
		a.StartScheduler(ctx, func(events []domain.QueueEvent) {
			pump.sendAttention(events)
		})
		if !st.Connected {
			reason := st.Error
			if reason == "" {
				reason = "no url/token"
			}
			return MCPDegradedMsg{Reason: reason}
		}
		count := 0
		if st.ToolCount != nil {
			count = *st.ToolCount
		}
		return MCPConnectedMsg{Transport: st.Transport, ToolCount: count}
	}
}

// buildDashboardCmd builds an operations snapshot off the loop (DashboardSnapshotMsg).
func (m Model) buildDashboardCmd() tea.Cmd {
	a := m.app
	ctx := m.ctx
	return func() tea.Msg {
		snap := buildDashboard(ctx, a)
		return DashboardSnapshotMsg{Snapshot: snap}
	}
}

// --- shutdown ---

// onShutdown rejects every pending confirm, rewires future confirms to auto-decline,
// cancels the in-flight turn, and quits.
func (m Model) onShutdown() (tea.Model, tea.Cmd) {
	m.quitting = true
	if m.pending != nil {
		select {
		case m.pending.resolve <- false:
		default:
		}
		m.pending = nil
	}
	// Future confirms auto-decline so a dispatch can't block on a dead modal.
	m.app.SetHooks(appAutoDecline())
	m.controller.cancelTurn()
	return m, tea.Quit
}

// --- scrollback block factories (rendered fresh at the current width) ---

// headerBlock renders the masthead block at the current chrome width.
func (m *Model) headerBlock() ScrollbackBlock {
	w := m.chromeW()
	rendered := indentLines(renderMasthead(m.theme, m.masthead, w), LeftPad)
	return ScrollbackBlock{ID: headerID, Kind: BlockMasthead, Rendered: rendered, Plain: stripAnsi(rendered), Width: w}
}

// sealedBlock renders the sealed transcript cell at index i fresh at the current
// width (a resize re-renders it; never reflowed in place).
func (m *Model) sealedBlock(i int) ScrollbackBlock {
	cell := m.transcript[i]
	w := m.chromeW()
	cw := m.contentW()
	var rendered string
	var kind ScrollbackKind
	switch {
	case cell.Turn != nil:
		rendered = renderTurn(m.theme, m.md, cell.Turn, w, cw, m.expanded, m.spinnerFrame, domain.NowMS())
		kind = BlockTurn
	case cell.Note != nil:
		rendered = renderNoteCell(m.theme, cell.Note, w)
		kind = BlockNote
	case cell.Command != nil:
		rendered = renderCommandCell(m.theme, cell.Command, w)
		kind = BlockCommand
	}
	rendered = indentLines(rendered, LeftPad)
	if rendered != "" {
		// Each sealed cell OWNS the single blank line ABOVE it (shared layout rule §3:
		// Transcript/TurnCellView marginTop={1}). Committed via its own tea.Println,
		// the leading "\n" is what separates this turn from the masthead and from the
		// prior turn in native scrollback — without it the history packs together with
		// no breathing room.
		rendered = "\n" + rendered
	}
	return ScrollbackBlock{ID: cell.ID(), Kind: kind, Rendered: rendered, Plain: stripAnsi(rendered), Width: w}
}
