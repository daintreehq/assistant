// Package jsonout is the one-shot JSONL sink, schema
// v1. It implements agent.EventSink: every event becomes one stdout line
// {type, ts, seq, ...payload}; finish() writes the terminal `result` envelope and
// returns the process exit code.
//
// STDOUT PURITY: in --json mode stdout carries ONLY these JSONL lines. The caller
// routes every human/diagnostic line (errors, warnings, logs) to stderr.
package jsonout

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// Clock returns the current epoch-ms; injectable for tests.
type Clock func() int64

// SessionInfo is the payload of the one-time `session` line. It is an ALIAS of the
// domain contract, not a copy: the keys are declared once, in domain, and the sink
// marshals that struct instead of hand-rolling a map — so the wire shape cannot drift
// from the documented contract without the compiler noticing.
type SessionInfo = domain.JsonSessionPayload

// Stats is the accounting block on the terminal `result` line. Same reasoning as
// SessionInfo: one declaration, in domain.
type Stats = domain.JsonRunStats

// Sink is the JSONL event sink + terminal-envelope writer. Construct with New.
type Sink struct {
	out io.Writer
	now Clock

	mu            sync.Mutex
	seq           int
	startedAt     int64
	contentBuffer string
	content       string
	// status/exitCode/errorMessage are TURN-scoped once multiTurn is on and run-scoped
	// otherwise. They are the same fields either way, because a single-prompt run is
	// exactly a one-turn run: the aggregate below folds the one turn and reproduces
	// today's terminal line byte for byte.
	status         domain.JsonOutputStatus
	exitCode       int
	errorMessage   *string
	finished       bool
	sessionEmitted bool
	stats          Stats

	// multiTurn arms the turn bracket. Off, BeginTurn/SettleTurn/CommandResult are
	// never called and the sink is precisely the one-shot sink it has always been.
	multiTurn bool
	// turn is the NEXT turn's zero-based index; turnOpen says a bracket is unclosed.
	turn     int
	turnOpen bool

	// The run-level aggregate, folded from each turn as it ends. Separate from the
	// turn-level fields above because AssistantEnd RESETS those to success and clears
	// errorMessage — correct within a turn (a round can fail and the retry succeed),
	// catastrophic across turns, where it would let turn 3 report success for a run
	// whose turn 2 failed. The issue is explicit: a run where turn two failed is a
	// failed run.
	runStatus       domain.JsonOutputStatus
	runExitCode     int
	runErrorMessage *string
	runOutcomeSet   bool
}

// New builds a Sink writing to w. The default terminal state is "error"/exit 1: a
// turn that ends with no explicit terminal event is itself a failure.
func New(w io.Writer, now Clock) *Sink {
	if now == nil {
		now = domain.NowMS
	}
	return &Sink{
		out:       w,
		now:       now,
		startedAt: now(),
		status:    domain.JSONStatusError,
		exitCode:  domain.OneShotExitCode.Error,
	}
}

// NewMultiTurn builds a Sink for a --multi-turn run: same wire schema, same single
// `session` header, same single terminal `result`, plus the turn bracket and the
// run-level outcome latch. The mode is a CONSTRUCTOR choice rather than a setter so a
// sink cannot change shape halfway through a stream.
func NewMultiTurn(w io.Writer, now Clock) *Sink {
	s := New(w, now)
	s.multiTurn = true
	return s
}

// outcome is a terminal verdict: the triple that ends up on the `result` line.
type outcome struct {
	status   domain.JsonOutputStatus
	exitCode int
	message  *string
}

// worse merges a turn's outcome into the run's, worst-wins: error > cancelled > success.
// It is the whole of "a run where turn two failed is a failed run" — and its mirror, "a
// later success does not forgive an earlier failure".
//
// Pure, so Finish and ExitCode answer the same question without either mutating state
// the other depends on, and so the precedence rule is stated exactly once.
//
// The error MESSAGE is last-writer-wins among failures, matching how the single-turn
// sink already behaves when two Error events land in one run: the most recent failure is
// the one a reader is looking at.
func worse(run, turn outcome) outcome {
	// Nothing outranks an error, and its message says more than "cancelled" does — but a
	// LATER error still replaces the message.
	if run.status == domain.JSONStatusError {
		if turn.status == domain.JSONStatusError {
			return turn
		}
		return run
	}
	switch turn.status {
	case domain.JSONStatusError, domain.JSONStatusCancelled:
		return turn
	}
	return run
}

