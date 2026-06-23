package ui

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/agent"
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

// --- key routing ---

func (m Model) onKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Ctrl+C is ALWAYS live (including during splash). It is STAGED — never a
	// single-press hard kill: the first press cancels any in-flight turn (or, when
	// idle, just arms) and a second press within quitArmWindow quits (onCtrlC).
	if isCtrl(k, 'c') {
		return m.onCtrlC()
	}
	// Ctrl+D at an EMPTY composer is EOF → quit (readline convention). With text in
	// the buffer it falls through to the composer's forward-delete.
	if isCtrl(k, 'd') && m.pending == nil && m.composerFocus() && m.composer.Value() == "" {
		return m.onShutdown()
	}
	// Any other key disarms a pending "press Ctrl+C again to exit": the second-press
	// window only counts an IMMEDIATE second Ctrl+C.
	if m.quitArmed {
		m.quitArmed = false
	}
	// #10: do NOT swallow keys during boot. The composer renders before MCP/project
	// resolve, so it must accept input during the splash (the splash is a pure
	// overlay, never an input gate). Only Ctrl+C/Ctrl+D are special above; everything
	// else falls through to the normal routing (which ends at the focused composer).

	// Ctrl+L: manual redraw — a recovery key for when the live footer renders corrupted
	// (a resize glitch, a stray control sequence, a race). Bare tea.ClearScreen resets
	// Bubble Tea's internal cell buffer so the NEXT View() repaints every cell fresh.
	// CRITICAL: it does NOT wipe native scrollback — unlike onRedraw (resize) which adds
	// hostClearCmd() to also purge \x1b[3J. So this is a pure footer repaint that leaves
	// the committed transcript untouched. Available in EVERY view (it is a recovery key),
	// so it sits ahead of the approval gate.
	if isCtrl(k, 'l') {
		return m, tea.ClearScreen
	}

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
			m.opsScroll = 0 // a freshly-opened deck starts at the top
		}
		return m.afterStateChange(nil)
	}
	// Ctrl+X toggles expanded raw-tool detail.
	if isCtrl(k, 'x') {
		m.expanded = !m.expanded
		return m.afterStateChange(nil)
	}
	// Off-home Esc returns home (home-Esc is the composer's).
	if (k.Code == tea.KeyEscape || k.Code == tea.KeyEsc) && m.view != viewHome {
		m.view = viewHome
		m.activePanel = PanelNone
		m.opsScroll, m.helpScroll = 0, 0 // leaving a deck clears its scroll
		return m.afterStateChange(nil)
	}

	// Operations / help decks SCROLL instead of truncating. composerFocus is false in these
	// views, so vertical-motion keys are otherwise unused here — route them to the active
	// deck's offset, clamped to its content (maxDeckScroll). Placed before the "?" / composer
	// blocks since those only fire on the home view.
	if m.view == viewOperations || m.view == viewHelp {
		page := m.rows - 3 // a page leaves ~one line of overlap for context
		if page < 1 {
			page = 1
		}
		switch k.Code {
		case tea.KeyUp:
			return m.scrollActiveDeck(-1)
		case tea.KeyDown:
			return m.scrollActiveDeck(1)
		case tea.KeyPgUp:
			return m.scrollActiveDeck(-page)
		case tea.KeyPgDown:
			return m.scrollActiveDeck(page)
		case tea.KeyHome:
			return m.scrollActiveDeck(-1 << 30)
		case tea.KeyEnd:
			return m.scrollActiveDeck(1 << 30)
		}
	}

	// "?" on an EMPTY composer opens the help/keys view (the standard at-empty-prompt help
	// trigger); with text in the buffer it types literally.
	if k.Code == '?' && m.composerFocus() && m.composer.Value() == "" {
		m.view = viewHelp
		m.helpScroll = 0 // a freshly-opened deck starts at the top
		return m.afterStateChange(nil)
	}

	// Home view: the focused composer gets the key first.
	if m.composerFocus() {
		out := m.composer.Update(k)
		if out.Submit != nil && out.Submit.OK {
			return m.onSubmit(out.Submit.Text)
		}
		if out.Cancel {
			// Esc-empty-while-busy: retract the newest queued follow-up if there is one,
			// else cancel the active turn (Ctrl-C always cancels the turn regardless).
			return m.onEscWhileBusy()
		}
		return m.afterStateChange(nil)
	}
	return m, nil
}

