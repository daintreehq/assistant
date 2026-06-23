// Package cli owns CLI routing (one-shot / classic REPL / doctor / cockpit seam),
// the human console sink, and the classic line REPL. The Bubble Tea cockpit is a
// separate wave reached through the CockpitRunner seam.
package cli

import (
	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// consoleSink maps agent events to render calls. It is the human
// (non-JSON) one-shot + REPL sink.
type consoleSink struct {
	r         *render.Renderer
	tty       bool
	lastPhase domain.RunPhase
}

// NewConsoleSink builds a console sink over a renderer. It records whether stdout is a
// TTY so the "silent work" phase cues stay off piped/one-shot output.
func NewConsoleSink(r *render.Renderer) agent.EventSink {
	return &consoleSink{r: r, tty: stdoutIsTTY()}
}

// Phase surfaces the "silent work" gaps (analyze / integrate) as a dim status line so
// the console doesn't sit mute through model latency — the gap between Enter and the
// first token. TTY-only (piped/JSON-adjacent output stays clean for parsers), deduped
// on transitions, and deliberately NON-rewriting: a plain dim line that scrolls, with
// no cursor games that would collide with interleaved tool/log output.
func (s *consoleSink) Phase(p domain.RunPhase) {
	if !s.tty || p == s.lastPhase {
		return
	}
	s.lastPhase = p
	switch p {
	case domain.PhaseAnalyzing:
		s.r.Line(s.r.Gray("· analyzing request…"))
	case domain.PhaseIntegrating:
		s.r.Line(s.r.Gray("· integrating results…"))
	}
}

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
func (s *consoleSink) Warn(m string)          { s.r.Warn(m) }
func (s *consoleSink) Info(m string)          { s.r.Info(m) }
func (s *consoleSink) Usage(agent.UsageEvent) {}
func (s *consoleSink) TurnPrompt(string)      {}

// ModelRateLimited is a live-cockpit health badge; the console already prints the
// "Model rate-limited" reply, so there's nothing extra to render here.
func (s *consoleSink) ModelRateLimited() {}