// turnOutcome is the live turn's verdict; runOutcome is the aggregate folded so far.
// Caller holds mu.
func (s *Sink) turnOutcome() outcome {
	return outcome{status: s.status, exitCode: s.exitCode, message: s.errorMessage}
}

// runOutcome is the run's verdict INCLUDING the live turn — the answer both Finish and
// ExitCode want. Before any turn has been folded it is simply the live turn, which is
// what makes a single-prompt run's terminal line identical to what it always was.
// Caller holds mu.
func (s *Sink) runOutcome() outcome {
	if !s.runOutcomeSet {
		return s.turnOutcome()
	}
	return worse(outcome{status: s.runStatus, exitCode: s.runExitCode, message: s.runErrorMessage}, s.turnOutcome())
}

// foldOutcome merges one turn's outcome into the run-level aggregate. Caller holds mu.
func (s *Sink) foldOutcome(o outcome) {
	if !s.runOutcomeSet {
		s.runOutcomeSet = true
		s.runStatus, s.runExitCode, s.runErrorMessage = o.status, o.exitCode, o.message
		return
	}
	merged := worse(outcome{status: s.runStatus, exitCode: s.runExitCode, message: s.runErrorMessage}, o)
	s.runStatus, s.runExitCode, s.runErrorMessage = merged.status, merged.exitCode, merged.message
}

// BeginTurn opens a turn bracket: it emits `turn:prompt` and resets the TURN-level
// outcome to the same default New uses, so a turn that produces no terminal assistant
// event is a failed turn exactly as a whole one-shot run would be.
//
// Called by the multi-turn loop rather than driven off the EventSink's TurnPrompt hook,
// which the Session also fires once per send. One owner for the bracket — the loop that
// opens and closes it — means the stream cannot grow a half-bracket if the session's
// internal event ordering ever changes, and it keeps this sink a passive recorder of
// everything it does not itself own.
//
// A no-op outside multi-turn mode.
func (s *Sink) BeginTurn(prompt string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.multiTurn {
		return
	}
	s.flushContent()
	s.status = domain.JSONStatusError
	s.exitCode = domain.OneShotExitCode.Error
	s.errorMessage = nil
	s.content = ""
	s.turnOpen = true
	s.emitStruct(string(domain.JsonlTurnPrompt), domain.JsonTurnPromptPayload{Turn: s.turn, Prompt: prompt})
}

// SettleTurn closes the open bracket: it emits `turn:end` with the turn's own status and
// folds that outcome into the run aggregate. Idempotent — with no bracket open it does
// nothing, so the loop can defer it unconditionally.
func (s *Sink) SettleTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settleTurnLocked()
}

// settleTurnLocked is SettleTurn's body; Finish calls it to close a dangling bracket.
func (s *Sink) settleTurnLocked() {
	if !s.multiTurn || !s.turnOpen {
		return
	}
	s.flushContent()
	s.turnOpen = false
	s.emitStruct(string(domain.JsonlTurnEnd), domain.JsonTurnEndPayload{Turn: s.turn, Status: s.status})
	s.foldOutcome(s.turnOutcome())
	s.turn++
}

// CommandResult emits one `command:result` line for a slash command run between turns.
// A command opens no bracket, moves no turn number, and never touches the run outcome.
func (s *Sink) CommandResult(payload domain.JsonCommandResultPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.multiTurn {
		return
	}
	s.flushContent()
	s.emitStruct(string(domain.JsonlCommandResult), payload)
}

