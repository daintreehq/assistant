// Package agent drives the main-thread agentic turn loop. AgentSession runs ONE
// user (or autonomous) turn to completion: optional auto-compact → skill
// re-selection → assemble the three control messages → stream the large model →
// dispatch tool calls → feed results back (looping until the model stops calling
// tools; no per-turn round cap). It owns conversation
// persistence/rehydration, the repeated-failure circuit breaker, oversized-tool-
// result truncation into session artifacts, and a liveness-rich structured event
// stream.
package agent

import (
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// --- Event vocabulary (liveness) ---

// ToolCallEvent describes a tool invocation the loop is about to run. Args is the
// raw JSON-arguments string the model emitted (kept verbatim — the UI parses it).
// StartedAt/EndedAt are epoch-ms.
type ToolCallEvent struct {
	ID        string
	Name      string
	Args      string
	StartedAt int64
}

// ToolResultEvent is the settled result for a ToolCallEvent (matched by ID).
type ToolResultEvent struct {
	ID      string
	Name    string
	Result  domain.ToolResult
	EndedAt int64
	// FailureCount is the session-cumulative number of times THIS tool name has failed
	// (this result included), 0 on a successful result. A cheap drift signal (issue
	// #251) surfaced off the dispatch result, not the audit path.
	FailureCount int
}

// BatchedToolCall is one entry in a ToolBatch announcement: a queued tool call,
// announced before sequential dispatch so the live footer can show the whole
// batch at once.
type BatchedToolCall struct {
	ID   string
	Name string
	Args string
}

// ToolState is the per-call lifecycle the footer renders (glyphs).
type ToolState string

const (
	ToolStateQueued  ToolState = "queued"
	ToolStateActive  ToolState = "active"
	ToolStateWaiting ToolState = "waiting" // awaiting approval
	ToolStateDone    ToolState = "done"
	ToolStateFailed  ToolState = "failed"
)

// UsageEvent is the per-round token/cost/context-pressure accounting. Emitted
// once per streamed round, AFTER the call returns and BEFORE the assistant
// message is appended (so ContextTokens reflects the prompt sent).
type UsageEvent struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     *int
	// CacheHitRatio is the prompt-cache efficiency signal (issue #262): the share of
	// prompt tokens served from the provider's cache, cachedTotal/PromptTokens across
	// ALL tiers — numerator and denominator both cross-tier so the ratio can't exceed
	// 1.0 from a tier mismatch. nil whenever CachedTokens is nil (no tier reported) or
	// PromptTokens is 0, so the UI shows "no data" rather than a misleading 0.0. NOT
	// clamped: a provider anomaly (cached > prompt) stays visible as a diagnostic.
	CacheHitRatio    *float64
	ContextTokens    int
	ContextThreshold int // auto-compact trigger point (NOT the gauge denominator)
	ContextWindow    int // model context window — the CTX% gauge denominator
	// CompactionDepth is how many times the session has compacted so far (issue #251).
	// A drift signal: rising depth over a long run flags a summary-of-summary chain
	// degrading detail. Session-scoped; resets to 0 on /clear.
	CompactionDepth int
	// CostUsd is nil when the provider reported no usage ("no data"), never a
	// misleading $0.000.
	CostUsd *float64
	Tier    string
	Model   string
	// Tiers is the per-(tier,model) breakdown the aggregate above is summed from
	// (large stream + small-tier background work). nil when nothing was metered
	// this round. The UI footer reads the aggregate; the durable run-event log
	// captures the breakdown.
	Tiers []models.TierUsage
}

