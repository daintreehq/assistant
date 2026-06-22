// Package cli owns CLI routing (one-shot / classic REPL / doctor / cockpit seam),
// the human console sink, and the classic line REPL. Port of src/cli/{index,
// consoleSink,repl,render}.ts. The Bubble Tea cockpit is a separate wave reached
// through the CockpitRunner seam.
package cli

import (
	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// consoleSink maps agent events to render calls (consoleSink.ts). It is the human
// (non-JSON) one-shot + REPL sink.
type consoleSink struct{ r *render.Renderer }

// NewConsoleSink builds a console sink over a renderer.
func NewConsoleSink(r *render.Renderer) agent.EventSink { return &consoleSink{r: r} }

// Phase is not rendered in the console (it drives the cockpit footer only).
func (s *consoleSink) Phase(domain.RunPhase) {}

func (s *consoleSink) AssistantStart()             { s.r.AssistantStart() }
func (s *consoleSink) AssistantToken(t string)     { s.r.StreamToken(t) }
func (s *consoleSink) AssistantEnd(_, _ string)    { s.r.AssistantEnd() }
func (s *consoleSink) AssistantCancelled(_ string) { s.r.Info("Turn cancelled"); s.r.AssistantEnd() }

// ToolBatch / ToolState / ToolProgress are live-footer-only; the console prints
// concrete tool calls + results, not the per-call substep stream.
func (s *consoleSink) ToolBatch([]agent.BatchedToolCall) {}
func (s *consoleSink) ToolState(string, agent.ToolState) {}
func (s *consoleSink) ToolProgress(string, string)       {}

func (s *consoleSink) ToolCall(ev agent.ToolCallEvent) {
	s.r.Line("")
	s.r.ToolCall(ev.Name, render.Truncate(ev.Args, 120))
}

func (s *consoleSink) ToolResult(ev agent.ToolResultEvent) {
	s.r.ToolResult(ev.Result.Ok, ev.Result.Summary)
}

func (s *consoleSink) Error(m string)         { s.r.Line(""); s.r.Error(m) }
func (s *consoleSink) Info(m string)          { s.r.Info(m) }
func (s *consoleSink) Usage(agent.UsageEvent) {}
