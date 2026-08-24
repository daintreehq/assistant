package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
)

// The session's half of the fast preview: what the user SEES while it is still
// provisional. The backend client owns what gets committed (see
// internal/backend/preamble_test.go); these pin the rendering contract, where the
// failures are visual and would otherwise only show up in front of a user.

// The preview goes out on its OWN channel and NEVER on the token channel. That
// separation is the whole fix: AssistantToken buffers, and every durable flush
// beneath it — the runbook decision that fires on the first executor token, a usage
// row, an error — promotes whatever is buffered into a permanent row. Routing a
// provisional preview through it recorded the preview twice on a good turn and once
// on a failed one, which is precisely the "commit nothing on error" rule it is
// supposed to obey.
func TestOnPreamble_UsesItsOwnChannelNotTheTokenStream(t *testing.T) {
	_, tokens, preambles := runScriptedTurnFull(t, func(cb backend.StreamCallbacks) {
		cb.OnPreamble(backend.StreamPreamble{
			ID:          "pre_1",
			Content:     "I'll check the failing test.",
			Provisional: true,
			CommitOn:    "done",
		})
		cb.OnContent("The loader was wrong.")
	}, "I'll check the failing test.\n\nThe loader was wrong.")

	if len(preambles) != 1 || preambles[0] != "I'll check the failing test." {
		t.Fatalf("preambles = %q, want the preview once, unseparated", preambles)
	}
	// Spacing is the renderer's business: painters add the blank line so the live
	// view matches the joined message, and record sinks store neither.
	for _, tok := range tokens {
		if strings.Contains(tok, "I'll check the failing test.") {
			t.Fatalf("the preview leaked onto the token channel: tokens = %q", tokens)
		}
	}
	if len(tokens) != 1 || tokens[0] != "The loader was wrong." {
		t.Fatalf("tokens = %q, want the executor content alone", tokens)
	}
}

// Visible text is visible text: the footer must say Generating while a sentence is
// on screen. Leaving it on "Analyzing request" is the failure this stage exists to
// prevent — the whole point is that something legible arrives early.
func TestOnPreamble_FlipsThePhaseToGenerating(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnPreamble(backend.StreamPreamble{Content: "I'll start on that."})
		cb.OnContent("Done.")
	}, "I'll start on that.\n\nDone.")

	if indexOfPhase(phases, domain.PhaseGenerating) < 0 {
		t.Fatalf("phases = %v, want Generating once the preview was shown", phases)
	}
}

// Once prose is on screen the thinking cue is suppressed, exactly as it is after the
// first executor token. Flipping the footer back to "Thinking" under a sentence the
// user is already reading reads as the answer having been retracted.
func TestOnPreamble_SuppressesTheLaterThinkingCue(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnPreamble(backend.StreamPreamble{Content: "I'll look at the loader."})
		// The executor genuinely does reason after the handoff; the cue is still
		// wrong now that visible text has begun.
		cb.OnReasoning("chain of thought")
		cb.OnStatus(backend.StreamStatus{Phase: "thinking"})
		cb.OnContent("The loader was wrong.")
	}, "I'll look at the loader.\n\nThe loader was wrong.")

	if n := countPhase(phases, domain.PhaseThinking); n != 0 {
		t.Fatalf("Thinking phase fired %d times after a visible preview; phases = %v", n, phases)
	}
}

// Without a preview the thinking cue is untouched. FAST_RESPONSE_MODE is off in every
// deployment today, so this is the path essentially every real turn takes and the one
// that must not move.
func TestWithoutAPreamble_TheThinkingCueStillFires(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnReasoning("chain of thought")
		cb.OnContent("The loader was wrong.")
	}, "The loader was wrong.")

	if n := countPhase(phases, domain.PhaseThinking); n != 1 {
		t.Fatalf("Thinking phase fired %d times, want 1; phases = %v", n, phases)
	}
}

// A 426 is a version problem between two programs shipped together, and it must say so.
// Left to the generic branch it read as "Model error: backend: http 426
// unsupported_daintree_pr…" — a compatibility failure dressed as a model failure, and
// truncated in the terminal before the one word that identified it.
func TestProtocolMismatchReadsAsAVersionProblemNotAModelError(t *testing.T) {
	s := &Session{events: NoopEventSink{}}
	reply := s.classifyBackendError(&backend.Error{
		HTTPStatus: 426,
		Code:       "unsupported_daintree_protocol",
		Message:    "protocol 2 is not supported",
	})
	if strings.HasPrefix(reply, "Model error:") {
		t.Fatalf("a protocol mismatch still reads as a model failure: %q", reply)
	}
	if !strings.Contains(reply, "different version") {
		t.Fatalf("reply = %q, want it to name the version mismatch", reply)
	}
	// Registered as a wake-failure prefix, or an unattended wake would mistake this
	// failed turn for a real answer and record the work as summarized.
	if !IsWakeFailureReply(reply) {
		t.Fatalf("reply %q is not a registered wake-failure prefix", reply)
	}
}
