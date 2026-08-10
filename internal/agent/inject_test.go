package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// injectRouter records the message snapshot it was handed each round and runs an
// optional hook before returning a queued result — so a test can inject a message
// MID-TURN (from the turn goroutine, exactly as the daemon/composer would) and then
// assert what the NEXT round's model call actually saw.
type injectRouter struct {
	results []models.ChatResult
	round   int
	seen    [][]models.ChatMessage
	onRound func(round int)
}

func (r *injectRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	i := r.round
	r.round++
	r.seen = append(r.seen, opts.Messages)
	if r.onRound != nil {
		r.onRound(i)
	}
	if i >= len(r.results) {
		return models.ChatResult{Content: "done"}, nil
	}
	return r.results[i], nil
}
func (r *injectRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "summary"}, nil
}
func (r *injectRouter) ModelFor(domain.ModelTier) string { return "m" }

func userTextSeen(round []models.ChatMessage, want string) bool {
	for _, m := range round {
		if m.Role == "user" && strings.Contains(m.StringContent, want) {
			return true
		}
	}
	return false
}

// A message injected DURING a round is folded into history at the TOP of the NEXT round,
// so the model's next call sees it ("between tasks"), and an Interjection event fires.
func TestInjectPrompt_FoldedInAtNextRound(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
			{Content: "final"}, // round 1: final answer
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("also check the logs")
			}
		},
	}
	sink := &orderSink{}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.Events = sink
	s = NewSession(deps)

	reply, err := s.Send(context.Background(), "start", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want final", reply)
	}
	if len(r.seen) < 2 {
		t.Fatalf("expected at least 2 rounds, got %d", len(r.seen))
	}
	if userTextSeen(r.seen[0], "also check the logs") {
		t.Error("round 0 must not yet see the message (it was typed DURING round 0)")
	}
	if !userTextSeen(r.seen[1], "also check the logs") {
		t.Error("round 1 must see the folded-in message in its snapshot")
	}
	if !contains(sink.log, "interject:also check the logs") {
		t.Errorf("Interjection event not emitted; log=%v", sink.log)
	}
}

// A message that arrives while the model produces a FINAL answer (no tool calls) must
// NOT let the turn seal — it loops to consume the message instead of stranding it.
func TestInjectPrompt_AtFinalAnswerBoundaryKeepsTurnAlive(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{Content: "premature-final"}, // round 0: model thinks it's done…
			{Content: "real-final"},      // round 1: …but answers the injected message
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("wait, one more thing")
			}
		},
	}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s = NewSession(deps)

	reply, err := s.Send(context.Background(), "start", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "real-final" {
		t.Fatalf("reply = %q, want real-final (the turn must not seal while a message is pending)", reply)
	}
	if r.round < 2 {
		t.Fatalf("turn ended after %d round(s); it should have looped to consume the injection", r.round)
	}
}

// A turn has NO per-turn round ceiling: a long-running autonomous workflow must be
// able to drive far more rounds than any old cap and still reach its final answer.
// The only runaway guard is the same-call failure breaker — and these calls all
// succeed, so nothing trips it no matter how many rounds run.
func TestLongTurn_HasNoIterationCeiling(t *testing.T) {
	const rounds = 40 // far beyond the old 12-round cap
	results := make([]models.ChatResult, rounds+1)
	for i := 0; i < rounds; i++ {
		results[i] = models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}
	}
	results[rounds] = models.ChatResult{Content: "final"}
	r := &injectRouter{results: results}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "start", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q after %d rounds, want final — a long turn must not be capped", reply, rounds)
	}
}

func TestRetractPendingInjection_LIFO(t *testing.T) {
	s := NewSession(baseDeps(&fakeRouter{}, &fakeTools{}))
	s.InjectPrompt("first")
	s.InjectPrompt("second")

	if text, ok := s.RetractPendingInjection(); !ok || text != "second" {
		t.Fatalf("first retract = (%q,%v), want (second,true)", text, ok)
	}
	if text, ok := s.RetractPendingInjection(); !ok || text != "first" {
		t.Fatalf("second retract = (%q,%v), want (first,true)", text, ok)
	}
	if _, ok := s.RetractPendingInjection(); ok {
		t.Fatal("retract on an empty buffer must report ok=false")
	}
}

func TestDiscardPendingInjections(t *testing.T) {
	s := NewSession(baseDeps(&fakeRouter{}, &fakeTools{}))
	s.InjectPrompt("a")
	s.InjectPrompt("b")
	s.DiscardPendingInjections()
	if _, ok := s.RetractPendingInjection(); ok {
		t.Fatal("discard must drop every buffered injection")
	}
}

// A buffered injection that is never retracted is still folded into history with the
// user-interjection framing (so the model knows it arrived mid-task).
func TestInjectPrompt_FoldedMessageCarriesPrefix(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
			{Content: "final"},
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("the raw user text")
			}
		},
	}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	s = NewSession(deps)
	if _, err := s.Send(context.Background(), "start", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range s.Messages() {
		if m.Role == "user" && strings.HasPrefix(m.StringContent, userInterjectPrefix) &&
			strings.Contains(m.StringContent, "the raw user text") {
			found = true
		}
	}
	if !found {
		t.Error("folded-in message should be a user message carrying the interjection prefix")
	}
}
