// Package cli owns CLI routing (one-shot / classic REPL / doctor / cockpit seam),
// the human console sink, and the classic line REPL. The Bubble Tea cockpit is a
// separate wave reached through the CockpitRunner seam.
package cli

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// consoleSink maps agent events to render calls. It is the human
// (non-JSON) one-shot + REPL sink.
type consoleSink struct {
	r           *render.Renderer
	diagnostics *render.Renderer
	tty         bool
	lastPhase   domain.RunPhase
	failed      bool // an Error event fired this run (drives the one-shot exit code)
	cancelled   bool // an AssistantCancelled event fired (drives exit code 2)
	answerOpen  bool // streamed prose has started and still needs its closing newline
}

// NewConsoleSink builds a console sink over a renderer. It records whether stdout is a
// TTY so the "silent work" phase cues stay off piped/one-shot output. Returns the
// concrete type so the one-shot path can read Failed() for its exit code.
func NewConsoleSink(r *render.Renderer) *consoleSink {
	return &consoleSink{r: r, diagnostics: r, tty: stdoutIsTTY()}
}

// newOneShotConsoleSink keeps successful assistant/tool output on stdout while
// routing diagnostics to stderr. Interactive REPLs use NewConsoleSink, where both
// streams intentionally share the terminal renderer.
func newOneShotConsoleSink(out, diagnostics *render.Renderer) *consoleSink {
	return &consoleSink{r: out, diagnostics: diagnostics, tty: stdoutIsTTY()}
}

// Failed reports whether a turn-level error was surfaced (Session.Send returns a
// sentinel reply, not an error, so the one-shot uses this to exit non-zero).
func (s *consoleSink) Failed() bool { return s.failed }

// Cancelled reports whether the turn ended through cancellation rather than
// success. Session.Send returns cancellation as a sentinel reply, so the event is
// the reliable source for a scriptable one-shot exit code.
func (s *consoleSink) Cancelled() bool { return s.cancelled }

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

// AssistantStart is intentionally lazy: printing its blank line before the first
// token leaves a failed one-shot with non-empty stdout. The first token opens the
// human answer instead; tool-only turns already add their own spacing.
func (s *consoleSink) AssistantStart() {}
func (s *consoleSink) AssistantToken(t string) {
	if !s.answerOpen {
		s.r.AssistantStart()
		s.answerOpen = true
	}
	s.r.StreamToken(t)
}
func (s *consoleSink) AssistantEnd(_, _ string) {
	s.closeAnswer()
}
func (s *consoleSink) AssistantCancelled(_ string) {
	s.cancelled = true
	s.closeAnswer()
	s.diagnostics.Info("Turn cancelled")
}

func (s *consoleSink) closeAnswer() {
	if !s.answerOpen {
		return
	}
	s.r.AssistantEnd()
	s.answerOpen = false
}

// Interjection prints a mid-turn user message as a distinct line so the console
// transcript shows the steer the model received between tasks.
func (s *consoleSink) Interjection(text string) {
	s.closeAnswer()
	s.r.Info("you (mid-turn): " + text)
}

// SkillLoaded prints a one-line capability cue so the console transcript shows the
// runbook the backend loaded for this turn.
func (s *consoleSink) SkillLoaded(titles []string) {
	s.closeAnswer()
	s.r.Info("Skill loaded: " + strings.Join(titles, ", "))
}

// ToolBatch / ToolState / ToolProgress are live-footer-only; the console prints
// concrete tool calls + results, not the per-call substep stream.
func (s *consoleSink) ToolBatch([]agent.BatchedToolCall) {}
func (s *consoleSink) ToolState(string, agent.ToolState) {}
func (s *consoleSink) ToolProgress(string, string)       {}

func (s *consoleSink) ToolCall(ev agent.ToolCallEvent) {
	s.closeAnswer()
	s.r.Line("")
	s.r.ToolCall(ev.Name, render.Truncate(ev.Args, 120))
}

func (s *consoleSink) ToolResult(ev agent.ToolResultEvent) {
	s.r.ToolResult(ev.Result.Ok, ev.Result.Summary)
}

func (s *consoleSink) Error(m string) {
	s.failed = true
	s.closeAnswer()
	s.diagnostics.Line("")
	s.diagnostics.Error(m)
}
func (s *consoleSink) Warn(m string)          { s.diagnostics.Warn(m) }
func (s *consoleSink) Info(m string)          { s.r.Info(m) }
func (s *consoleSink) Usage(agent.UsageEvent) {}
func (s *consoleSink) TurnPrompt(string)      {}

// ModelRateLimited is a live-cockpit health badge; the console already prints the
// "Model rate-limited" reply, so there's nothing extra to render here.
func (s *consoleSink) ModelRateLimited() {}
