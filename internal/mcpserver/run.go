// Package mcpserver serves the assistant itself as an MCP server, so another agent
// (Claude Code, most immediately) can drive it as a sub-agent rather than shelling out
// to a one-shot process per question.
//
// The shape is dictated by one fact: a Daintree turn takes MINUTES. It spawns agent
// terminals, waits on cohorts, extracts and scores. Every MCP client times out long
// before that, so a synchronous ask(prompt) -> answer tool would be unusable for exactly
// the work this assistant exists to do. The surface is therefore ASYNC-FIRST — ask
// returns a run handle immediately and poll reads it incrementally — which is the same
// shape the assistant already uses internally for its own long work
// (terminal.run.async returns an `asy_…` handle that a coordinator settles later).
//
// The other governing decision is that the server holds NO configuration of its own.
// An MCP client launches this process once and keeps the pipe for its whole session; it
// has no way to restart it when the developer edits config or rebuilds. So every
// binding — project, backend endpoint, tier, MCP credentials — is an argument to
// session.open, and the process env only supplies defaults. Changing any of them is a
// close/open pair, not a reconnect.
package mcpserver

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// RunStatus is the lifecycle of one turn driven through this server.
type RunStatus string

const (
	RunRunning   RunStatus = "running"
	RunSucceeded RunStatus = "success"
	RunFailed    RunStatus = "error"
	RunCancelled RunStatus = "cancelled"
)

// Event is one recorded step of a run, in the vocabulary the --json stream already
// uses. Keeping the two vocabularies identical is deliberate: a consumer that learned
// one can read the other, and docs/HEADLESS.md documents both.
type Event struct {
	Seq  int    `json:"seq"`
	Ts   int64  `json:"ts"`
	Type string `json:"type"`
	// Text carries the payload for prose-ish events (content, message, interjection).
	Text string `json:"text,omitempty"`
	// Tool fields, set on tool:call / tool:result.
	Tool    string `json:"tool,omitempty"`
	CallID  string `json:"callId,omitempty"`
	Ok      *bool  `json:"ok,omitempty"`
	Summary string `json:"summary,omitempty"`
	Error   string `json:"error,omitempty"`
	// Async is the `asy_…` handle when a tool accepted work that settles later. A
	// caller seeing this must NOT expect the result on this run.
	Async string `json:"async,omitempty"`
	// Skills are runbook titles: the ones newly loaded on a skill:loaded event, the
	// whole ACTIVE set on a skill:decision event.
	Skills []string `json:"skills,omitempty"`
	// SkillsDegraded marks a skill:decision whose selector failed open and reused the
	// prior active set — the run carries a runbook it did not actually choose. Omitted
	// everywhere else, including on a clean decision.
	SkillsDegraded bool `json:"skillsDegraded,omitempty"`
}

// Run is one turn: its prompt, its recorded events, and its outcome. It is written by
// the Recorder on the turn goroutine and read by poll on an MCP handler goroutine, so
// every field access goes through mu.
type Run struct {
	ID        string
	SessionID string
	Prompt    string

	mu        sync.Mutex
	status    RunStatus
	startedAt int64
	endedAt   int64
	events    []Event
	content   string
	errMsg    string
	stats     domain.JsonRunStats
	// cancel aborts this run's context. Held here so interrupt can reach it without
	// walking the session's lock ordering.
	cancel func()
	// done closes when the run settles, so a blocking ask can wait without polling.
	done chan struct{}
	// changed is a broadcast channel replaced on every observable change (a new event,
	// a parked approval, settlement). A long poll waits on it so `waitMs` means "wake
	// me when something HAPPENS", not "wake me when the turn finishes" — the latter
	// made a 60s wait sit through new content, a started tool and a blocking approval
	// before reporting any of them.
	changed chan struct{}
	// revision counts observable changes. A waiter captures it BEFORE it starts waiting
	// and hands it back, which closes the lost-wakeup window: without it, a change
	// landing between the caller reading state and reaching the select would be missed
	// and the poll would sleep out its whole budget over news that had already arrived.
	revision uint64
	// asyncOps is the run's ledger of background handles it accepted, keyed by id and
	// in acceptance order. It exists because the old surface derived "pending async"
	// by scanning the events in the CURRENT poll window: the handles vanished as soon
	// as the caller advanced sinceSeq, and were missed entirely when the accepting
	// event fell outside maxEvents.
	asyncOps   map[string]*AsyncOperation
	asyncOrder []string
}

