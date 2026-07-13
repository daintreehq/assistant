package ui

import (
	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// messages.go is the typed tea.Msg vocabulary the root Model reduces in Update.
// Every payload is a concrete struct — NO anonymous any payloads — so the message
// flow is statically checked.
//
// These come from three sources: the runtime event pump (agent events coalesced
// into UI msgs), the controller (turn/command/wake completion), and UI-internal
// timers/lifecycle (splash, redraw, attention, shutdown).

// --- bootstrap / connection lifecycle ---

// BootstrapMsg fires once after the program is up: it awaits the ONE MCP connect that
// already started under the splash, then starts the scheduler. The wait runs off-loop,
// so the composer is interactive while any remaining discovery settles.
type BootstrapMsg struct{}

// MCPConnectedMsg lands when the async MCP connect settles successfully. ToolCount
// is the advertised tool count; it upgrades the masthead/status from provisional.
type MCPConnectedMsg struct {
	Transport string
	ToolCount int
}

// MCPDegradedMsg lands when the MCP link is down (failed connect or a later drop).
// The status line shows DEGRADED by exception; there is no steady-state healthy
// badge.
type MCPDegradedMsg struct {
	Reason string
}

// ProjectNameMsg carries the authoritative project name from Daintree's
// actions.getContext (or an empty Name when the fetch gave up / the link is down).
// It closes the projectSettled boot gate and updates later redraws/session metadata.
type ProjectNameMsg struct {
	Name string
}

// BootCapMsg is the hard 8s boot-cap backstop: if the 3-gate boot lock hasn't closed
// by then (e.g. a hung MCP), drop into the cockpit regardless so the user is never
// stranded on the splash. A no-op once booting has already finished.
type BootCapMsg struct{}

// CommitArmMsg fires one short tick after launch and arms the FIRST scrollback commit.
// Bubble Tea v2 defers its cell-buffer resize to the FPS-ticker flush, and tea.Println
// reads that buffer's height synchronously; arming a cycle in guarantees the short
// footer has flushed first, so the masthead lays out above a correctly-sized footer
// rather than wiping it (charmbracelet/bubbletea#1613). See scheduleCommit.
type CommitArmMsg struct{}

// rendererRepaintMsg is the post-clear half of a host redraw. Bubble Tea v2 can
// optimize away tea.ClearScreen when View.Content and its bounds are unchanged,
// even though hostClearCmd has already erased the physical terminal. Handling this
// message toggles a zero-cell content tag so the next renderer flush cannot take
// that equality fast path. It must be sequenced AFTER tea.ClearScreen.
type rendererRepaintMsg struct{}

// --- dashboard / operations snapshots ---

// DashboardTickMsg is the ~1s poll that refreshes the operations deck + status
// rollup from the store/queue (timers, audit, inbox, agent rows).
type DashboardTickMsg struct{}

// DashboardSnapshotMsg carries a freshly built operations snapshot (built off the
// loop so Update stays cheap). Previews/FetchedAt thread the preview-throttle state
// back to the Model: the agent rows already have their AgentState/Preview folded in,
// so these two fields exist only so Update can advance lastPreviewFetchedAt and the
// previewCache without calling the clock in the pure reducer. FetchedAt is 0 (and
// Previews nil) when this tick reused the cache instead of polling MCP.
type DashboardSnapshotMsg struct {
	Snapshot  Dashboard
	Previews  []daemon.TerminalPreview
	FetchedAt int64
}

// --- agent run events (already mapped off agent.EventSink) ---

// AssistantStartMsg — a new model round is about to stream.
type AssistantStartMsg struct{}

// AssistantTokensMsg carries COALESCED assistant text, never a single
// per-token chunk. The coalescer concatenates adjacent tokens in arrival order and
// flushes ≤16-33ms or before any non-token event.
type AssistantTokensMsg struct{ Text string }

// AssistantEndMsg seals the round's final prose (reasoning kept for ^X expand).
type AssistantEndMsg struct {
	Content   string
	Reasoning string
}

// AssistantCancelledMsg — the user aborted mid-flight; Content is usually "".
type AssistantCancelledMsg struct{ Content string }

// PhaseMsg drives the explicit RunPhase of the active turn (the liveness source of
// truth; never inferred from empty text).
type PhaseMsg struct{ Phase domain.RunPhase }

// ToolBatchMsg announces every parsed tool call as queued BEFORE sequential
// dispatch so the footer shows the whole batch at once.
type ToolBatchMsg struct{ Calls []agent.BatchedToolCall }

// ToolStateMsg promotes one announced call (queued→active→done/failed/waiting).
type ToolStateMsg struct {
	ID    string
	State agent.ToolState
}

// ToolCallMsg — a tool call is starting (with parsed/raw args).
type ToolCallMsg struct {
	ID        string
	Name      string
	Args      string
	StartedAt int64
}

// ToolResultMsg — a tool call settled.
type ToolResultMsg struct {
	ID      string
	Name    string
	Result  domain.ToolResult
	EndedAt int64
}

// LogMsg — an out-of-band informational/warning/error line (a NoteCell).
type LogMsg struct {
	Level NoteLevel
	Text  string
}

// AgentEventMsg is the raw pump message: one event pulled off the buffered channel
// by the re-armed waitEvent command. Update fans it into the typed msgs above and
// re-arms the pump. Kept distinct so the channel+command pattern is explicit and we
// never call Program.Send per token.
type AgentEventMsg struct{ Event pumpEvent }

// --- controller completion msgs ---

// TurnCompleteMsg lands when a Session.Send finishes (success or sentinel). Reply
// is the model's final answer or a sentinel; it frees the single-flight lock and
// triggers the FIFO drain of queued follow-ups / wakes.
type TurnCompleteMsg struct {
	// RunID is the UI cell id the turn was dispatched under; the reducer's ordering
	// barrier ignores a completion whose id is no longer the active turn (#1).
	RunID  string
	Reply  string
	Failed bool
}

// WakeCompleteMsg lands when an autonomous wake reactor turn finishes. Failed
// re-queues once (the per-burst retry budget). RunID is the wake cell id (barrier).
type WakeCompleteMsg struct {
	RunID  string
	Reply  string
	Failed bool
}

// CommandCompleteMsg carries a slash-command result rendered into the transcript.
type CommandCompleteMsg struct {
	Title           string
	Text            string
	ClearTranscript bool
	Quit            bool
	// SwitchPanel switches the live view instead of printing a card: PanelHelp → the help
	// view; PanelInbox/Watchers/Timers/Audit → the ops deck focused on that section.
	SwitchPanel PanelKey
	// Tracked marks a completion from controller.runCommand — the async path whose
	// submit incremented commandsRunning. Only tracked completions decrement it: the
	// synchronous /approvals shortcut also lands here but never incremented, and an
	// untracked decrement would retire ANOTHER command's liveness cue mid-run.
	Tracked bool
	// Unhandled marks a tracked submission the commands package rejected (a bare "/"
	// parses to no command). It must still complete — otherwise the counter leaks and
	// the spinner ticks forever — but prints no card.
	Unhandled bool
}

// --- approval lifecycle ---

// ApprovalRequestedMsg carries a confirm request + the resolve channel the runtime
// goroutine blocks on. Update parks it as the pending sheet and drives phase
// awaiting_approval. The runtime cannot proceed until ApprovalResolvedMsg routes a
// decision back through the channel.
type ApprovalRequestedMsg struct {
	Request tools.ConfirmRequest
	Resolve chan bool
}

// ApprovalResolvedMsg is dispatched when the user (or shutdown) decides. It pops
// the sheet and refines the phase; the controller sends the decision on Resolve.
type ApprovalResolvedMsg struct {
	Approved bool
}

// --- multiple-choice question lifecycle ---

// questionReply is the user's answer routed back to the blocked user.askMultipleChoice
// dispatch goroutine. Index is the chosen 0-based option; cancelled marks an Esc /
// Ctrl+C / shutdown that aborts the turn instead of answering.
type questionReply struct {
	index     int
	cancelled bool
}

// QuestionRequestedMsg carries a multiple-choice request + the reply channel the
// runtime goroutine blocks on. Update parks it as the pending question sheet (in place
// of the composer) and drives phase awaiting_question. The runtime cannot proceed until
// the user answers (or cancels), which routes a questionReply back through the channel.
type QuestionRequestedMsg struct {
	Request tools.AskChoiceRequest
	Reply   chan questionReply
}

// --- scrollback commit ack ---

// ScrollbackCommittedMsg is the ack Bubble Tea delivers after a persistent-print
// command flushes above the program. It pops the in-flight queue head, advances the
// committed cursor (dropping that sealed cell from the live footer), and schedules
// the next block. ID matches the just-committed block (or "__header__").
type ScrollbackCommittedMsg struct {
	ID  string
	Gen int // the commit generation (#4); the ack is ignored if it != the queue's gen
}

// --- splash ---

// SplashTickMsg advances the boot splash frame index on its ~28fps tick.
type SplashTickMsg struct{}

// SplashDoneMsg dissolves the splash overlay (timer-driven; never gates input).
type SplashDoneMsg struct{}

// --- attention / out-of-band ---

// AttentionBatchMsg is a fresh batch of attention queue events from the scheduler.
// It feeds the wake queue, bumps the window-title count, and rings the BEL once per
// batch (on the event, not a count increment).
type AttentionBatchMsg struct{ Events []domain.QueueEvent }

// --- shell lifecycle ---

// ShutdownMsg requests an orderly cockpit teardown (Ctrl+C): reject pending
// confirms, cancel the in-flight turn, then quit.
type ShutdownMsg struct{}

// QuitArmExpireMsg disarms the staged-Ctrl+C "press again to exit" window once it
// lapses. Gen guards against a stale timer disarming a freshly re-armed prompt.
type QuitArmExpireMsg struct{ Gen int }

// RedrawMsg is the debounced resize "nuclear redraw" trigger. It re-commits
// the masthead + whole transcript fresh at the new width; the transcript model is
// left intact (separate from /clear). Nonce dedupes a drag storm.
type RedrawMsg struct{ Nonce int }
