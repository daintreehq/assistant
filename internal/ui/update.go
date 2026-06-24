package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// update.go is the single reducer (NEVER mutate the model outside
// Update). It folds runtime/pump msgs + key events into the transcript, drives the
// explicit RunPhase, serializes work (single-flight + FIFO queue + wake priority),
// and schedules scrollback commits.

// Init kicks the event pump, the dashboard/spinner ticks, and the one-shot bootstrap
// (async MCP connect + scheduler). The splash already played BEFORE the program
// started (boot_splash.go), so there is no in-program splash tick or boot-cap — the
// cockpit is live immediately. The composer is interactive from frame one.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.pump.waitEvent(),
		func() tea.Msg { return BootstrapMsg{} },
		dashboardTickCmd(),
		// The spinner tick is NOT armed at boot — it starts on the first in-flight turn
		// (afterStateChange) and lapses when idle, so an idle cockpit doesn't repaint ~10×/s.
		// Arm the first scrollback commit one render cycle in, so Bubble Tea has flushed
		// the SHORT footer (and sized its cell buffer to it) before the masthead's first
		// tea.Println — BT defers cellbuf resize to the flush, so an immediate Println
		// would read a stale height (charmbracelet/bubbletea#1613). See scheduleCommit.
		commitArmCmd(),
	)
}

