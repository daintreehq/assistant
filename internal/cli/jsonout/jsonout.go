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

	mu             sync.Mutex
	seq            int
	startedAt      int64
	contentBuffer  string
	content        string
	status         domain.JsonOutputStatus
	exitCode       int
	errorMessage   *string
	finished       bool
	sessionEmitted bool
	stats          Stats
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
	s.emitStruct("session", info)
}

// emit builds {type, ts, seq, ...payload}, marshals it, and writes one line. After
// finish() no line is emitted (dropped). On a marshal failure a degraded but valid
// line is written keeping the seq (a gap would break the monotonic-seq contract).
func (s *Sink) emit(typ string, payload map[string]any) {
	if s.finished {
		return
	}
	line := map[string]any{"type": typ, "ts": s.now(), "seq": s.seq}
	s.seq++
	for k, v := range payload {
		line[k] = v
	}
	b, err := json.Marshal(line)
	if err != nil {
		degraded := map[string]any{
			"type": typ, "ts": line["ts"], "seq": line["seq"], "serializationError": true,
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

// Interjection emits a mid-turn user message as its own JSONL line (flushing buffered
// prose first so it lands after the round it interrupted).
func (s *Sink) Interjection(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	s.emit("user:interjection", map[string]any{"text": text})
}

// SkillLoaded emits a server-side skill load as its own JSONL line (flushing buffered
// prose first so it lands at the round boundary where the skill was selected).
func (s *Sink) SkillLoaded(titles []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent()
	s.emit("skill:loaded", map[string]any{"titles": titles})
}

// SkillDecision emits the committed per-round skill outcome. Unlike every other payload
// on this sink it is marshalled from the event STRUCT rather than a hand-rolled map (the
// emitStruct path, as the `session` line uses): the keys a consumer asserts on are then
// declared exactly once, on agent.SkillDecisionEvent, and cannot drift from what is
// documented without the compiler noticing. Those tags are camelCase to match the rest of
// this stream, deliberately NOT the snake_case the backend sends on the wire.
//
// Additive: skill:loaded keeps its shape, so this needs no JSONOutputSchemaVersion bump.
// A consumer must switch on `type` and tolerate line types it does not know — the extra
// line does shift every later `seq`.
func (s *Sink) SkillDecision(ev agent.SkillDecisionEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushContent() // prose precedes the round boundary this decision belongs to
	s.emitStruct("skill:decision", ev)
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
	s.flushContent()
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

// ExitCode returns the current terminal exit code without finishing.
func (s *Sink) ExitCode() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode
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