// EventSink is the structured-event vocabulary the loop emits. The cockpit's
// LiveRunStatus is driven from Phase(), never inferred from "is text empty". Go
// has no optional interface methods, so Usage is required; sinks that don't care
// no-op it.
type EventSink interface {
	// Phase drives the explicit run lifecycle (RunPhase). Cockpit liveness reads
	// this, NEVER an emptiness heuristic.
	Phase(p domain.RunPhase)

	AssistantStart()                        // a new round is about to stream
	AssistantToken(token string)            // one streamed visible token (think-stripped)
	AssistantEnd(content, reasoning string) // final round; reasoning = <think> body ("" when none)
	AssistantCancelled(content string)      // user abort mid-flight; content often ""

	// Interjection reports a message the human typed WHILE the turn was running, at the
	// moment the loop folds it into history (the next tool-iteration boundary, see
	// Session.foldInInjections). The cockpit renders it inline in the running turn; the
	// durable log records it so /explain replay shows the mid-turn steer.
	Interjection(text string)

	// ToolBatch announces every parsed tool call as queued BEFORE sequential
	// dispatch begins. The loop then promotes each call.
	ToolBatch(calls []BatchedToolCall)
	// ToolState promotes one announced call (queued→active→done/failed/waiting).
	ToolState(id string, state ToolState)
	// ToolProgress carries an in-tool substep ("launching terminal") for the active
	// call so the live footer never looks frozen. The id is
	// the tool call id so the UI maps it to the right activity row; msg is "" when a
	// progress beat carries only a fraction (the UI keeps the prior message).
	ToolProgress(id string, msg string)

	ToolCall(ev ToolCallEvent)     // a call is starting (with parsed/raw args)
	ToolResult(ev ToolResultEvent) // a call settled

	Error(message string) // fatal-for-this-turn
	Warn(message string)  // non-fatal warning (e.g. a tool loop repeating the same failure)
	Info(message string)  // informational
	Usage(ev UsageEvent)  // per-round token accounting (no-op in sinks that ignore)

	// TurnPrompt records the turn's originating user prompt as the run's first
	// durable event, so /explain can label runs by what prompted them. Emitted
	// once per turn, BEFORE AssistantStart. Live-only sinks no-op it (it carries no
	// liveness — only the durable RunEventSink persists it).
	TurnPrompt(input string)

	// ModelRateLimited signals the provider throttled us after the retry budget was
	// exhausted on a 429. A live-only health cue: the cockpit raises a "▲ Model
	// rate-limited" badge that clears on the next successful Usage. No-op in sinks
	// that don't render persistent health state.
	ModelRateLimited()
}

// NoopEventSink discards every event. Default sink and test stand-in.
type NoopEventSink struct{}

func (NoopEventSink) Phase(domain.RunPhase)       {}
func (NoopEventSink) AssistantStart()             {}
func (NoopEventSink) AssistantToken(string)       {}
func (NoopEventSink) AssistantEnd(string, string) {}
func (NoopEventSink) AssistantCancelled(string)   {}
func (NoopEventSink) Interjection(string)         {}
func (NoopEventSink) ToolBatch([]BatchedToolCall) {}
func (NoopEventSink) ToolState(string, ToolState) {}
func (NoopEventSink) ToolProgress(string, string) {}
func (NoopEventSink) ToolCall(ToolCallEvent)      {}
func (NoopEventSink) ToolResult(ToolResultEvent)  {}
func (NoopEventSink) Error(string)                {}
func (NoopEventSink) Warn(string)                 {}
func (NoopEventSink) Info(string)                 {}
func (NoopEventSink) Usage(UsageEvent)            {}
func (NoopEventSink) TurnPrompt(string)           {}
func (NoopEventSink) ModelRateLimited()           {}

// MultiSink fans every event out to several sinks, each isolated by panic
// recovery so one misbehaving sink (e.g. a UI bridge) can never break the loop or
// starve the others.
type MultiSink struct {
	sinks []EventSink
}

// NewMultiSink builds a MultiSink (nil entries skipped).
func NewMultiSink(sinks ...EventSink) *MultiSink {
	filtered := make([]EventSink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			filtered = append(filtered, s)
		}
	}
	return &MultiSink{sinks: filtered}
}

func fanOut(s EventSink, call func(EventSink)) {
	defer func() { _ = recover() }()
	call(s)
}

func (m *MultiSink) Phase(p domain.RunPhase) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Phase(p) })
	}
}
func (m *MultiSink) AssistantStart() {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.AssistantStart() })
	}
}
func (m *MultiSink) AssistantToken(t string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.AssistantToken(t) })
	}
}
func (m *MultiSink) AssistantEnd(content, reasoning string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.AssistantEnd(content, reasoning) })
	}
}
func (m *MultiSink) AssistantCancelled(content string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.AssistantCancelled(content) })
	}
}
func (m *MultiSink) Interjection(text string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Interjection(text) })
	}
}
func (m *MultiSink) ToolBatch(calls []BatchedToolCall) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ToolBatch(calls) })
	}
}
func (m *MultiSink) ToolState(id string, state ToolState) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ToolState(id, state) })
	}
}
func (m *MultiSink) ToolProgress(id string, msg string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ToolProgress(id, msg) })
	}
}
func (m *MultiSink) ToolCall(ev ToolCallEvent) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ToolCall(ev) })
	}
}
func (m *MultiSink) ToolResult(ev ToolResultEvent) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ToolResult(ev) })
	}
}
func (m *MultiSink) Error(msg string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Error(msg) })
	}
}
func (m *MultiSink) Warn(msg string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Warn(msg) })
	}
}
func (m *MultiSink) Info(msg string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Info(msg) })
	}
}
func (m *MultiSink) Usage(ev UsageEvent) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.Usage(ev) })
	}
}
func (m *MultiSink) TurnPrompt(input string) {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.TurnPrompt(input) })
	}
}
func (m *MultiSink) ModelRateLimited() {
	for _, s := range m.sinks {
		fanOut(s, func(s EventSink) { s.ModelRateLimited() })
	}
}