// Session emits the one-time `session` line. Call it once, as early as the facts are
// known; a second call is dropped so a retry or a re-wired hook cannot produce two
// conflicting headers.
func (s *Sink) Session(info SessionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionEmitted {
		return
	}
	s.sessionEmitted = true
	// Stamped here rather than left to every caller: the point of putting the version
	// on the first frame is that a consumer can ALWAYS rely on it being there.
	info.SchemaVersion = domain.JSONOutputSchemaVersion
	s.emitStruct("session", info)
}

// emit builds {type, ts, seq, ...payload}, marshals it, and writes one line. After
// finish() no line is emitted (dropped). On a marshal failure a degraded but valid
// line is written keeping the seq (a gap would break the monotonic-seq contract).
func (s *Sink) emit(typ string, payload map[string]any) {
	if s.finished {
		return
	}
	// schemaVersion on EVERY frame, not just the session header and the terminal
	// result. Stamping it only on the session line left the documented "reject an
	// incompatible schema at frame one" guarantee false for the case that needs it
	// most: a setup failure — bad arguments, an unreadable key file, a busy lease —
	// emits `error` before any session frame exists, so a strict consumer met an
	// unversioned line first.
	line := map[string]any{"type": typ, "ts": s.now(), "seq": s.seq, "schemaVersion": domain.JSONOutputSchemaVersion}
	s.seq++
	for k, v := range payload {
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		degraded := map[string]any{
			"type": typ, "ts": line["ts"], "seq": line["seq"], "serializationError": true,
			"schemaVersion": domain.JSONOutputSchemaVersion,
		}
		b, _ = json.Marshal(degraded)
	}
	fmt.Fprintln(s.out, string(b))
}

// emitStruct emits a line whose payload is a STRUCT rather than a hand-written map, so
// the keys on the wire are exactly the struct's json tags. The round trip through a map
// is deliberate: it keeps the {type, ts, seq, ...payload} flattening in one place
// (emit), while removing the class of bug where a contract type and the map that is
// actually emitted drift apart silently. A payload that cannot marshal degrades through
// emit's own path rather than throwing.
func (s *Sink) emitStruct(typ string, payload any) {
	b, err := json.Marshal(payload)
	if err != nil {
		s.emit(typ, map[string]any{"serializationError": true})
		return
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		s.emit(typ, map[string]any{"serializationError": true})
		return
	}
	s.emit(typ, m)
}

// flushContent emits the buffered streamed prose as one assistant:content line.
func (s *Sink) flushContent() {
	if s.contentBuffer == "" {
		return
	}
	buf := s.contentBuffer
	s.contentBuffer = ""
	s.emit("assistant:content", map[string]any{"content": buf})
}

// --- agent.EventSink implementation ---

// Phase is live-only UI vocabulary; not part of the JSONL stream.
func (s *Sink) Phase(domain.RunPhase) {}

func (s *Sink) AssistantStart() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	// One assistant:start per model generation, so this IS the round count.
	s.stats.Rounds++
	s.emit("assistant:start", nil)
}

func (s *Sink) AssistantToken(token string) {
	s.mu.Lock()
	s.contentBuffer += token
	s.mu.Unlock()
}

func (s *Sink) AssistantEnd(content, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contentBuffer = "" // drop the streamed dup; content arrives authoritative here
	s.content = content
	s.status = domain.JSONStatusSuccess
	s.exitCode = domain.OneShotExitCode.Success
	s.errorMessage = nil
	s.emit("assistant:end", map[string]any{"content": content})
}

func (s *Sink) AssistantCancelled(content string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.contentBuffer = ""
	s.content = content
	s.status = domain.JSONStatusCancelled
	s.exitCode = domain.OneShotExitCode.Cancelled
	s.emit("assistant:cancelled", map[string]any{"content": content})
}