// AsyncOperation is one background handle this run accepted. The run itself never
// learns the outcome — an async tool settles through the attention queue, deliberately
// not as a late event on a closed run — so `status` is what this run can honestly say.
type AsyncOperation struct {
	ID string `json:"id"`
	// Tool is the tool call that accepted the work, so a caller can tell a spawned
	// agent from a terminal wait without correlating call ids by hand.
	Tool string `json:"tool,omitempty"`
	// Status is "accepted": this run saw the handle issued and will never see it
	// settle. Completion is reported through daintree.attention.
	Status      string `json:"status"`
	AcceptedAt  int64  `json:"acceptedAt"`
	AcceptedSeq int    `json:"acceptedSeq"`
}

// NewRun starts a run record in the running state.
func NewRun(id, sessionID, prompt string, cancel func()) *Run {
	return &Run{
		ID:        id,
		SessionID: sessionID,
		Prompt:    prompt,
		status:    RunRunning,
		startedAt: domain.NowMS(),
		cancel:    cancel,
		done:      make(chan struct{}),
		changed:   make(chan struct{}),
		asyncOps:  map[string]*AsyncOperation{},
	}
}

// Done returns a channel closed when the run settles.
func (r *Run) Done() <-chan struct{} { return r.done }

// signalChangeLocked wakes every waiter by closing the current broadcast channel and
// installing a fresh one. Callers hold r.mu.
func (r *Run) signalChangeLocked() {
	r.revision++
	close(r.changed)
	r.changed = make(chan struct{})
}

// Revision is the run's change counter. Capture it before waiting and pass it to
// WaitForChange, which then cannot miss a change that lands in between.
func (r *Run) Revision() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revision
}

// Touch reports an observable change that is not an event of this run's own — a parked
// approval is the one that matters, since a run blocked on a confirmation produces no
// further events at all and would otherwise be invisible until the wait expired.
func (r *Run) Touch() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.signalChangeLocked()
}

// WaitForChange blocks until this run has something new to say past sinceSeq, its
// revision moves off sinceRev, it settles, the budget expires, or the caller gives up.
// It returns as soon as any of those is true, which is what turns `waitMs` into a real
// long poll rather than "wait for finish".
func (r *Run) WaitForChange(ctx context.Context, sinceSeq int, sinceRev uint64, budget time.Duration) {
	r.mu.Lock()
	settled := r.status != RunRunning
	fresh := len(r.events) > 0 && r.events[len(r.events)-1].Seq >= sinceSeq
	moved := r.revision != sinceRev
	changed := r.changed
	r.mu.Unlock()
	// Already something to report: never park a caller over news that has landed —
	// unread events, a settled run, or any change since it took its revision.
	if settled || fresh || moved {
		return
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	// ONE wake, not a loop back to the freshness test. A change that produces no event
	// is exactly the case worth reporting — a run parked on an approval is stopped, not
	// slow, and it emits nothing further of its own — so any signal returns and lets the
	// caller re-read the state for itself.
	select {
	case <-changed:
	case <-timer.C:
	case <-ctx.Done():
	}
}

// AsyncOperations returns the run's background-handle ledger in acceptance order.
func (r *Run) AsyncOperations() []AsyncOperation {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]AsyncOperation, 0, len(r.asyncOrder))
	for _, id := range r.asyncOrder {
		if op, ok := r.asyncOps[id]; ok {
			out = append(out, *op)
		}
	}
	return out
}