// RunIDRef is the shared mutable run-id holder. The session stamps the current
// run id here per turn and clears it in finally; the durable RunEventSink reads it
// to attribute rows. Single-flight (one app, one session) makes a plain guarded
// pointer sufficient.
type RunIDRef struct {
	current string
}

// Set stamps the active run id.
func (r *RunIDRef) Set(id string) { r.current = id }

// Clear unsets the active run id (called in the turn's finally).
func (r *RunIDRef) Clear() { r.current = "" }

// Current returns the active run id ("" outside a run).
func (r *RunIDRef) Current() string { return r.current }

// MaxRunEventPayload bounds a serialized run_events payload; overflow → a
// truncation marker. serializePayload preview headroom = -200.
const (
	MaxRunEventPayload      = 8000
	runEventPreviewHeadroom = MaxRunEventPayload - 200 // 7800
)

// runEventStore is the consumer-defined seam the durable RunEventSink persists
// through. *storage.Store satisfies it (InsertRunEvent). Declared locally so the
// agent package compiles without a hard dep on storage; all writes are
// best-effort (a DB failure must never break a live turn).
type runEventStore interface {
	InsertRunEvent(rec domain.RunEventRecord) (domain.RunEventRecord, error)
}

// RunEventSink persists the event stream to run_events for /explain replay.
// Streamed tokens accumulate in contentBuffer and flush as ONE
// assistant:content row when the round ends, capturing intermediate prose (text
// in the same round as a tool call) that never reaches AssistantEnd. seq resets
// per run (when ref.Current() changes); monotonic within a run from 0.
type RunEventSink struct {
	db            runEventStore
	ref           *RunIDRef
	seq           int
	seqRunID      string
	contentBuffer string
}

// NewRunEventSink builds the durable sink over a store and the shared run-id ref.
func NewRunEventSink(db runEventStore, ref *RunIDRef) *RunEventSink {
	return &RunEventSink{db: db, ref: ref}
}

// Phase is not persisted (it's live-only UI vocabulary); the durable log records
// concrete content rows, not phase transitions.
func (s *RunEventSink) Phase(domain.RunPhase) {}

// TurnPrompt persists the originating user prompt as the run's first durable row
// (type "turn:prompt"), so /explain can label the run by what prompted it.
func (s *RunEventSink) TurnPrompt(input string) {
	s.write("turn:prompt", map[string]any{"prompt": input})
}

func (s *RunEventSink) AssistantStart() {
	s.flushContent()
	s.write("assistant:start", nil)
}

func (s *RunEventSink) AssistantToken(token string) {
	// Buffered; flushed as one assistant:content row when the round ends.
	s.contentBuffer += token
}

func (s *RunEventSink) AssistantEnd(content, reasoning string) {
	// Drop the streamed buffer (its content arrives here) to avoid duplication.
	s.contentBuffer = ""
	if reasoning != "" {
		s.write("assistant:end", map[string]any{"content": content, "reasoning": reasoning})
	} else {
		s.write("assistant:end", map[string]any{"content": content})
	}
}

func (s *RunEventSink) AssistantCancelled(content string) {
	s.contentBuffer = ""
	s.write("assistant:cancelled", map[string]any{"content": content})
}

// Interjection persists a mid-turn user message as its own durable row, flushing any
// buffered prose first so replay shows it AFTER the round it interrupted and before
// the model's response to it.
func (s *RunEventSink) Interjection(text string) {
	s.flushContent()
	s.write("user:interjection", map[string]any{"text": text})
}

// ToolBatch/ToolState are live-footer-only; the durable log keys off the concrete
// tool:call/tool:result rows, so they are intentionally not persisted.
func (s *RunEventSink) ToolBatch([]BatchedToolCall) {}
func (s *RunEventSink) ToolState(string, ToolState) {}
func (s *RunEventSink) ToolProgress(string, string) {}

func (s *RunEventSink) ToolCall(ev ToolCallEvent) {
	s.flushContent()
	// args is the raw JSON string; emit it as parsed JSON when valid, else the raw
	// string on parse failure.
	s.write("tool:call", map[string]any{"id": ev.ID, "name": ev.Name, "args": rawArgsAsAny(ev.Args)})
}