// Update reduces one message.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		return m.onResize(msg)

	case tea.KeyPressMsg:
		return m.onKey(msg)

	case tea.PasteMsg:
		// Bracketed paste only reaches the composer when it owns keys.
		if m.composerFocus() {
			m.composer.Update(msg)
		}
		return m, nil

	case AgentEventMsg:
		// Attention bursts route through the full onAttention reducer (BEL + wake
		// drain); everything else folds into the active turn. Always RE-ARM the pump.
		rearm := m.pump.waitEvent()
		if msg.Event.kind == pumpAttention {
			next, cmd := m.onAttention(AttentionBatchMsg{Events: msg.Event.attention})
			return next, tea.Batch(cmd, rearm)
		}
		// Completion arrives IN-STREAM (#1): every prior event for the turn has already
		// drained, so clearing/promoting activeTurn here can never strand a final token
		// or apply a stale event to the next turn. The runID barrier guards the case
		// where the active turn already moved on.
		if msg.Event.kind == pumpComplete {
			c := msg.Event.completion
			var next tea.Model
			var cmd tea.Cmd
			if c.wake {
				next, cmd = m.onWakeComplete(WakeCompleteMsg{RunID: c.runID, Reply: c.reply, Failed: c.failed})
			} else {
				next, cmd = m.onTurnComplete(TurnCompleteMsg{RunID: c.runID, Reply: c.reply, Failed: c.failed})
			}
			return next, tea.Batch(cmd, rearm)
		}
		cmd := m.applyPumpEvent(msg.Event)
		next, tail := m.afterStateChange(cmd)
		return next, tea.Batch(tail, rearm)

	case BootstrapMsg:
		return m, m.bootstrapCmd()

	case MCPConnectedMsg:
		m.degraded = false
		// Surface the link as a TOP status note (green "● Connected to Daintree MCP"), not a
		// permanent footer segment: the assistant exists to drive Daintree over MCP, so the
		// connection is worth announcing ONCE, right under the masthead. (The footer only
		// flags the link BY EXCEPTION when down.)
		//
		// But ONLY while the transcript is still just the masthead — before any turn has run.
		// The boot connect is async (bootstrapCmd → MCPConnectedMsg) and the 8s bootCap can
		// drop the cockpit in before it resolves, so the user may already have run a turn that
		// SUCCESSFULLY called an MCP tool. Notes commit to scrollback in transcript order, so
		// appending the banner after that turn would land it mid-transcript — a confusing
		// "just connected" line below a turn that already proved the link was up. If work has
		// started, stay silent: m.degraded is false and the footer reflects the link by
		// exception, so a live connection needs no late banner. (In the normal/idle case the
		// transcript is still empty here, so the banner shows and commits under the masthead.)
		if !m.transcriptHasWork() {
			m.addNote(NoteSuccess, "Connected to Daintree MCP")
		}
		return m.onMcpResolved()

	case MCPDegradedMsg:
		m.degraded = true
		m.addNote(NoteWarn, "Daintree MCP degraded — "+msg.Reason)
		return m.onMcpResolved()

	case ProjectNameMsg:
		// The authoritative project name resolved (or the fetch gave up / the link is
		// down). The masthead is committed to scrollback ONCE on the boot hand-off, so
		// the name must be in BEFORE the first paint — this is the third boot gate.
		if msg.Name != "" {
			m.masthead.ProjectName = msg.Name
		}
		m.projectSettled = true
		return m.afterStateChange(m.finishBootIfReady())

	case DashboardTickMsg:
		// Single-flight backpressure: if a prior build is still polling MCP for
		// previews (a slow read must not let builds pile up at the ~1s tick), skip this
		// build and just re-arm the ticker. The in-flight build will refresh the deck.
		if m.previewFetchInFlight {
			return m, dashboardTickCmd()
		}
		m.previewFetchInFlight = true
		return m, tea.Batch(m.buildDashboardCmd(), dashboardTickCmd())

	case DashboardSnapshotMsg:
		m.dashboard = msg.Snapshot
		// Advance preview-throttle state. previewCache always mirrors the tails in
		// effect (fetched or reused); lastPreviewFetchedAt only moves on a real poll
		// (FetchedAt>0) so the PreviewPollMS gate measures from the last actual fetch.
		m.previewFetchInFlight = false
		m.previewCache = msg.Previews
		if msg.FetchedAt > 0 {
			m.lastPreviewFetchedAt = msg.FetchedAt
		}
		// The attention badge is recomputed from each snapshot so it DECREMENTS as items
		// resolve — previously it only ratcheted up via onAttention and never came back down.
		m.attentionN = len(m.dashboard.Inbox)
		// The first snapshot is the OTHER half of the startup-settled gate (MCP connect
		// resolved AND the first dashboard snapshot is in). A later tick is a no-op here.
		if !m.bootSnapshotIn {
			m.bootSnapshotIn = true
			return m.afterStateChange(m.recomputeStartupSettled())
		}
		return m.afterStateChange(nil)

	case spinnerTickMsg:
		m.spinnerFrame++
		// Keep ticking only while a turn is in flight; idle, let it lapse (afterStateChange
		// re-arms it the instant the next turn starts) so the cockpit can quiesce.
		if !m.inFlight {
			m.spinnerRunning = false
			return m, nil
		}
		return m, spinnerTickCmd()

	case QuitArmExpireMsg:
		// The staged-Ctrl+C window lapsed; disarm unless a newer press re-armed (gen).
		if msg.Gen == m.quitArmGen {
			m.quitArmed = false
		}
		return m.afterStateChange(nil)

	case SplashTickMsg:
		cmd := m.splash.advance()
		// Force a FULL repaint each splash frame. The whole 18-row mark changes every
		// frame, and Bubble Tea's inline diff renderer repaints that with relative
		// cursor moves (up/left/backward-tab) that desync on tall, fully-changing
		// content — smearing the mark across the top of the screen. ClearScreen erases
		// the renderer's cell buffer so the next frame paints fresh from a clean slate
		// (under BT's synchronized-output, no flicker). Only while booting — the steady
		// cockpit's small, mostly-static footer diffs cleanly and must NOT clear (that
		// would wipe the host's native scrollback every render).
		if m.booting {
			return m, tea.Batch(cmd, tea.ClearScreen)
		}
		return m, cmd

	case CommitArmMsg:
		// One render cycle has passed since launch, so the short footer has flushed and
		// the renderer's cell buffer is sized to it — safe to begin scrollback commits
		// (the masthead lands first). See scheduleCommit.
		m.commitArmed = true
		return m.afterStateChange(nil)

	case SplashDoneMsg:
		// The splash draw + linger finished — one of the THREE boot gates (animationDone).
		// Booting does NOT flip here on its own: it waits for startup + project to settle
		// too, so a fast animation can't surface a half-built cockpit.
		m.splash.done = true
		m.animationDone = true
		return m.afterStateChange(m.finishBootIfReady())

	case TurnCompleteMsg:
		return m.onTurnComplete(msg)

	case WakeCompleteMsg:
		return m.onWakeComplete(msg)

	case CommandCompleteMsg:
		return m.onCommandComplete(msg)

	case ApprovalRequestedMsg:
		return m.onApprovalRequested(msg)

	case ApprovalResolvedMsg:
		return m.onApprovalResolved(msg)

	case AttentionBatchMsg:
		return m.onAttention(msg)

	case ScrollbackCommittedMsg:
		m.queue.ack(msg.ID, msg.Gen, len(m.transcript))
		return m.afterStateChange(nil)

	case RedrawMsg:
		return m.onRedraw(msg)

	case LogMsg:
		m.addNote(msg.Level, msg.Text)
		return m.afterStateChange(nil)

	case BootCapMsg:
		// Hard 8s backstop: drop into the cockpit regardless of gate readiness so a
		// hung startup never strands the user on the splash. A no-op if already up.
		if m.booting {
			return m, m.completeBoot()
		}
		return m, nil

	case ShutdownMsg:
		return m.onShutdown()
	}

	return m, nil
}