// Cancel aborts the run's context if it is still live. Safe to call repeatedly.
func (r *Run) Cancel() {
	r.mu.Lock()
	cancel := r.cancel
	r.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Status reports the current lifecycle state.
func (r *Run) Status() RunStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.status
}

// Snapshot returns the run's outcome plus the events at or after sinceSeq, capped at
// maxEvents. The cap matters: a long orchestration turn produces hundreds of events and
// the caller is an LLM paying context for every one of them, so poll returns a WINDOW
// and reports how much it withheld rather than dumping the lot.
func (r *Run) Snapshot(sinceSeq, maxEvents int) (evs []Event, remaining int, st RunStatus, content, errMsg string, stats domain.JsonRunStats, startedAt, endedAt int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if e.Seq >= sinceSeq {
			evs = append(evs, e)
		}
	}
	if maxEvents > 0 && len(evs) > maxEvents {
		remaining = len(evs) - maxEvents
		evs = evs[:maxEvents]
	}
	return evs, remaining, r.status, r.content, r.errMsg, r.stats, r.startedAt, r.endedAt
}

// settle records the terminal state exactly once and releases anyone waiting on Done.
// A second call is ignored, so a cancelled turn that then reports an error keeps the
// first, more specific classification.
func (r *Run) settle(st RunStatus, content, errMsg string) {
	r.mu.Lock()
	if r.status != RunRunning {
		r.mu.Unlock()
		return
	}
	r.status = st
	r.endedAt = domain.NowMS()
	r.stats.DurationMs = int(r.endedAt - r.startedAt)
	if content != "" {
		r.content = content
	}
	if errMsg != "" {
		r.errMsg = errMsg
	}
	r.cancel = nil
	close(r.done)
	r.signalChangeLocked()
	r.mu.Unlock()
}

// append records one event under the lock, stamping the next seq.
func (r *Run) append(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = len(r.events)
	e.Ts = domain.NowMS()
	r.events = append(r.events, e)
	// Record the handle the instant it is issued, not when a poll window happens to
	// contain the event — the ledger is what makes "this run started background work"
	// survive the caller advancing sinceSeq.
	if e.Async != "" {
		if _, seen := r.asyncOps[e.Async]; !seen {
			r.asyncOps[e.Async] = &AsyncOperation{
				ID: e.Async, Tool: e.Tool, Status: "accepted",
				AcceptedAt: e.Ts, AcceptedSeq: e.Seq,
			}
			r.asyncOrder = append(r.asyncOrder, e.Async)
		}
	}
	r.signalChangeLocked()
}

// Recorder is the agent.EventSink that writes a turn into its Run. It is the MCP
// server's equivalent of the attached session's event pump and the --json sink.
//
// Streamed TOKENS are deliberately dropped: the caller is another agent reading a
// digest, not a human watching prose appear, and re-emitting every token would make a
// poll result enormous for no gain. The authoritative content arrives whole on
// assistant:end.
type Recorder struct {
	run *Run
	// buffer accumulates streamed prose so a round that is interrupted before
	// assistant:end still reports what it had said. Guarded by the run's lock via the
	// append path, but written only from the turn goroutine.
	buffer string

	// candidate is the terminal outcome the STREAM implies, recorded but not committed.
	// A sink can see that the agent emitted a terminal-looking event; only the turn
	// goroutine knows whether Send returned cleanly, whether cancellation won, and
	// whether the post-response bookkeeping finished. Settling from here would open a
	// window where poll answers "success" while Busy() is still true and the very next
	// ask gets ErrBusy — so the sink records evidence and Session.Ask commits it.
	//
	// mu guards it because the commit read happens on the turn goroutine after Send
	// returns while the writes happen on whatever goroutine the agent fanned the sink
	// out on; in practice that is the same goroutine, but the contract does not say so.
	mu        sync.Mutex
	candidate *terminalCandidate
}