// CancelRun marks the RUN cancelled without touching the turn. It is for a bound
// that expires AFTER the assistant already finished — a --run-scheduler one-shot
// whose --timeout fires while it waits for async work to settle — where the answer
// is real and must survive into the terminal `result` line.
//
// Deliberately not AssistantCancelled: that emits an `assistant:cancelled` event,
// and a consumer that already saw `assistant:end` for this turn would then see two
// terminal assistant events for one turn. This only moves the run-level status and
// exit code, so the cancellation shows up exactly once, in `result`.
//
// It never downgrades a run that already failed: an error status carries a message
// and a non-zero code that say more than "cancelled" does.
//
// The message is what makes that true, and so the message is what the guard tests. A
// sink's DEFAULT status is also "error" — the pessimistic sentinel meaning "no terminal
// event has happened yet" — and that one carries nothing at all. Guarding on the status
// alone conflated the two and made this method a silent no-op in exactly the case it
// exists for: a bound expiring before anything ran, which then reported a bare
// error/exit-1 with a null message instead of the cancellation it was.
func (s *Sink) CancelRun() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == domain.JSONStatusError && s.errorMessage != nil {
		return
	}
	// Fold at the RUN level too, not just the turn level. In multi-turn mode this can
	// fire between turns, where the live turn-level fields are about to be reset by the
	// next BeginTurn and would carry the cancellation nowhere.
	s.status = domain.JSONStatusCancelled
	s.exitCode = domain.OneShotExitCode.Cancelled
	s.foldOutcome(outcome{status: domain.JSONStatusCancelled, exitCode: domain.OneShotExitCode.Cancelled})
}

// Interjection emits a mid-turn user message as its own JSONL line (flushing buffered
// prose first so it lands after the round it interrupted).
func (s *Sink) Interjection(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	s.emit("user:interjection", map[string]any{"text": text})
}

// RunbookLoaded emits a server-side runbook load as its own JSONL line (flushing buffered
// prose first so it lands at the round boundary where the runbook was selected).
func (s *Sink) RunbookLoaded(titles []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	s.emit("runbook:loaded", map[string]any{"titles": titles})
}

// RunbookDecision emits the committed per-round runbook outcome. Unlike every other payload
// on this sink it is marshalled from the event STRUCT rather than a hand-rolled map (the
// emitStruct path, as the `session` line uses): the keys a consumer asserts on are then
// declared exactly once, on agent.RunbookDecisionEvent, and cannot drift from what is
// documented without the compiler noticing. Those tags are camelCase to match the rest of
// this stream, deliberately NOT the snake_case the backend sends on the wire.
//
// Additive: runbook:loaded keeps its shape, so this needs no JSONOutputSchemaVersion bump.
// A consumer must switch on `type` and tolerate line types it does not know — the extra
// line does shift every later `seq`.
func (s *Sink) RunbookDecision(ev agent.RunbookDecisionEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent() // prose precedes the round boundary this decision belongs to
	s.emitStruct("runbook:decision", ev)
}

// ToolBatch / ToolState / ToolProgress are live-footer-only; not part of the JSONL stream.
func (s *Sink) ToolBatch([]agent.BatchedToolCall) {}
func (s *Sink) ToolState(string, agent.ToolState) {}
func (s *Sink) ToolProgress(string, string)       {}

func (s *Sink) ToolCall(ev agent.ToolCallEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent() // prose precedes the call
	s.stats.ToolCalls++
	s.emit("tool:call", map[string]any{"id": ev.ID, "name": ev.Name, "args": rawArgsAsAny(ev.Args)})
}

func (s *Sink) ToolResult(ev agent.ToolResultEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errPayload any
	if ev.Result.Error != nil {
		errPayload = ev.Result.Error
	}
	// Counted off Ok, not off the presence of an error payload: a failed result is
	// recoverable model context, so it never changes the run's exit code — but "the
	// answer arrived after six tool failures" is exactly what a harness wants to gate on.
	if !ev.Result.Ok {
		s.stats.ToolErrors++
	}
	payload := map[string]any{
		"id":      ev.ID,
		"name":    ev.Name,
		"ok":      ev.Result.Ok,
		"summary": ev.Result.Summary,
		"error":   errPayload, // null on success
	}
	if ev.Result.AuditID != "" {
		payload["auditId"] = ev.Result.AuditID
	}
	// An accepted async handle: mark the line so a JSONL consumer can tell an
	// "accepted, still running in the background" result from a finished one.
	if ev.Result.Async != nil {
		payload["async"] = ev.Result.Async
	}
	s.emit("tool:result", payload)
}