// applyPumpEvent fans one pump event into a model mutation. Returns any follow-up
// cmd (none for most). It mutates the active TurnCell's ordered steps + phase.
func (m *Model) applyPumpEvent(ev pumpEvent) tea.Cmd {
	t := m.activeTurnCell()
	if t != nil {
		// Any streamed event is a heartbeat — reset the stall timer (see renderLiveStatus).
		t.LastActivityAt = domain.NowMS()
	}
	switch ev.kind {
	case pumpPhase:
		if t != nil && t.Phase != ev.phase {
			t.Phase = ev.phase
			t.PhaseStartedAt = domain.NowMS()
		}
	case pumpStart:
		// A new model round; nothing to add — prose tokens will open a StepProse.
	case pumpTokens:
		if t != nil {
			t.appendProse(ev.text)
		}
	case pumpEnd:
		if t != nil {
			t.sealProse()
			t.Reasoning = ev.reasoning
		}
	case pumpCancelled:
		if t != nil {
			t.sealProse()
		}
	case pumpInterject:
		// A message the user typed mid-turn was just folded into the running turn. Seal
		// any live prose (this round became an intermediate one) and append an inline
		// interjection step in chronological place; the model's response opens a fresh
		// prose step after it. Clear the pending cue now that it's been delivered.
		if t != nil {
			t.sealProse()
			t.Steps = append(t.Steps, TurnStep{Kind: StepInterject, Text: ev.text})
		}
		if m.pendingInject > 0 {
			m.pendingInject--
		}
	case pumpBatch:
		if t != nil {
			for _, c := range ev.batch {
				t.Steps = append(t.Steps, TurnStep{Kind: StepTool, Activity: &Activity{
					ID: c.ID, Name: c.Name, Args: c.Args, State: ActQueued,
				}})
			}
		}
	case pumpToolState:
		if t != nil {
			if a := t.findActivity(ev.toolID); a != nil {
				a.State = mapToolState(ev.toolState)
			}
		}
	case pumpToolProgress:
		// In-tool substep for the active call: update its live progress line. The
		// settled result later overwrites Detail; ProgressMsg is the while-active line.
		if t != nil {
			if a := t.findActivity(ev.toolID); a != nil {
				a.ProgressMsg = ev.msg
			}
		}
	case pumpToolCall:
		if t != nil {
			if a := t.findActivity(ev.call.ID); a != nil {
				a.State = ActActive
				a.StartedAt = ev.call.StartedAt
				a.Args = ev.call.Args
			}
		}
	case pumpToolResult:
		if t != nil {
			if a := t.findActivity(ev.result.ID); a != nil {
				a.EndedAt = ev.result.EndedAt
				if ev.result.Result.Ok {
					a.State = ActDone
					a.Detail = ev.result.Result.Summary
				} else {
					a.State = ActFailed
					a.Outcome = toolFailSummary(ev.result.Result)
				}
			}
		}
	case pumpUsage:
		m.applyUsage(ev.usage)
	case pumpModelRateLimited:
		m.modelRateLimited = true
	case pumpLog:
		m.addNote(ev.level, ev.msg)
	case pumpError:
		m.addNote(NoteError, ev.msg)
	}
	return nil
}

