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
	"strings"
	"sync"

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
	}
}

// Done returns a channel closed when the run settles.
func (r *Run) Done() <-chan struct{} { return r.done }

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
	r.mu.Unlock()
}

// append records one event under the lock, stamping the next seq.
func (r *Run) append(e Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e.Seq = len(r.events)
	e.Ts = domain.NowMS()
	r.events = append(r.events, e)
}

// Recorder is the agent.EventSink that writes a turn into its Run. It is the MCP
// server's equivalent of the cockpit's event pump and the --json sink.
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
}

// NewRecorder binds a sink to a run.
func NewRecorder(run *Run) *Recorder { return &Recorder{run: run} }

func (rec *Recorder) flush() {
	if rec.buffer == "" {
		return
	}
	text := rec.buffer
	rec.buffer = ""
	rec.run.append(Event{Type: "assistant:content", Text: text})
}

// --- agent.EventSink ---

// Phase, ToolBatch, ToolState and ToolProgress are live-footer vocabulary for a human
// watching a cockpit. A polling agent gets nothing from them.
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
	rec.run.settle(RunSucceeded, content, "")
}

func (rec *Recorder) AssistantCancelled(content string) {
	rec.buffer = ""
	rec.run.append(Event{Type: "assistant:cancelled", Text: content})
	rec.run.settle(RunCancelled, content, "")
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
	rec.flush()
	rec.run.append(Event{Type: "error", Text: message})
	// An Error event is fatal for the turn but Send still returns normally (turn
	// failures are sentinel replies, not errors), so settle here or the run would sit
	// in `running` until the caller gave up.
	rec.run.settle(RunFailed, "", message)
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