func (s *Sink) Error(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	s.status = domain.JSONStatusError
	s.exitCode = domain.OneShotExitCode.Error
	m := message
	s.errorMessage = &m
	s.emit("error", map[string]any{"message": message})
}

// Warn emits a "warning" line. Unlike Error it does NOT flip the terminal status/exit
// code — a warning (e.g. a tool loop repeating the same failure) is non-fatal.
func (s *Sink) Warn(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit("warning", map[string]any{"message": message})
}

func (s *Sink) Info(message string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emit("info", map[string]any{"message": message})
}

// Usage does not get a line of its own — per-round token accounting would be noise in
// a stream whose consumer wants the total — but it IS accumulated into the terminal
// envelope's stats. Tokens sum across rounds; ContextTokens is deliberately the LAST
// round's prompt size rather than a sum, because that is the compaction-pressure
// figure.
func (s *Sink) Usage(ev agent.UsageEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.PromptTokens += ev.PromptTokens
	s.stats.CompletionTokens += ev.CompletionTokens
	s.stats.TotalTokens += ev.TotalTokens
	if ev.ContextTokens > 0 {
		s.stats.ContextTokens = ev.ContextTokens
	}
}

// TurnPrompt is durable-log-only vocabulary (persisted for /explain); the JSONL
// one-shot stream does not echo it.
func (s *Sink) TurnPrompt(string) {}

// ModelRateLimited is a live-only health cue; not part of the JSONL stream. The
// "Model rate-limited" reply still lands as the terminal envelope's content.
func (s *Sink) ModelRateLimited() {}

// Finish writes the terminal `result` envelope (idempotent) and returns the exit
// code. After Finish no further line is emitted.
func (s *Sink) Finish() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.finished {
		return s.exitCode
	}
	// Close a bracket the loop left open (a turn cut short by the deadline), so a
	// consumer never sees a turn:prompt without its turn:end.
	s.settleTurnLocked()
	s.flushContent()
	// Report the RUN's verdict, which folds the live turn-level state in one last time.
	//
	// Single-prompt runs go through this same path and are unchanged by it: with nothing
	// folded yet the run's verdict IS the turn's. It is also what keeps a post-turn
	// CancelRun — the --run-scheduler async barrier expiring after the answer already
	// landed — reaching the terminal line in multi-turn mode, where the last turn has
	// already been folded once. Re-folding the same outcome is idempotent.
	final := s.runOutcome()
	s.status, s.exitCode, s.errorMessage = final.status, final.exitCode, final.message
	var errObj any
	if s.errorMessage != nil {
		errObj = map[string]any{"message": *s.errorMessage}
	}
	// Wall clock, not a monotonic one (the Clock seam is epoch-ms so lines carry a real
	// timestamp). An NTP step or a manual clock change between New and Finish would
	// otherwise report a NEGATIVE duration, which reads as corruption rather than as the
	// clock adjustment it is. Clamp: an understated duration is a small lie, a negative
	// one breaks arithmetic downstream.
	if elapsed := s.now() - s.startedAt; elapsed > 0 {
		s.stats.DurationMs = int(elapsed)
	}
	s.emit("result", map[string]any{
		"schemaVersion": domain.JSONOutputSchemaVersion,
		"status":        s.status,
		"exitCode":      s.exitCode,
		"content":       s.content,
		"error":         errObj, // null pointer → JSON null
		"stats":         s.stats,
	})
	s.finished = true
	return s.exitCode
}

// ExitCode returns the current terminal exit code without finishing — the RUN's, which
// in multi-turn mode already carries an earlier turn's failure.
func (s *Sink) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.runOutcome().exitCode
}

// rawArgsAsAny decodes the raw JSON args string to a generic value, falling back
// to the raw string when it isn't valid JSON.
func rawArgsAsAny(raw string) any {
	if raw == "" {
		return map[string]any{}
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return v
}
