package agent

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
)

// These tests pin the streaming progress-event → UI phase mapping: the FIRST
// reasoning delta (or the backend's status "thinking" event) flips Analyzing →
// Thinking as a pure LIVENESS signal — the chain-of-thought text itself is NEVER
// surfaced — and the first visible content token still flips to Generating. Unknown
// status phases are ignored (the current phase is kept).

// scriptedStreamBackend drives RespondStream's callbacks from a test script and
// returns a plain final answer, so a test can replay any SSE event ordering.
type scriptedStreamBackend struct {
	script  func(cb backend.StreamCallbacks)
	content string
}

func (b *scriptedStreamBackend) RespondStream(_ context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	meta := backend.StreamMeta{Model: "test-model", State: "dst1.test"}
	if cb.OnRawMeta != nil {
		cb.OnRawMeta(meta)
	}
	if cb.OnMeta != nil {
		cb.OnMeta(meta)
	}
	if b.script != nil {
		b.script(cb)
	}
	return backend.RespondResult{
		Meta:         meta,
		Message:      backend.RespondMessage{Content: b.content},
		FinishReason: "stop",
	}, nil
}

func (b *scriptedStreamBackend) RunTask(_ context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{Task: req.Task}, nil
}

// phaseSink records every phase transition and every visible token the session emits.
type phaseSink struct {
	NoopEventSink
	mu     sync.Mutex
	phases []domain.RunPhase
	tokens []string
}

func (s *phaseSink) Phase(p domain.RunPhase) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phases = append(s.phases, p)
}

func (s *phaseSink) AssistantToken(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, t)
}

func (s *phaseSink) snapshot() ([]domain.RunPhase, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.RunPhase(nil), s.phases...), append([]string(nil), s.tokens...)
}

func runScriptedTurn(t *testing.T, script func(cb backend.StreamCallbacks), content string) ([]domain.RunPhase, []string) {
	t.Helper()
	sink := &phaseSink{}
	deps := SessionDeps{
		Backend:   &scriptedStreamBackend{script: script, content: content},
		Tools:     &fakeTools{},
		SessionID: "sess_phase",
		Events:    sink,
	}
	s := NewSession(deps)
	reply, err := s.Send(context.Background(), "hello", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != content {
		t.Fatalf("reply = %q, want %q", reply, content)
	}
	s.DrainBackgroundWork()
	return sink.snapshot()
}

func countPhase(phases []domain.RunPhase, want domain.RunPhase) int {
	n := 0
	for _, p := range phases {
		if p == want {
			n++
		}
	}
	return n
}

func indexOfPhase(phases []domain.RunPhase, want domain.RunPhase) int {
	for i, p := range phases {
		if p == want {
			return i
		}
	}
	return -1
}

// The FIRST reasoning delta flips the phase to Thinking — once, before Generating —
// and the reasoning text reaches NO user-facing channel (no token, no phase payload).
func TestOnReasoning_FlipsToThinkingWithoutExposingText(t *testing.T) {
	const secret = "SECRET-CHAIN-OF-THOUGHT"
	phases, tokens := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnReasoning(secret)
		cb.OnReasoning(secret + " more")
		cb.OnContent("Hello")
	}, "Hello")

	if n := countPhase(phases, domain.PhaseThinking); n != 1 {
		t.Fatalf("PhaseThinking emitted %d times, want exactly 1 (first delta only); phases = %v", n, phases)
	}
	ti := indexOfPhase(phases, domain.PhaseThinking)
	gi := indexOfPhase(phases, domain.PhaseGenerating)
	if gi < 0 || ti > gi {
		t.Fatalf("Thinking must precede Generating; phases = %v", phases)
	}
	if got := strings.Join(tokens, ""); got != "Hello" {
		t.Fatalf("visible tokens = %q — reasoning text must never be surfaced", got)
	}
}

// The backend's status event (phase "thinking") maps to the same flip.
func TestOnStatus_ThinkingMapsToThinkingPhase(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnStatus(backend.StreamStatus{Phase: "thinking"})
		cb.OnContent("Hi")
	}, "Hi")
	if n := countPhase(phases, domain.PhaseThinking); n != 1 {
		t.Fatalf("status \"thinking\" should flip the phase exactly once; phases = %v", phases)
	}
}

// Status + reasoning together still flip exactly once (the latch is shared).
func TestStatusAndReasoning_FlipOnlyOnce(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnStatus(backend.StreamStatus{Phase: "thinking"})
		cb.OnReasoning("...")
		cb.OnContent("Hi")
	}, "Hi")
	if n := countPhase(phases, domain.PhaseThinking); n != 1 {
		t.Fatalf("PhaseThinking emitted %d times, want exactly 1; phases = %v", n, phases)
	}
}

// An UNKNOWN status phase is ignored — conservative mapping keeps the current phase.
func TestOnStatus_UnknownPhaseKeepsCurrentPhase(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnStatus(backend.StreamStatus{Phase: "warming_up_flux_capacitor"})
		cb.OnContent("Hi")
	}, "Hi")
	if n := countPhase(phases, domain.PhaseThinking); n != 0 {
		t.Fatalf("an unknown status must not flip the phase; phases = %v", phases)
	}
}

// A trailing reasoning fragment AFTER content must not regress Generating → Thinking.
func TestOnReasoning_AfterContentNeverRegressesPhase(t *testing.T) {
	phases, tokens := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnContent("Hi")
		cb.OnReasoning("late fragment")
	}, "Hi")
	if n := countPhase(phases, domain.PhaseThinking); n != 0 {
		t.Fatalf("reasoning after content must not regress the phase; phases = %v", phases)
	}
	if gi := indexOfPhase(phases, domain.PhaseGenerating); gi < 0 {
		t.Fatalf("content must still flip to Generating; phases = %v", phases)
	}
	if got := strings.Join(tokens, ""); got != "Hi" {
		t.Fatalf("visible tokens = %q", got)
	}
}

// No reasoning and no status ⇒ no Thinking phase at all (thinking-off streams are
// byte-identical to before).
func TestNoReasoning_NoThinkingPhase(t *testing.T) {
	phases, _ := runScriptedTurn(t, func(cb backend.StreamCallbacks) {
		cb.OnContent("Hi")
	}, "Hi")
	if n := countPhase(phases, domain.PhaseThinking); n != 0 {
		t.Fatalf("a thinking-off stream must emit no PhaseThinking; phases = %v", phases)
	}
}
