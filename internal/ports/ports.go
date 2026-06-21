// Package ports defines the small interface seams that wire the subsystems
// together: EventSink (event fan-out) plus the core service interfaces the agent
// loop depends on (Store, Router, ToolRegistry, MCPClient, Queue).
//
// These interfaces are deliberately SMALL — just the core methods — so concrete
// packages (storage, models, tools, mcp, queue) can implement them in Phase C
// without import cycles. Each interface points to the docs/port spec that
// describes its full surface. Concrete signatures will grow there; keep the
// seam minimal here.
package ports

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// EventSink receives structured runtime events. The UiBridge, console sink, and
// one-shot JSON sink each implement it. Spec: docs/port/_interaction-ux.md
// (the live-footer event model) and the AgentEventSink in src/agent/events.ts.
type EventSink interface {
	// Emit publishes a single event. Implementations MUST NOT block the caller
	// for long and MUST NOT panic into it.
	Emit(ev domain.AgentEvent)
}

// NoopSink discards every event. Useful as a default and in tests.
type NoopSink struct{}

// Emit implements EventSink by doing nothing.
func (NoopSink) Emit(domain.AgentEvent) {}

// MultiSink fans an event out to several sinks. A panic in one sink is recovered
// so it never breaks the emit path or starves the other sinks (a side-channel
// like a UI bridge must never crash the agent loop).
type MultiSink struct {
	sinks []EventSink
}

// NewMultiSink builds a MultiSink over the given sinks (nil entries are skipped).
func NewMultiSink(sinks ...EventSink) *MultiSink {
	filtered := make([]EventSink, 0, len(sinks))
	for _, s := range sinks {
		if s != nil {
			filtered = append(filtered, s)
		}
	}
	return &MultiSink{sinks: filtered}
}

// Emit forwards the event to every sink, recovering from any panic.
func (m *MultiSink) Emit(ev domain.AgentEvent) {
	for _, s := range m.sinks {
		emitSafe(s, ev)
	}
}

// emitSafe calls s.Emit with panic recovery so one misbehaving sink cannot break
// fan-out.
func emitSafe(s EventSink, ev domain.AgentEvent) {
	defer func() { _ = recover() }()
	s.Emit(ev)
}

// Store is the persistence seam the agent loop and daemon depend on. The full
// surface (timers, watchers, events, audit, conversation, grants, memories,
// workflow runs, skill state, agent launches) lives in docs/port/domain-config.md
// §2.4 and _contracts.md §7. Phase C's internal/storage implements this; the
// methods here are the minimum the loop needs to compile against.
type Store interface {
	// AppendRunEvent persists one durable replay event for a turn.
	AppendRunEvent(ctx context.Context, ev domain.RunEventRecord) error
	// AppendAudit persists one audit-log row and returns its id.
	AppendAudit(ctx context.Context, rec domain.AuditRecord) (string, error)
	// Close releases the underlying database.
	Close() error
}

// ChatMessage is a single message in a model request. Kept minimal; the models
// package owns the full request/response shape. Spec: docs/port/FIREWORKS.md (TS
// src/models/fireworks.ts).
type ChatMessage struct {
	Role       string
	Content    string
	ToolCallID string
}

// ModelChunk is one streamed delta from the router.
type ModelChunk struct {
	Content   string
	ToolCalls []domain.ToolCallInfo
	Done      bool
	Usage     *domain.Usage
}

// Router selects a model tier and streams a completion. The full router surface
// (retry, json mode, tool calling) lives in the models package; this is the seam
// the loop calls. Spec: TS src/models/router.ts.
type Router interface {
	// Stream runs a streaming completion for the given tier, delivering chunks
	// on the returned channel until it closes. ctx cancellation aborts the turn.
	Stream(ctx context.Context, tier domain.ModelTier, messages []ChatMessage) (<-chan ModelChunk, error)
}

// ToolRegistry validates, gates, runs and audits a tool call. Full surface in
// _contracts.md §1 and src/tools/registry.ts; assertNoFileEditTools and the tier
// gate live behind Dispatch. The seam here is what the loop invokes per call.
type ToolRegistry interface {
	// Dispatch runs a single tool call by name as the given actor and returns
	// the ToolResult envelope. Handlers never throw — Dispatch always returns a
	// ToolResult (a failure becomes domain.Fail), never an error for tool-level
	// problems; the error return is reserved for infrastructure faults.
	Dispatch(ctx context.Context, actor domain.ToolActor, name string, argsJson string) (domain.ToolResult, error)
	// Has reports whether a tool with the given name is registered.
	Has(name string) bool
}

// MCPClient talks to the Daintree MCP server (Streamable HTTP, SSE fallback).
// Full surface in docs/port/DAINTREE_MCP.md and src/mcp/client.ts.
type MCPClient interface {
	// CallTool invokes a raw MCP tool by name with JSON arguments.
	CallTool(ctx context.Context, name string, argsJson string) (domain.ToolResult, error)
	// Connected reports whether the client currently has a live session.
	Connected() bool
}

// Queue is the attention queue: sub-threads publish here instead of interrupting
// the main thread. Full surface (dedupe, severity ordering, format) in
// docs/port/domain-config.md §6.
type Queue interface {
	// Publish adds or dedupes an event and returns the resulting QueueEvent.
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
	// Digest returns the ordered, filtered open events.
	Digest(ctx context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error)
	// Resolve stamps an event resolved, reporting whether a row changed.
	Resolve(ctx context.Context, id string) (bool, error)
}
