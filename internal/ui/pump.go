package ui

import (
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// pump.go bridges the runtime's agent.EventSink (which the loop calls on its own
// goroutines) into Bubble Tea's single Update loop using the idiomatic
// channel + re-armed-command pattern — NEVER Program.Send per
// token. The coalescer buffers adjacent assistant tokens and flushes them as
// one msg ≤16-33ms or before any non-token event, preserving order, never dropping.
//
// ORDERING (CODE-REVIEW #1/#2/#6): the coalescer "drain buffer + enqueue event" is
// serialized through ONE mutex and a SINGLE dedicated sender goroutine. A non-token
// event's enqueue can NEVER race the timer's token flush, because both append to the
// same in-order pending slice under the same lock; the sole sender drains that slice
// to the channel. That makes the buffer one bounded output path (no per-token
// goroutine pile-up under a stalled consumer) and guarantees tokens land before any
// following phase/end/error. Turn/wake COMPLETION rides this same stream (via
// Complete), so it can never overtake queued events on a separate tea.Cmd (#1).

// pumpEvent is the closed union the pump carries over its channel. Exactly one
// field is meaningful per kind; the model fans each into a typed tea.Msg in Update.
type pumpEvent struct {
	kind pumpKind

	// token (coalesced)
	text string
	// end / cancelled
	reasoning string
	// phase
	phase domain.RunPhase
	// tool batch
	batch []agent.BatchedToolCall
	// tool state
	toolID    string
	toolState agent.ToolState
	// tool progress (toolID carries the call id; msg carries the substep text)
	// tool call
	call agent.ToolCallEvent
	// tool result
	result agent.ToolResultEvent
	// usage
	usage agent.UsageEvent
	// log / error / info
	level NoteLevel
	msg   string
	// attention (scheduler burst)
	attention []domain.QueueEvent
	// completion (turn / wake), delivered in-stream so it can't overtake events.
	completion completionPayload
}

// completionPayload carries an ordered turn/wake completion through the pump stream.
// runID lets the reducer reject a completion meant for a turn that is no longer
// active (an ordering barrier — see Model.onTurnComplete).
type completionPayload struct {
	wake   bool
	runID  string
	reply  string
	failed bool
}

type pumpKind int

const (
	pumpStart  pumpKind = iota
	pumpTokens          // coalesced
	pumpEnd
	pumpCancelled
	pumpInterject // a mid-turn user message folded into the running turn
	pumpPhase
	pumpBatch
	pumpToolState
	pumpToolProgress
	pumpToolCall
	pumpToolResult
	pumpUsage
	pumpLog
	pumpError
	pumpAttention
	pumpComplete         // turn / wake completion, delivered IN-STREAM (#1)
	pumpModelRateLimited // provider 429 after the retry budget — raise the health badge
	pumpCommandProgress  // a slash command's live stage label (msg carries the text)
)

// eventPump is the agent.EventSink implementation. It coalesces tokens and forwards
// everything else through ONE ordered pending slice drained by a single sender
// goroutine, so token order vs non-token events is preserved and completion can't
// overtake queued events.
type eventPump struct {
	ch chan pumpEvent

	mu       sync.Mutex
	buf      []byte      // pending coalesced tokens
	timer    *time.Timer // flush deadline; nil when no tokens pending
	flushDur time.Duration

	pending []pumpEvent // ordered queue drained by the sole sender goroutine
	wake    chan struct{}
}

// coalesceWindow is one frame at ~30fps; flushing on this cadence keeps Update at
// ≤30-60 msgs/sec regardless of token rate.
const coalesceWindow = 25 * time.Millisecond

// newEventPump builds a pump with a buffered channel so the runtime never blocks on
// a slow Update (backpressure is the buffer; ordering is the channel's FIFO). A
// single sender goroutine owns the channel.
func newEventPump() *eventPump {
	p := &eventPump{
		ch:       make(chan pumpEvent, 256),
		flushDur: coalesceWindow,
		wake:     make(chan struct{}, 1),
	}
	go p.run()
	return p
}

// run is the sole sender goroutine: it drains the ordered pending slice to the
// channel in FIFO order. Because every producer appends under p.mu and ONLY this
// goroutine sends, "drain buffer + enqueue event" stays atomic relative to the
// stream — tokens can never land after a following non-token event, and a stalled
// consumer (full channel) blocks only THIS one goroutine, not the runtime's
// emitters (they just append to pending and signal).
func (p *eventPump) run() {
	for range p.wake {
		for {
			p.mu.Lock()
			if len(p.pending) == 0 {
				p.mu.Unlock()
				break
			}
			ev := p.pending[0]
			p.pending = p.pending[1:]
			p.mu.Unlock()
			p.ch <- ev // may block on a full channel — only this goroutine waits
		}
	}
}

// enqueueLocked appends one event to the ordered pending slice. Caller holds p.mu.
func (p *eventPump) enqueueLocked(ev pumpEvent) {
	p.pending = append(p.pending, ev)
}

// signal wakes the sender (non-blocking; the buffered chan coalesces signals).
func (p *eventPump) signal() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// emit drains any buffered tokens THEN appends ev, both under one lock, so a
// non-token event always lands after the trailing token chunk (#2). One signal.
func (p *eventPump) emit(ev pumpEvent) {
	p.mu.Lock()
	p.drainBufLocked()
	p.enqueueLocked(ev)
	p.mu.Unlock()
	p.signal()
}

// drainBufLocked moves any pending coalesced tokens into the ordered queue as ONE
// pumpTokens event and disarms the timer. Caller holds p.mu.
func (p *eventPump) drainBufLocked() {
	if p.timer != nil {
		p.timer.Stop()
		p.timer = nil
	}
	if len(p.buf) == 0 {
		return
	}
	text := string(p.buf)
	p.buf = p.buf[:0]
	p.enqueueLocked(pumpEvent{kind: pumpTokens, text: text})
}

// waitEvent is the re-armed command: it blocks on the channel and yields ONE msg.
// Update returns it again after each AgentEventMsg to re-arm the pump.
func (p *eventPump) waitEvent() tea.Cmd {
	return func() tea.Msg { return AgentEventMsg{Event: <-p.ch} }
}

// --- agent.EventSink implementation (called on runtime goroutines) ---

// Phase flushes pending tokens (order!) then forwards the phase.
func (p *eventPump) Phase(ph domain.RunPhase) {
	p.emit(pumpEvent{kind: pumpPhase, phase: ph})
}

func (p *eventPump) AssistantStart() {
	p.emit(pumpEvent{kind: pumpStart})
}

// AssistantToken buffers the token; the flush timer (or a non-token event) emits it.
// This is the ONLY method that does not forward immediately — the coalescer.
func (p *eventPump) AssistantToken(token string) {
	if token == "" {
		return
	}
	p.mu.Lock()
	p.buf = append(p.buf, token...)
	if p.timer == nil {
		// Arm a one-shot flush. AfterFunc runs on its own goroutine; flush re-locks.
		p.timer = time.AfterFunc(p.flushDur, p.flush)
	}
	p.mu.Unlock()
}

func (p *eventPump) AssistantEnd(content, reasoning string) {
	p.emit(pumpEvent{kind: pumpEnd, text: content, reasoning: reasoning})
}

func (p *eventPump) AssistantCancelled(content string) {
	p.emit(pumpEvent{kind: pumpCancelled, text: content})
}

// Interjection forwards a mid-turn user message (flushing buffered tokens first so it
// lands after the round it interrupted). The reducer renders it inline in the turn.
func (p *eventPump) Interjection(text string) {
	p.emit(pumpEvent{kind: pumpInterject, text: text})
}

// SkillLoaded is DELIBERATELY inert in the cockpit. Backend skill selection is
// prompt-assembly machinery the user neither approves nor steers, so it gets no
// transcript real estate; the durable run log, --json stream and debug trace still carry
// every load. Inert rather than an emit-with-no-render: emit() drains the token
// coalescer, so forwarding a skill load would still perturb the prose flush boundary for
// something that draws nothing. See Session.emitSkillLoads for the full rationale.
func (p *eventPump) SkillLoaded([]string) {}

func (p *eventPump) ToolBatch(calls []agent.BatchedToolCall) {
	p.emit(pumpEvent{kind: pumpBatch, batch: calls})
}

func (p *eventPump) ToolState(id string, state agent.ToolState) {
	p.emit(pumpEvent{kind: pumpToolState, toolID: id, toolState: state})
}

// ToolProgress forwards an in-tool substep for the active call (flushes tokens
// first so the substep lands after any streamed prose).
func (p *eventPump) ToolProgress(id string, msg string) {
	p.emit(pumpEvent{kind: pumpToolProgress, toolID: id, msg: msg})
}

func (p *eventPump) ToolCall(ev agent.ToolCallEvent) {
	p.emit(pumpEvent{kind: pumpToolCall, call: ev})
}

func (p *eventPump) ToolResult(ev agent.ToolResultEvent) {
	p.emit(pumpEvent{kind: pumpToolResult, result: ev})
}

func (p *eventPump) Usage(ev agent.UsageEvent) {
	p.emit(pumpEvent{kind: pumpUsage, usage: ev})
}

// TurnPrompt is durable-log-only vocabulary (persisted by the run-event sink for
// /explain); the live cockpit already shows the prompt as the user turn, so the
// pump drops it.
func (p *eventPump) TurnPrompt(string) {}

// ModelRateLimited forwards the provider-throttled health cue so the reducer can
// raise the "▲ Model rate-limited" footer badge (cleared on the next Usage).
func (p *eventPump) ModelRateLimited() {
	p.emit(pumpEvent{kind: pumpModelRateLimited})
}

func (p *eventPump) Error(msg string) {
	p.emit(pumpEvent{kind: pumpError, level: NoteError, msg: msg})
}

func (p *eventPump) Warn(msg string) {
	p.emit(pumpEvent{kind: pumpLog, level: NoteWarn, msg: msg})
}

func (p *eventPump) Info(msg string) {
	p.emit(pumpEvent{kind: pumpLog, level: NoteInfo, msg: msg})
}

// CommandProgress forwards a running slash command's stage label (e.g. /compact's
// "Compacting conversation…") so the composer's busy cue can narrate the silent
// model-backed stretches. Called from the command goroutine, ordered like any other
// event; the reducer just updates the label (no transcript mutation).
func (p *eventPump) CommandProgress(stage string) {
	p.emit(pumpEvent{kind: pumpCommandProgress, msg: stage})
}

// Complete delivers a turn/wake completion THROUGH the ordered stream (#1) so it
// can never overtake the turn's own queued events on a separate tea.Cmd. The
// reducer (onTurnComplete/onWakeComplete) only clears/promotes the active turn after
// this event drains, by which point every prior event for the turn has been applied.
func (p *eventPump) Complete(c completionPayload) {
	p.emit(pumpEvent{kind: pumpComplete, completion: c})
}

// flush is the timer callback: drain buffered tokens into the ordered queue. It is
// the timer's only job (the buffer is non-empty whenever the timer is armed), so it
// never enqueues an empty chunk. Serialized with emit through p.mu.
func (p *eventPump) flush() {
	p.mu.Lock()
	p.drainBufLocked()
	p.mu.Unlock()
	p.signal()
}

// send pushes one event onto the ordered queue (used by sendAttention). FIFO,
// serialized with the coalescer through emit.
func (p *eventPump) send(ev pumpEvent) { p.emit(ev) }
