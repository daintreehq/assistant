// Package cli owns CLI routing (one-shot / line REPL / host / mcp / daemon / doctor),
// the human console sink, and the line REPL itself. There is no terminal UI beyond
// this: Daintree renders the assistant natively over `host --stdio`.
package cli

import (
	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/domain"
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
	// Whether any preview fragment has been written, and whether the blank line
	// dividing it from the answer has been. The preview arrives as a RUN of
	// fragments, so the separator cannot be appended per fragment — it would
	// land between every pair of words.
	preambleWritten  bool
	separatorWritten bool
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

// AssistantPreamble paints the fast preview into the answer the user is already
// watching. This is a SCREEN, so it shows provisional text: a preview nobody sees
// buys nothing, and a failed turn leaving words on a terminal is what streaming has
// always meant.
//
// The text arrives as a RUN of fragments typed out over about a second, so the
// blank line that divides it from the answer cannot be appended here — it would
// land between every pair of words. It is written once instead, at the first
// executor token (see AssistantToken), which is the only point that knows the
// preview has ended. A tool-call round with no prose correctly gets none.
func (s *consoleSink) AssistantPreamble(t string) {
	s.write(t)
	s.preambleWritten = true
}

func (s *consoleSink) AssistantToken(t string) {
	// The one point that can tell the preview from the answer: the preview goes
	// through AssistantPreamble, so the first token arriving HERE after one ends
	// it and earns the blank line. Written once, and never on a tool-call round
	// that produces no prose at all.
	if s.preambleWritten && !s.separatorWritten {
		s.separatorWritten = true
		s.write("\n\n")
	}
	s.write(t)
}

func (s *consoleSink) write(t string) {
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
	// Per TURN. Left set, the next turn's preview and body run together
	// (separator already spent) or a turn with no preview at all opens with a
	// stray blank line.
	s.preambleWritten = false
	s.separatorWritten = false
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

// RunbookLoaded is DELIBERATELY silent, matching the attached session: which runbooks the backend
// selected is prompt-assembly machinery, not a step in the operator's narrative. See
// Session.emitRunbookLoads.
//
// Note it does NOT call closeAnswer. The old visible cue had to, to terminate the open
// answer paragraph before printing; doing it for a silent event would split one streamed
// answer into two paragraphs for no visible reason.
func (s *consoleSink) RunbookLoaded([]string) {}

// RunbookDecision is silent for the same reason, and more so: it fires on EVERY round,
// including ones that changed nothing. It exists for the --json stream and the durable
// run log, not for a person reading an answer scroll past.
func (s *consoleSink) RunbookDecision(agent.RunbookDecisionEvent) {}

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

// ModelRateLimited is a live-attached session health badge; the console already prints the
// "Model rate-limited" reply, so there's nothing extra to render here.
func (s *consoleSink) ModelRateLimited() {}