// isCtrl reports whether k is Ctrl+<r> (lowercase rune).
func isCtrl(k tea.KeyPressMsg, r rune) bool {
	return k.Mod&tea.ModCtrl != 0 && (k.Code == r || k.Code == r-32)
}

// scrollActiveDeck moves the active deck's scroll offset by delta lines, clamped to the
// deck's valid range (maxDeckScroll), and repaints. A clamped no-op press still repaints —
// harmless and keeps the handler uniform. Only viewOperations / viewHelp call this.
func (m Model) scrollActiveDeck(delta int) (tea.Model, tea.Cmd) {
	max := m.maxDeckScroll()
	if m.view == viewHelp {
		m.helpScroll = clampScroll(m.helpScroll+delta, max)
	} else {
		m.opsScroll = clampScroll(m.opsScroll+delta, max)
	}
	return m.afterStateChange(nil)
}

// clampScroll bounds a scroll offset to [0, max].
func clampScroll(v, max int) int {
	if v < 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// onCtrlC implements the staged Ctrl+C contract (the interrupt-then-
// quit standard — never a single-press hard kill). A second press while the quit is
// armed exits; the first press cancels any in-flight turn (it stays alive otherwise)
// and arms a short "press again to exit" window that lapses via quitArmExpireCmd.
func (m Model) onCtrlC() (tea.Model, tea.Cmd) {
	if m.quitArmed {
		return m.onShutdown()
	}
	if m.pending != nil {
		// A Ctrl-C while an approval sheet is up declines it (fail closed) and dismisses it.
		select {
		case m.pending.resolve <- false:
		default:
		}
		m.pending = nil
	}
	if m.inFlight {
		// Mirror onCancel: set Cancelling… synchronously so the UI never looks frozen,
		// then abort the in-flight turn's context. The cockpit stays alive — exiting
		// needs a deliberate second press.
		if t := m.activeTurnCell(); t != nil {
			t.Phase = domain.PhaseCancelling
			t.PhaseStartedAt = domain.NowMS()
		}
		m.controller.cancelTurn()
	}
	m.quitArmed = true
	m.quitArmGen++
	return m.afterStateChange(quitArmExpireCmd(m.quitArmGen))
}

// quitArmWindow is how long a first Ctrl+C stays "armed" for the confirming second
// press before it lapses back to the normal cancel-first behavior.
const quitArmWindow = 1500 * time.Millisecond

// quitArmExpireCmd fires a QuitArmExpireMsg after the window; gen guards staleness.
func quitArmExpireCmd(gen int) tea.Cmd {
	return tea.Tick(quitArmWindow, func(time.Time) tea.Msg { return QuitArmExpireMsg{Gen: gen} })
}

// --- work serialization ---

// onSubmit accepts a non-empty submit: a slash command runs as a command; plain
// text starts a turn (if idle) or queues a visible dimmed follow-up (if busy).
func (m Model) onSubmit(text string) (tea.Model, tea.Cmd) {
	m.composer.AcceptSubmit(text)

	if strings.HasPrefix(text, "/") {
		// /approvals inspects or clears the session allow-list. That state lives on the UI
		// Model (not the App), so it's handled here rather than through the commands
		// package, which only sees *app.App and couldn't read or mutate this map.
		if title, body, ok := m.handleApprovalsCommand(text); ok {
			return m.onCommandComplete(CommandCompleteMsg{Title: title, Text: body})
		}
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
	now := domain.NowMS()
	cell := &TurnCell{
		ID: domain.NewID("turn_"), UserText: text, State: TurnActive,
		Phase: domain.PhaseReceived, PhaseStartedAt: now, Ts: now,
		StartedAt: now, LastActivityAt: now,
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
			now := domain.NowMS()
			c.Queued = false
			c.Phase = domain.PhaseReceived
			c.PhaseStartedAt = now
			c.StartedAt = now // cumulative elapsed counts from when it STARTS, not when queued
			c.LastActivityAt = now
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

// onEscWhileBusy handles Esc on an empty composer while a turn runs. With queued
// follow-ups it retracts the MOST RECENT one back into the composer (so a queued thought
// can be edited or dropped before it fires); with none it cancels the active turn. The
// active turn can always be cancelled with Ctrl-C regardless of the queue, so Esc owning
// the retract here doesn't strand the user.
func (m Model) onEscWhileBusy() (tea.Model, tea.Cmd) {
	if len(m.queuedInput) > 0 {
		return m.retractLastQueued()
	}
	return m.onCancel()
}

// retractLastQueued pops the newest queued follow-up: drop its dimmed cell and pull its
// text back into the composer for editing or removal.
func (m Model) retractLastQueued() (tea.Model, tea.Cmd) {
	n := len(m.queuedInput)
	q := m.queuedInput[n-1]
	m.queuedInput = m.queuedInput[:n-1]
	m.removeTurnCell(q.cellID)
	m.composer.Restore(q.prompt)
	return m.afterStateChange(nil)
}

// removeTurnCell drops the transcript cell whose turn id matches (a still-live queued
// cell — never a committed one). A no-op if not found.
func (m *Model) removeTurnCell(id string) {
	for i := range m.transcript {
		if m.transcript[i].Turn != nil && m.transcript[i].Turn.ID == id {
			m.transcript = append(m.transcript[:i], m.transcript[i+1:]...)
			return
		}
	}
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
		// Durable cancelled marker: the turn's committed header is already flushed and
		// can't be re-badged, so a standalone note records the cancellation in scrollback —
		// otherwise a cancelled turn reads identically to a completed one.
		if t.State == TurnCancelled {
			// Re-stamp any announced-but-unresolved tool rows (the agent emits no UI
			// ToolResult for the calls it abandons) so they don't freeze as ◦ queued / ◌
			// active in scrollback, falsely implying they're still running.
			t.cancelPending()
			m.addNote(NoteInfo, "Turn cancelled.")
		}
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
	// A wake "failed" when the turn flagged it OR the reply is a model/tool failure
	// SENTINEL. Send returns those as plain strings (nil error), and the cockpit's
	// isFailureReply only catches the turn-level sentinels — NOT the wake sentinels
	// ("Model unavailable:", "Model error:", …). Use agent.IsWakeFailureReply (the
	// same predicate the host uses) so a transient model outage never gets recorded as
	// a real summary and never permanently downgrades the terminal's later events.
	wakeFailed := msg.Failed || agent.IsWakeFailureReply(msg.Reply)
	if t := m.activeTurnCell(); t != nil {
		t.sealProse()
		if wakeFailed {
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
	if wakeFailed && !m.wakeRetried && len(burst) > 0 {
		// Re-queue THIS wake burst once so a transient outage isn't stranded. Prepend
		// so the retry runs ahead of any newer burst that arrived meanwhile.
		m.wakeRetried = true
		m.pendingWake = append(append([]domain.QueueEvent{}, burst...), m.pendingWake...)
	} else if !wakeFailed {
		// Record the burst's terminals as summarized ONLY on a REAL reply (mirrors the
		// host) — a failed wake must not permanently downgrade later lifecycle events
		// to one-line acks. A successful retry path lands here too.
		if m.summarizedTerminals == nil {
			m.summarizedTerminals = map[string]struct{}{}
		}
		for _, e := range burst {
			if e.Target != nil && e.Target.TerminalID != "" {
				m.summarizedTerminals[e.Target.TerminalID] = struct{}{}
			}
		}
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
		// Use the shared wake-reactor prompt (cross-burst dedup + the read-only
		// "NOT typed by the user" framing) so the cockpit and the host react
		// identically and a terminal already summarized this session is downgraded
		// to a one-line ack instead of being re-summarized.
		prompt := agent.BuildWakePrompt(burst, m.summarizedTerminals)
		wnow := domain.NowMS()
		cell := &TurnCell{ID: domain.NewID("turn_"), State: TurnActive, Phase: domain.PhaseReceived, PhaseStartedAt: wnow, Ts: wnow, StartedAt: wnow, LastActivityAt: wnow}
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

// --- slash command completion ---

func (m Model) onCommandComplete(msg CommandCompleteMsg) (tea.Model, tea.Cmd) {
	if msg.Quit {
		return m.onShutdown()
	}
	if msg.ClearTranscript {
		return m.onClear(msg.Title, msg.Text)
	}
	// A panel-switching command (/help, /inbox, /watchers, /timers, /audit) switches the
	// live view in place rather than printing a card — the view itself renders the content.
	if msg.SwitchPanel != PanelNone {
		if msg.SwitchPanel == PanelHelp {
			m.view = viewHelp
			m.helpScroll = 0 // a freshly-opened deck starts at the top (mirrors the ?/^O entry paths)
		} else {
			m.view = viewOperations
			m.activePanel = msg.SwitchPanel
			m.opsScroll = 0
		}
		return m.afterStateChange(nil)
	}
	// Otherwise render the result as a CommandCell in the transcript.
	m.transcript = append(m.transcript, TranscriptCell{Command: &CommandCell{
		ID: domain.NewID("cmd_"), Title: msg.Title, Text: msg.Text, Ts: domain.NowMS(),
	}})
	return m.afterStateChange(nil)
}

// onClear runs /clear: drop the transcript, wipe host scrollback, re-arm the commit
// queue (bump clearNonce → resetKey change), and re-commit the masthead + the fresh
// confirmation card. The host wipe runs as a tea.Cmd AFTER the cleared tree commits.
func (m Model) onClear(title, text string) (tea.Model, tea.Cmd) {
	// A slash command runs regardless of single-flight (onSubmit), so /clear can land while
	// a turn is in flight. Abort that turn's runtime context and DROP the single-flight lock
	// here: otherwise its now-stale completion hits the RunID barrier in onTurnComplete and
	// returns WITHOUT clearing inFlight, leaving the cockpit permanently "busy" and queued
	// follow-ups stranded (drainPending only runs while inFlight is false).
	if m.inFlight {
		m.controller.cancelTurn()
		m.inFlight = false
	}
	m.transcript = nil
	m.activeTurn = ""
	m.queuedInput = nil
	m.pendingWake = nil
	// The handler already resolved every open inbox event (ClearInbox), so drop the
	// live attention badge to 0 immediately rather than waiting for the next dashboard
	// tick to recompute it from the now-empty inbox.
	m.attentionN = 0
	m.dashboard.Inbox = nil
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

// --- approval ---

func (m Model) onApprovalRequested(msg ApprovalRequestedMsg) (tea.Model, tea.Cmd) {
	// Session allow-list: a tool the user previously chose to "approve & don't ask again"
	// (and whose risk is eligible to remember) is auto-approved without surfacing the
	// sheet at all. The dispatch layer still audits the call. The stored value is a
	// remaining-count, so check the sentinel explicitly (count > 0 OR the forever
	// sentinel) rather than a bare != 0 — a stale negative other than -1 must not grant.
	name := msg.Request.ToolName
	if rememberable(msg.Request.Risk) {
		if count := m.approvedTools[name]; count == allowForeverCount || count > 0 {
			// Bounded grants decrement and drop at zero so the sheet re-surfaces; the
			// forever sentinel (-1) is left untouched.
			if count == 1 {
				delete(m.approvedTools, name)
			} else if count > 0 {
				m.approvedTools[name] = count - 1
			}
			select {
			case msg.Resolve <- true:
			default:
			}
			return m.afterStateChange(nil)
		}
	}
	m.pending = &pendingConfirm{
		req:     msg.Request,
		resolve: msg.Resolve,
		shownAt: domain.NowMS(),
		// The typed-confirm requirement is decided once at the safety gate and carried
		// on the request, so the cockpit and the classic REPL gate the same action with
		// identical friction (no per-surface re-derivation).
		requireType: msg.Request.NeedsTypedConfirm,
	}
	if t := m.activeTurnCell(); t != nil {
		t.Phase = domain.PhaseAwaitingApproval
		t.PhaseStartedAt = domain.NowMS()
	}
	return m.afterStateChange(nil)
}

func (m Model) onApprovalKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	if m.pending == nil {
		return m, nil
	}
	// Typed-confirmation mode (system / git history-rewrite): single-key approval is
	// disabled; the user types confirmPhrase + Enter. Esc still declines.
	if m.pending.requireType {
		return m.onTypedApprovalKey(k)
	}

	switch {
	case k.Code == 'y' || k.Code == 'Y':
		return m.approveAfterDebounce(0)
	case k.Code == 'a' || k.Code == 'A':
		// Approve AND remember this tool for a BOUNDED number of further calls (eligible
		// risks only). A small bound keeps a forgotten A press from being a standing grant.
		// The rememberable guard here (and on F) is forward-defensive: today the only
		// non-rememberable classes (git/system) already route to typed-confirm above and
		// never reach this switch, but a future non-typed-confirm class would still be
		// kept off the allow-list.
		if !rememberable(m.pending.req.Risk) {
			return m, bellCmd()
		}
		return m.approveAfterDebounce(approveDefaultCount)
	case k.Code == 'f' || k.Code == 'F':
		// Approve AND remember this tool for the WHOLE session, unbounded (eligible risks
		// only) — the pre-bounded-A behavior, now an explicit opt-in.
		if !rememberable(m.pending.req.Risk) {
			return m, bellCmd()
		}
		return m.approveAfterDebounce(allowForeverCount)
	case k.Code == 'n' || k.Code == 'N' || k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		// Decline — the safe default, live immediately (no debounce).
		return m.resolveApproval(false)
	case k.Code == tea.KeyEnter || k.Code == tea.KeyKpEnter:
		// Enter triggers the visual DEFAULT (decline) rather than being silently swallowed.
		return m.resolveApproval(false)
	case k.Code == 'v' || k.Code == 'V':
		m.pending.showArgs = !m.pending.showArgs
		return m.afterStateChange(nil)
	}
	// Any other key: acknowledge with the bell rather than swallowing it silently.
	return m, bellCmd()
}

// approveAfterDebounce approves the pending action, but ignores the affirmative (with a
// bell) inside the debounce window so a typed-ahead / buffered key can't fire an action
// the user never read. grantCount seeds the session allow-list: 0 = approve once (Y),
// approveDefaultCount = bounded "auto-approve N more" (A), allowForeverCount = forever
// this session (F). A non-zero grant is recorded only for rememberable risks, and is
// announced once in scrollback so the grant is visible the moment it happens.
func (m Model) approveAfterDebounce(grantCount int) (tea.Model, tea.Cmd) {
	if domain.NowMS()-m.pending.shownAt < approveDebounceMs {
		return m, bellCmd()
	}
	if grantCount != 0 && rememberable(m.pending.req.Risk) {
		if m.approvedTools == nil {
			m.approvedTools = map[string]int{}
		}
		name := m.pending.req.ToolName
		m.approvedTools[name] = grantCount
		m.addNote(NoteInfo, approvalGrantNote(name, grantCount))
	}
	return m.resolveApproval(true)
}

// approvalGrantNote describes a freshly granted session approval for the scrollback, so
// pressing A/F is announced at the moment it happens rather than only being discoverable
// later via /approvals.
func approvalGrantNote(tool string, count int) string {
	if count == allowForeverCount {
		return "Approved " + tool + " for the rest of this session (/approvals to review)."
	}
	return fmt.Sprintf("Approved %s for %d more call(s) this session (/approvals to review).", tool, count)
}

// handleApprovalsCommand intercepts /approvals and /approvals clear at the UI layer. It
// returns the result card's (title, body) and ok=true when the line is an /approvals
// command; ok=false means some other slash command that should fall through to the normal
// dispatch. The session allow-list lives on the Model, so this can't route through the
// commands package (which only receives *app.App). Pointer receiver: /approvals clear
// mutates the map.
func (m *Model) handleApprovalsCommand(text string) (title, body string, ok bool) {
	fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(text, "/")))
	if len(fields) == 0 || !strings.EqualFold(fields[0], "approvals") {
		return "", "", false
	}
	if len(fields) > 1 {
		if strings.EqualFold(fields[1], "clear") {
			n := len(m.approvedTools)
			m.approvedTools = nil
			if n == 0 {
				return "Approvals", "No session approvals to clear.", true
			}
			return "Approvals", fmt.Sprintf("Cleared %d session approval(s).", n), true
		}
		// Unknown subcommand — report usage rather than silently listing.
		return "Approvals", "Usage: /approvals [clear]", true
	}
	return "Approvals", approvalsListText(m.approvedTools), true
}

// approvalsListText renders the active session allow-list with each tool's remaining
// auto-approvals ("forever this session" for the F sentinel), sorted for a stable view.
func approvalsListText(approved map[string]int) string {
	if len(approved) == 0 {
		return "No active session approvals. Press A (bounded) or F (forever this session) on an approval prompt to add one."
	}
	names := make([]string, 0, len(approved))
	for name := range approved {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	for _, name := range names {
		detail := fmt.Sprintf("%d more", approved[name])
		if approved[name] == allowForeverCount {
			detail = "forever this session"
		}
		b.WriteString(fmt.Sprintf("%-26s %s\n", name, detail))
	}
	b.WriteString("\n/approvals clear resets all.")
	return b.String()
}

// onTypedApprovalKey drives the typed-confirmation sheet for the highest-risk actions.
// Esc declines; Enter approves only when the typed phrase matches; Backspace edits; any
// other printable rune builds the phrase ('n' is a valid letter, so only Esc declines).
func (m Model) onTypedApprovalKey(k tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case k.Code == tea.KeyEscape || k.Code == tea.KeyEsc:
		return m.resolveApproval(false)
	case k.Code == tea.KeyEnter || k.Code == tea.KeyKpEnter:
		if strings.EqualFold(strings.TrimSpace(m.pending.confirmInput), confirmPhrase) {
			return m.resolveApproval(true)
		}
		return m, bellCmd() // phrase not yet matched
	case k.Code == tea.KeyBackspace:
		if r := []rune(m.pending.confirmInput); len(r) > 0 {
			m.pending.confirmInput = string(r[:len(r)-1])
		}
		return m.afterStateChange(nil)
	}
	if k.Text != "" && isPrintableText(k.Text) {
		m.pending.confirmInput += k.Text
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

// approveDebounceMs is the window after the approval sheet appears during which the
// affirmative is ignored (rung back with a bell), so a typed-ahead / buffered key can't
// auto-approve an action the user never read.
const approveDebounceMs = 300

// approveDefaultCount is how many further calls pressing A auto-approves for the same tool
// before the sheet re-surfaces. A small bound (vs. the unbounded F) keeps a forgotten A
// press from becoming a standing grant — mirroring the use-count-bounded automation grants.
const approveDefaultCount = 5

// allowForeverCount is the sentinel stored for F ("allow for the whole session"): an
// unbounded grant that auto-approves and never decrements. 0/missing means "ask".
const allowForeverCount = -1

// confirmPhrase is the word a user must type to approve a typed-confirmation action.
const confirmPhrase = "confirm"

// rememberable reports whether a risk class may be added to the session "don't ask
// again" allow-list. The highest-risk classes (git, system) are always re-confirmed.
func rememberable(r domain.RiskClass) bool {
	switch r {
	case domain.RiskGit, domain.RiskSystem:
		return false
	}
	return true
}

// isPrintableText reports whether s is safe printable text to append to an input field
// (no control runes), so a raw escape sequence can't leak into the confirm phrase.
func isPrintableText(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// --- attention ---

// attentionSeverityGlyph mirrors the /inbox severity glyphs (queue.severityIcon) so a
// transcript attention note reads the same as the inbox digest. We deliberately do NOT
// import internal/queue — internal/ui only depends on domain, and the 7-glyph set is
// tiny and stable, so a local switch is cheaper than a new package edge. Unknown
// severities fall back to the info glyph, matching queue.Format's ELSE-1 fallback.
func attentionSeverityGlyph(sev domain.Severity) string {
	switch sev {
	case domain.SeverityDebug:
		return "·"
	case domain.SeverityDone:
		return "✓"
	case domain.SeverityAttention:
		return "!"
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

// severityToNoteLevel tones the transcript note's spine by severity: the loudest classes
// render danger-red, "attention" warns, "done" reads success, info/debug stay neutral.
// The precise per-severity glyph still lives in the note TEXT (attentionSeverityGlyph) so
// the line matches /inbox; this only colors the spine for at-a-glance triage.
func severityToNoteLevel(sev domain.Severity) NoteLevel {
	switch sev {
	case domain.SeverityUrgent, domain.SeverityBlocked, domain.SeverityError:
		return NoteError
	case domain.SeverityAttention:
		return NoteWarn
	case domain.SeverityDone:
		return NoteSuccess
	default: // info, debug, unknown
		return NoteInfo
	}
}

// attentionNoteText builds the glanceable one-liner echoed into the transcript when a
// sub-thread routes attention: "<glyph> <Title> — [term <id>]/[wt <id>] (×N)". The target
// and coalesce-count suffixes mirror queue.Format's logic (terminal wins over worktree;
// "×N" only when the event coalesced) so the line reads like the /inbox digest — the one
// formatting difference is the " — " separator before the target, which reads better inline
// than the digest's bare space. Both suffixes drop out when absent.
func attentionNoteText(e domain.QueueEvent) string {
	glyph := attentionSeverityGlyph(e.Severity)
	target := ""
	if e.Target != nil {
		if e.Target.TerminalID != "" {
			target = " — [term " + e.Target.TerminalID + "]"
		} else if e.Target.WorktreeID != "" {
			target = " — [wt " + e.Target.WorktreeID + "]"
		}
	}
	dup := ""
	if e.Count > 1 {
		dup = fmt.Sprintf(" (×%d)", e.Count)
	}
	return glyph + " " + e.Title + target + dup
}

func (m Model) onAttention(msg AttentionBatchMsg) (tea.Model, tea.Cmd) {
	if len(msg.Events) == 0 {
		return m, nil
	}
	// Feed the wake queue + optimistically bump the badge for the sub-tick window before the
	// next dashboard snapshot recomputes the AUTHORITATIVE count (which also decrements as
	// items resolve — DashboardSnapshotMsg). onAttention no longer OWNS the count.
	m.pendingWake = append(m.pendingWake, msg.Events...)
	m.attentionN += len(msg.Events)
	// Echo each fresh event into the transcript as a committed note — the durable,
	// scroll-back-able ledger line #175 asks for, glanceable where the operator is already
	// looking instead of only a BEL + badge bump. We emit one note per event with NO
	// UI-side dedupe: the scheduler delivers each event once per MATERIAL change (notify()
	// pulls Digest{NotifiedIsNull} then MarkNotified, and the queue re-arms NotifiedAt only
	// on a real severity/title/summary change — daemon/scheduler.go, queue.go). So a repeat
	// is always a genuine escalation worth a fresh line, never spam. Appended BEFORE
	// drainPending so any wake turn seals AFTER these notes (true chronological order).
	for _, e := range msg.Events {
		m.addNote(severityToNoteLevel(e.Severity), attentionNoteText(e))
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

// --- resize / redraw ---

func (m Model) onResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.columns = msg.Width
	m.rows = msg.Height
	// The FIRST size just establishes geometry (nothing is committed yet at boot, so there
	// is nothing to redraw). Every LATER resize schedules a DEBOUNCED NUCLEAR REDRAW
	// (onRedraw): wipe the host + re-commit the masthead and the whole transcript fresh at
	// the new width, then repaint the sticky footer. Without it, Bubble Tea's in-place
	// repaint strands stale footer-rule fragments across the screen on a resize. The 150ms
	// debounce coalesces a SIGWINCH drag-storm into a single redraw.
	if !m.sizedOnce {
		m.sizedOnce = true
		return m, nil
	}
	m.resizePending++
	nonce := m.resizePending
	return m, tea.Tick(resizeRedrawDelay, func(time.Time) tea.Msg { return RedrawMsg{Nonce: nonce} })
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
	m.resetFlushState()
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
// It captures the preview-throttle state from the Model and reads the clock INSIDE the
// closure (off the loop), so Update never touches time; the result carries the new
// fetch timestamp + tails back so Update can advance that state on the next snapshot.
func (m Model) buildDashboardCmd() tea.Cmd {
	a := m.app
	ctx := m.ctx
	lastFetched := m.lastPreviewFetchedAt
	cached := m.previewCache
	return func() tea.Msg {
		res := buildDashboard(ctx, a, dashboardBuildOptions{
			MCP:                  a.DaemonMCP(),
			NowMS:                domain.NowMS(),
			LastPreviewFetchedAt: lastFetched,
			CachedPreviews:       cached,
		})
		return DashboardSnapshotMsg{Snapshot: res.Dashboard, Previews: res.Previews, FetchedAt: res.FetchedAt}
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
	// Future confirms auto-decline so a dispatch can't block on a dead modal. Teardown
	// must never panic, so guard the wiring (the headless harness has no app/controller).
	if m.app != nil {
		m.app.SetHooks(appAutoDecline())
	}
	if m.controller != nil {
		m.controller.cancelTurn()
	}
	return m, tea.Quit
}

// --- scrollback block factories (rendered fresh at the current width) ---

// headerBlock renders the masthead block at the current chrome width.
func (m *Model) headerBlock() ScrollbackBlock {
	w := m.chromeW()
	rendered := indentLines(renderMasthead(m.theme, m.masthead, w), LeftPad)
	if rendered != "" {
		// The masthead owns the first transcript spacer. That keeps the row below the
		// rule/logging badge present from frame one, before the first note/turn commits.
		rendered += "\n"
	}
	return ScrollbackBlock{ID: headerID, Kind: BlockMasthead, Rendered: rendered, Plain: stripAnsi(rendered), Width: w}
}

// sealedBlock renders the sealed transcript cell at index i fresh at the current
// width (a resize re-renders it; never reflowed in place).
func (m *Model) sealedBlock(i int) ScrollbackBlock {
	cell := m.transcript[i]
	w := m.chromeW()
	cw := m.contentW()

	// A turn whose leading rows were already incrementally flushed to scrollback (the
	// streaming-dup fix — flush.go) must commit ONLY the tail not yet in scrollback; the
	// flushed prefix is already there, so re-committing it would print it a SECOND time
	// (the very duplication we are fixing). The turn is now SEALED (prose re-renders as
	// final markdown), so we strip the EXACT flushed text off the front and commit the
	// remainder. No leading "\n": the tail continues the flushed prefix.
	if cell.Turn != nil && cell.Turn.FlushedRows > 0 {
		tail := sealTail(m.activeTurnRows(cell.Turn), cell.Turn.flushedRowsText)
		if tail == "" {
			// The whole turn already streamed into scrollback — nothing left to commit.
			// Emit an empty block (commitCmd's Println of "" just advances the cursor).
			return ScrollbackBlock{ID: cell.ID(), Kind: BlockTurn, Rendered: "", Plain: "", Width: w}
		}
		tail = indentLines(tail, LeftPad)
		return ScrollbackBlock{ID: cell.ID(), Kind: BlockTurn, Rendered: tail, Plain: stripAnsi(tail), Width: w}
	}

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
	if rendered != "" && i > 0 {
		// Each sealed cell OWNS the single blank line ABOVE it (shared layout rule:
		// a marginTop of one blank line). Cell 0 is the exception: the masthead block
		// owns that first spacer so it is visible from the initial hand-off frame and
		// does not pop in when the first note/turn commits.
		rendered = "\n" + rendered
	}
	return ScrollbackBlock{ID: cell.ID(), Kind: kind, Rendered: rendered, Plain: stripAnsi(rendered), Width: w}
}