// mapToolState converts the agent tool state to the UI activity state.
func mapToolState(s agent.ToolState) ActivityState {
	switch s {
	case agent.ToolStateActive:
		return ActActive
	case agent.ToolStateWaiting:
		return ActWaiting
	case agent.ToolStateDone:
		return ActDone
	case agent.ToolStateFailed:
		return ActFailed
	default:
		return ActQueued
	}
}

// toolFailSummary extracts a failure summary from a ToolResult (message > code).
func toolFailSummary(r domain.ToolResult) string {
	if r.Error != nil {
		if r.Error.Message != "" {
			return r.Error.Message
		}
		return r.Error.Code
	}
	if r.Summary != "" {
		return r.Summary
	}
	return "failed"
}

// applyUsage updates the CTX% / cost / model rollup from a usage event.
func (m *Model) applyUsage(u agent.UsageEvent) {
	m.hasUsage = true
	// A successful round means the provider answered — clear any model-rate-limit
	// health badge raised by a prior exhausted-429 turn.
	m.modelRateLimited = false
	// CTX% is "% of the model's context window in use" — divide by the window, NOT the
	// (much smaller) auto-compact threshold, so a 1M-window model reads ~1% on a short
	// conversation rather than a misleading 13%. Fall back to the threshold only if no
	// window was reported (older events / tests). int64 avoids overflow on large token
	// counts; clamp to [0,100] so an over-full context reads "100%", never above.
	denom := u.ContextWindow
	if denom <= 0 {
		denom = u.ContextThreshold
	}
	if denom > 0 {
		pct := int(int64(u.ContextTokens) * 100 / int64(denom))
		if pct < 0 {
			pct = 0
		} else if pct > 100 {
			pct = 100
		}
		m.contextPct = pct
	}
	if u.CostUsd != nil {
		m.cost += *u.CostUsd
	}
	if u.Model != "" {
		m.model = u.Model
	}
}

// activeTurnCell returns the live TurnCell (the one streaming), or nil.
func (m *Model) activeTurnCell() *TurnCell {
	if m.activeTurn == "" {
		return nil
	}
	for i := range m.transcript {
		if m.transcript[i].Turn != nil && m.transcript[i].Turn.ID == m.activeTurn {
			return m.transcript[i].Turn
		}
	}
	return nil
}

// addNote appends a standalone NoteCell to the transcript (out-of-band line).
func (m *Model) addNote(level NoteLevel, text string) {
	m.addSeverityNote(level, "", text)
}

// addSeverityNote appends a standalone NoteCell that carries its precise queue
// severity, so the renderer draws ONE precise spine glyph instead of a coarse level
// glyph plus a duplicate in the text. A "" severity behaves exactly like addNote.
func (m *Model) addSeverityNote(level NoteLevel, sev domain.Severity, text string) {
	m.transcript = append(m.transcript, TranscriptCell{Note: &NoteCell{
		ID: domain.NewID("note_"), Level: level, Severity: sev, Text: text, Ts: domain.NowMS(),
	}})
}

// transcriptHasWork reports whether any turn or slash-command cell has been added to the
// transcript yet (notes — top status lines — don't count). It gates the one-time boot
// connect banner so it's only appended while the transcript is still just the masthead,
// committing directly under it rather than below a turn the user already ran. It scans
// appended-but-not-yet-committed cells too (committed cells stay in m.transcript, tracked
// by a cursor), so it stays correct even after early commits.
func (m Model) transcriptHasWork() bool {
	for _, c := range m.transcript {
		if c.Turn != nil || c.Command != nil {
			return true
		}
	}
	return false
}