func (s *RunEventSink) ToolResult(ev ToolResultEvent) {
	s.write("tool:result", map[string]any{
		"id":           ev.ID,
		"name":         ev.Name,
		"ok":           ev.Result.Ok,
		"summary":      ev.Result.Summary,
		"auditId":      ev.Result.AuditID,
		"failureCount": ev.FailureCount,
	})
}

func (s *RunEventSink) Error(msg string) {
	s.flushContent()
	s.write("error", map[string]any{"message": msg})
}

func (s *RunEventSink) Warn(msg string) {
	s.write("warning", map[string]any{"message": msg})
}

func (s *RunEventSink) Info(msg string) {
	s.write("info", map[string]any{"message": msg})
}

func (s *RunEventSink) Usage(ev UsageEvent) {
	s.flushContent()
	payload := map[string]any{
		"promptTokens":     ev.PromptTokens,
		"completionTokens": ev.CompletionTokens,
		"totalTokens":      ev.TotalTokens,
		"contextTokens":    ev.ContextTokens,
		"contextThreshold": ev.ContextThreshold,
		"contextWindow":    ev.ContextWindow,
		"compactionDepth":  ev.CompactionDepth,
		"tier":             ev.Tier,
		"model":            ev.Model,
	}
	if ev.CachedTokens != nil {
		payload["cachedTokens"] = *ev.CachedTokens
	}
	if ev.CacheHitRatio != nil {
		payload["cacheHitRatio"] = *ev.CacheHitRatio
	}
	if ev.CostUsd != nil {
		payload["costUsd"] = *ev.CostUsd
	}
	if len(ev.Tiers) > 0 {
		tiers := make([]map[string]any, 0, len(ev.Tiers))
		for _, t := range ev.Tiers {
			m := map[string]any{
				"tier":             t.Tier,
				"model":            t.Model,
				"promptTokens":     t.PromptTokens,
				"completionTokens": t.CompletionTokens,
				"totalTokens":      t.TotalTokens,
			}
			if t.CachedTokens != nil {
				m["cachedTokens"] = *t.CachedTokens
			}
			if t.CostUsd != nil {
				m["costUsd"] = *t.CostUsd
			}
			tiers = append(tiers, m)
		}
		payload["tiers"] = tiers
	}
	s.write("usage", payload)
}

// ModelRateLimited is a live-only health cue (badge state), not persisted to the
// durable run-event log.
func (s *RunEventSink) ModelRateLimited() {}

// flushContent emits the buffered intermediate prose as one assistant:content row
// and clears the buffer.
func (s *RunEventSink) flushContent() {
	if s.contentBuffer == "" {
		return
	}
	content := s.contentBuffer
	s.contentBuffer = ""
	s.write("assistant:content", map[string]any{"content": content})
}

// write persists one row. seq resets when the run id changes; runs without a
// current id (emitted outside a turn) are dropped. Best-effort: any DB error is
// swallowed (durable logging must never break a live turn).
func (s *RunEventSink) write(typ string, payload any) {
	runID := s.ref.Current()
	if runID == "" {
		return
	}
	if runID != s.seqRunID {
		s.seqRunID = runID
		s.seq = 0
	}
	var payloadPtr *string
	if payload != nil {
		p := serializeRunPayload(payload)
		payloadPtr = &p
	}
	rec := domain.RunEventRecord{
		RunID:   runID,
		Seq:     s.seq,
		Type:    typ,
		Payload: payloadPtr,
	}
	s.seq++
	func() {
		defer func() { _ = recover() }()
		if s.db == nil {
			return
		}
		_, _ = s.db.InsertRunEvent(rec)
	}()
}

// serializeRunPayload JSON-encodes a payload, replacing an oversized result with a
// truncation marker. On a marshal failure it emits a stable
// {error:"unserializable"} stub.
func serializeRunPayload(payload any) string {
	b, err := json.Marshal(payload)
	if err != nil {
		stub, _ := json.Marshal(map[string]string{"error": "unserializable"})
		return string(stub)
	}
	if len(b) <= MaxRunEventPayload {
		return string(b)
	}
	preview := sliceChars(string(b), runEventPreviewHeadroom)
	marker, _ := json.Marshal(map[string]any{
		"truncated": true,
		"bytes":     len(b), // byte length
		"preview":   preview,
	})
	return string(marker)
}

// rawArgsAsAny decodes a raw JSON argument string to a generic value for the
// run-event payload, falling back to the raw string when it isn't valid JSON
// (parsed JSON, or the raw string on parse failure).
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
