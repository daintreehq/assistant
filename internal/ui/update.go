package ui

import (
	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// update.go is the single reducer (ui-input.md §6.4: NEVER mutate the model outside
// Update). It folds runtime/pump msgs + key events into the transcript, drives the
// explicit RunPhase, serializes work (single-flight + FIFO queue + wake priority),
// and schedules scrollback commits.

// Init kicks the event pump, the dashboard tick, the splash tick, and the one-shot
// bootstrap (async MCP connect + scheduler). The composer is already interactive.
func (m Model) Init() tea.Cmd {
	return tea.Batch(
		m.pump.waitEvent(),
		func() tea.Msg { return BootstrapMsg{} },
		splashTickCmd(),
		dashboardTickCmd(),
		spinnerTickCmd(),
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
		return m.afterStateChange(nil)

	case MCPDegradedMsg:
		m.degraded = true
		m.addNote(NoteWarn, "Daintree MCP degraded — "+msg.Reason)
		return m.afterStateChange(nil)

	case DashboardTickMsg:
		return m, tea.Batch(m.buildDashboardCmd(), dashboardTickCmd())

	case DashboardSnapshotMsg:
		m.dashboard = msg.Snapshot
		return m.afterStateChange(nil)

	case spinnerTickMsg:
		m.spinnerFrame++
		return m, spinnerTickCmd()

	case SplashTickMsg:
		cmd := m.splash.advance()
		return m, cmd

	case SplashDoneMsg:
		m.splash.done = true
		m.booting = false
		return m.afterStateChange(nil)

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

	case ShutdownMsg:
		return m.onShutdown()
	}

	return m, nil
}

// applyPumpEvent fans one pump event into a model mutation. Returns any follow-up
// cmd (none for most). It mutates the active TurnCell's ordered steps + phase.
func (m *Model) applyPumpEvent(ev pumpEvent) tea.Cmd {
	t := m.activeTurnCell()
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
	if u.ContextThreshold > 0 {
		m.contextPct = (u.ContextTokens * 100) / u.ContextThreshold
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
	m.transcript = append(m.transcript, TranscriptCell{Note: &NoteCell{
		ID: domain.NewID("note_"), Level: level, Text: text, Ts: domain.NowMS(),
	}})
}

// afterStateChange is the common tail: schedule the next scrollback commit (if the
// frontier advanced) and update the composer's busy/focus flags. Returns (model, cmd).
func (m Model) afterStateChange(extra tea.Cmd) (tea.Model, tea.Cmd) {
	m.syncComposer()
	commit := m.scheduleCommit()
	if extra == nil {
		return m, commit
	}
	return m, tea.Batch(extra, commit)
}

// syncComposer pushes the current busy/focus state into the composer each reduction.
func (m *Model) syncComposer() {
	m.composer.SetBusy(m.inFlight)
	m.composer.SetFocus(m.composerFocus())
}

// scheduleCommit asks the queue for the next commit cmd at the current width.
func (m *Model) scheduleCommit() tea.Cmd {
	return m.queue.nextCommit(m.transcript, m.sealedBlock, m.headerBlock)
}