// afterStateChange is the common tail: schedule the next scrollback commit (if the
// frontier advanced), incrementally flush the active turn's newly-final rows to
// native scrollback (keeps the live View ~constant-height — see flush.go), and update
// the composer's busy/focus flags. Returns (model, cmd).
func (m Model) afterStateChange(extra tea.Cmd) (tea.Model, tea.Cmd) {
	m.syncComposer()
	spin := m.ensureSpinnerForState()
	commit := m.scheduleCommit()
	// Flush the active turn's completed rows AFTER scheduling the queue commit so the
	// masthead (queue) is ordered ahead of any prose Println (flushActiveTurn also gates
	// on headerDone). Emitting the flush in the SAME batch is fine: tea.Println preserves
	// emit order, and the queue's masthead commit is enqueued first here.
	flush := m.flushActiveTurn()
	return m, tea.Batch(extra, commit, flush, spin)
}

// ensureSpinnerForState starts the ~10fps spinner tick when a turn is in flight and the
// tick isn't already running. Returns nil (no extra wakeups) when idle or already ticking;
// spinnerTickMsg stops it once the turn settles.
func (m *Model) ensureSpinnerForState() tea.Cmd {
	if m.inFlight && !m.spinnerRunning {
		m.spinnerRunning = true
		return spinnerTickCmd()
	}
	return nil
}

// syncComposer pushes the current busy/focus state into the composer each reduction.
func (m *Model) syncComposer() {
	m.composer.SetBusy(m.inFlight)
	m.composer.SetFocus(m.composerFocus())
	m.composer.SetWidth(m.contentW()) // so ↑/↓ follow soft-wrapped visual rows
}

// scheduleCommit asks the queue for the next commit cmd at the current width.
//
// COMMIT ARM (charmbracelet/bubbletea#1613): nothing commits to native scrollback
// until commitArmed flips true, one short tick after Init (see commitArmCmd). Bubble
// Tea v2 DEFERS its cell-buffer resize to the FPS-ticker flush, and tea.Println's
// insertAbove runs synchronously off that buffer's height. The program opens already
// in the cockpit (the splash played before BT started), so the live View is the short
// footer from the start — but the renderer's buffer is only sized to it after the
// first flush. Arming the first commit one cycle in guarantees the footer has flushed,
// so the masthead Println lays out above a correctly-sized footer instead of wiping it.
func (m *Model) scheduleCommit() tea.Cmd {
	if !m.commitArmed {
		return nil
	}
	return m.queue.nextCommit(m.transcript, m.sealedBlock, m.headerBlock)
}

// finishBootIfReady flips booting off ONCE all three boot gates are satisfied
// (startupSettled && animationDone && projectSettled) — the original's exact lock.
// On the flip it fires a single boot→cockpit "nuclear redraw" (the original calls
// requestRedraw once on the hand-off via useResizeRedraw): it re-arms the commit
// queue at the now-correct cockpit width and wipes any splash rows the program left
// in the live region, then commits the masthead + transcript fresh. Returns the
// hand-off cmd (nil unless the gate just closed).
func (m *Model) finishBootIfReady() tea.Cmd {
	if !m.booting {
		return nil
	}
	if !(m.startupSettled && m.animationDone && m.projectSettled) {
		return nil
	}
	return m.completeBoot()
}

// completeBoot performs the boot→cockpit hand-off: drop the splash, re-arm the commit
// queue (bump redrawNonce → resetKey change → committed=0, headerDone=false), and run
// the ordered wipe-then-recommit so the masthead lands flush on a clean host screen
// with the composer live beneath it. Like a redraw, but driven by the boot gate.
func (m *Model) completeBoot() tea.Cmd {
	m.booting = false
	m.splash.done = true
	m.redrawNonce++
	m.queue.applyResetKey(m.clearNonce + m.redrawNonce)
	// #3 (see onClear/onRedraw): ordered wipe-then-recommit so the hand-off can't print
	// a stale block after the host clear or wipe a freshly committed masthead.
	//
	// The boot view was a tall (20-row) block, so right now Bubble Tea's renderer still
	// reserves that height. Committing the masthead immediately lays it out with stale
	// geometry (a misplaced erase-below then wipes it). So: clear the host + reset BT's
	// own cell buffer (tea.ClearScreen), then DEFER the masthead commit one render cycle
	// (bootCommitMsg) — by which point the short footer has rendered and the renderer's
	// live-area model has shrunk, so the masthead prints above a correctly-sized footer.
	return tea.Sequence(hostClearCmd(), tea.ClearScreen, commitArmCmd())
}