// terminalCandidate is the outcome a terminal event implies.
type terminalCandidate struct {
	status  RunStatus
	content string
	errMsg  string
}

// NewRecorder binds a sink to a run.
func NewRecorder(run *Run) *Recorder { return &Recorder{run: run} }

// propose records the first terminal-looking event of the turn. FIRST wins, matching
// the old settle semantics: a cancelled turn that then reports an error keeps the
// earlier, more specific classification.
func (rec *Recorder) propose(st RunStatus, content, errMsg string) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.candidate != nil {
		return
	}
	rec.candidate = &terminalCandidate{status: st, content: content, errMsg: errMsg}
}

// Candidate returns the terminal outcome the stream implied, or nil when the turn
// produced no terminal event at all.
func (rec *Recorder) Candidate() (RunStatus, string, string, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.candidate == nil {
		return "", "", "", false
	}
	return rec.candidate.status, rec.candidate.content, rec.candidate.errMsg, true
}

func (rec *Recorder) flush() {
	if rec.buffer == "" {
		return
	}
	text := rec.buffer
	rec.buffer = ""
	rec.run.append(Event{Type: "assistant:content", Text: text})
}

// FinalizePartial drains whatever prose was streamed but never terminated, and returns
// it. Called by the turn goroutine on an exit path that produced NO terminal event.
//
// Without it the buffer was simply dropped. The comment on `buffer` promised that a round
// interrupted before assistant:end "still reports what it had said", and that was true
// only for the paths that happened to call flush() — a new AssistantStart, an
// interjection, a skill load. A turn cancelled or errored mid-sentence hit none of them,
// so the one case the buffer exists for was the one it did not cover.
//
// It runs on the turn goroutine after Send has returned, which is the same goroutine the
// sink callbacks ran on, so the unsynchronized buffer read is safe here in a way it would
// not be from a poll handler.
func (rec *Recorder) FinalizePartial() string {
	if rec.buffer == "" {
		return ""
	}
	text := rec.buffer
	rec.buffer = ""
	rec.run.append(Event{Type: "assistant:partial", Text: text})
	return text
}

// --- agent.EventSink ---

// Phase, ToolBatch, ToolState and ToolProgress are live-footer vocabulary for a human
// watching an attached session. A polling agent gets nothing from them.
func (rec *Recorder) Phase(domain.RunPhase)             {}
func (rec *Recorder) ToolBatch([]agent.BatchedToolCall) {}
func (rec *Recorder) ToolState(string, agent.ToolState) {}
func (rec *Recorder) ToolProgress(string, string)       {}
func (rec *Recorder) TurnPrompt(string)                 {}
func (rec *Recorder) ModelRateLimited()                 {}

func (rec *Recorder) AssistantStart() {
	rec.flush()
	rec.run.mu.Lock()
	rec.run.stats.Rounds++
	rec.run.mu.Unlock()
	rec.run.append(Event{Type: "assistant:start"})
}

func (rec *Recorder) AssistantToken(token string) { rec.buffer += token }

func (rec *Recorder) AssistantEnd(content, _ string) {
	rec.buffer = "" // the authoritative content supersedes the streamed duplicate
	rec.run.append(Event{Type: "assistant:end", Text: content})
	rec.propose(RunSucceeded, content, "")
}

// AssistantCancelled records an interrupted turn.
//
// The buffer is dropped ONLY when the cancellation carried content of its own. The real
// agent emits AssistantCancelled("") — it has no final answer to give — and clearing the
// buffer there threw away the one account of what the turn had been saying, which is
// precisely what the buffer exists to keep. The partial is promoted to the cancellation's
// content instead, so a caller reading the run sees the prose rather than a sentinel.
func (rec *Recorder) AssistantCancelled(content string) {
	if content == "" {
		content = rec.buffer
	}
	rec.buffer = ""
	rec.run.append(Event{Type: "assistant:cancelled", Text: content})
	rec.propose(RunCancelled, content, "")
}

func (rec *Recorder) Interjection(text string) {
	rec.flush()
	rec.run.append(Event{Type: "user:interjection", Text: text})
}

func (rec *Recorder) SkillLoaded(titles []string) {
	rec.flush()
	rec.run.append(Event{Type: "skill:loaded", Skills: titles})
}

// SkillDecision records the committed per-round outcome. Only the active TITLES and the
// degraded flag are kept: this transcript is a digest an agent driving us reads back (it
// already drops tool args for the same reason), and those two answer the question a
// caller actually has — which runbook was in play, and was it really chosen. The ids,
// the newly-loaded delta and the rest of the selector telemetry live on the --json
// stream, which is the full diagnostic contract.
func (rec *Recorder) SkillDecision(ev agent.SkillDecisionEvent) {
	rec.flush()
	titles := make([]string, 0, len(ev.Active))
	for _, ref := range ev.Active {
		title := strings.TrimSpace(ref.Title)
		if title == "" {
			title = strings.TrimSpace(ref.ID)
		}
		if title == "" {
			continue
		}
		titles = append(titles, title)
	}
	rec.run.append(Event{Type: "skill:decision", Skills: titles, SkillsDegraded: ev.Selector.Degraded})
}

func (rec *Recorder) ToolCall(ev agent.ToolCallEvent) {
	rec.flush()
	rec.run.mu.Lock()
	rec.run.stats.ToolCalls++
	rec.run.mu.Unlock()
	// Args are NOT recorded. They can be large (a whole file, a prompt for a spawned
	// agent) and the caller already knows what it asked for; the tool name plus the
	// result summary is the digest that earns its context.
	rec.run.append(Event{Type: "tool:call", Tool: ev.Name, CallID: ev.ID})
}

func (rec *Recorder) ToolResult(ev agent.ToolResultEvent) {
	ok := ev.Result.Ok
	e := Event{Type: "tool:result", Tool: ev.Name, CallID: ev.ID, Ok: &ok, Summary: ev.Result.Summary}
	if ev.Result.Error != nil {
		e.Error = ev.Result.Error.Code + ": " + ev.Result.Error.Message
	}
	if ev.Result.Async != nil {
		e.Async = ev.Result.Async.ID
	}
	rec.run.mu.Lock()
	if !ok {
		rec.run.stats.ToolErrors++
	}
	rec.run.mu.Unlock()
	rec.run.append(e)
}

func (rec *Recorder) Error(message string) {
	// Whatever was streamed before the failure is the account of what the turn was
	// doing. It is flushed as its own event AND carried as the candidate's content, so a
	// caller polling the run gets the prose rather than only the error sentinel.
	partial := rec.buffer
	rec.flush()
	rec.run.append(Event{Type: "error", Text: message})
	if partial != "" {
		rec.mu.Lock()
		if rec.candidate == nil {
			rec.candidate = &terminalCandidate{status: RunFailed, content: partial, errMsg: message}
			rec.mu.Unlock()
			return
		}
		rec.mu.Unlock()
	}
	// An Error event is fatal for the turn but Send still returns normally (turn
	// failures are sentinel replies, not errors), so record it as the terminal
	// candidate or the run would settle `success` off the backstop.
	rec.propose(RunFailed, "", message)
}

func (rec *Recorder) Warn(message string) {
	rec.flush()
	rec.run.append(Event{Type: "warning", Text: message})
}

func (rec *Recorder) Info(message string) {
	rec.flush()
	rec.run.append(Event{Type: "info", Text: message})
}

func (rec *Recorder) Usage(ev agent.UsageEvent) {
	rec.run.mu.Lock()
	defer rec.run.mu.Unlock()
	rec.run.stats.PromptTokens += ev.PromptTokens
	rec.run.stats.CompletionTokens += ev.CompletionTokens
	rec.run.stats.TotalTokens += ev.TotalTokens
	if ev.ContextTokens > 0 {
		rec.run.stats.ContextTokens = ev.ContextTokens
	}
}
