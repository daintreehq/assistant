package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// cancelledStreamRouter aborts mid-stream by returning a CancelledError.
type cancelledStreamRouter struct{}

func (cancelledStreamRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{}, context.Canceled
}
func (cancelledStreamRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (cancelledStreamRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

func TestCancelMidStreamReturnsSentinelNotError(t *testing.T) {
	sink := &orderSink{}
	deps := baseDeps(cancelledStreamRouter{}, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	reply, err := s.Send(context.Background(), "hello", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q want %q", reply, domain.CancelledReply)
	}
	if !contains(sink.log, "cancelled:") {
		t.Fatalf("expected an assistantCancelled event, got %v", sink.log)
	}
	for _, e := range sink.log {
		if strings.HasPrefix(e, "error:") {
			t.Fatalf("a clean cancel must not surface as an error: %v", sink.log)
		}
	}
}

// signalRouter triggers a tool round then finishes, recording nothing; the tool
// itself records the ctx signal it was dispatched with.
type signalRouter struct{ round int }

func (r *signalRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	r.round++
	if r.round == 1 {
		return models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("c1", "probe", "{}")}}, nil
	}
	return models.ChatResult{Content: "done"}, nil
}
func (r *signalRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *signalRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

// signalTool records the dispatch context so we can prove the turn ctx threads in.
type signalTool struct {
	seen []context.Context
}

func (t *signalTool) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (t *signalTool) ResolveWireName(w string) string                 { return w }
func (t *signalTool) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.seen = append(t.seen, ctx)
	return domain.Ok("ok", nil)
}

func TestCancelForwardsSignalIntoDispatchContext(t *testing.T) {
	// The dispatch ctx must be the SAME cancellable ctx passed to Send — proof the
	// turn signal threads past router.Stream all the way into Dispatch (#81).
	tool := &signalTool{}
	deps := baseDeps(&signalRouter{}, tool)
	s := NewSession(deps)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := s.Send(ctx, "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(tool.seen) != 1 {
		t.Fatalf("dispatched %d times, want 1", len(tool.seen))
	}
	// Cancelling the parent must propagate to the dispatch ctx the tool captured.
	cancel()
	if tool.seen[0].Err() == nil {
		t.Fatal("dispatch ctx must derive from the turn ctx (cancellation propagates)")
	}
}

// abortingTool aborts the turn ctx from inside the first dispatch, simulating an
// Escape mid-batch. The remaining calls must be stubbed CANCELLED, never run.
type abortingTool struct {
	cancel     context.CancelFunc
	dispatched int
}

func (t *abortingTool) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (t *abortingTool) ResolveWireName(w string) string                 { return w }
func (t *abortingTool) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.dispatched++
	t.cancel() // user hit Escape while this tool ran
	return domain.Ok("did work before the cancel landed", nil)
}

// twoCallRouter asks for two tool calls in one round.
type twoCallRouter struct{ rounds int }

func (r *twoCallRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	r.rounds++
	return models.ChatResult{ToolCalls: []models.ToolCallRequest{
		toolCall("call_a", "abortertool", "{}"),
		toolCall("call_b", "abortertool", "{}"),
	}}, nil
}
func (r *twoCallRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *twoCallRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

func TestCancelMidBatchStubsRemainingCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tool := &abortingTool{cancel: cancel}
	router := &twoCallRouter{}
	sink := &orderSink{}
	deps := baseDeps(router, tool)
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(ctx, "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q want %q", reply, domain.CancelledReply)
	}
	// Only the first tool ran; the second was stubbed, never dispatched.
	if tool.dispatched != 1 {
		t.Fatalf("dispatched %d times, want 1", tool.dispatched)
	}
	// The model was not asked again after the cancel.
	if router.rounds != 1 {
		t.Fatalf("stream rounds = %d, want 1", router.rounds)
	}
	// History integrity: BOTH tool_call ids must have a matching tool reply so the
	// transcript replays cleanly (no dangling tool_calls / DeepSeek 400).
	var toolReplies []string
	var stubContent string
	for _, m := range s.Messages() {
		if m.Role == "tool" {
			toolReplies = append(toolReplies, m.ToolCallID)
			if m.ToolCallID == "call_b" {
				stubContent = m.StringContent
			}
		}
	}
	if len(toolReplies) != 2 {
		t.Fatalf("tool replies = %v want both call_a and call_b answered", toolReplies)
	}
	if !strings.Contains(stubContent, "CANCELLED") {
		t.Fatalf("stub for the un-run call must be marked CANCELLED: %q", stubContent)
	}
	if !contains(sink.log, "cancelled:") {
		t.Fatal("expected a cancelled event")
	}
}

// cancelThenTwoCallsRouter cancels the turn ctx in onToken, then returns a 2-call
// batch — so by the time runToolBatch enters its loop the ctx is ALREADY cancelled
// and NO call has run. This exercises the PRE-dispatch cancel check (#2): the check
// at the top of the batch must stub calls[0:] (the current call AND every remaining
// one) and dispatch none.
type cancelThenTwoCallsRouter struct {
	cancel context.CancelFunc
	rounds int
}

func (r *cancelThenTwoCallsRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	r.rounds++
	if onToken != nil {
		onToken("planning") // a token streamed fine...
	}
	r.cancel() // ...then the user hit Escape, before the batch dispatches
	return models.ChatResult{ToolCalls: []models.ToolCallRequest{
		toolCall("call_a", "probe", "{}"),
		toolCall("call_b", "probe", "{}"),
	}}, nil
}
func (r *cancelThenTwoCallsRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *cancelThenTwoCallsRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

func TestCancelBeforeFirstDispatchStubsAllCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	router := &cancelThenTwoCallsRouter{cancel: cancel}
	tool := &fakeTools{result: domain.Ok("should not run", nil)}
	sink := &orderSink{}
	deps := baseDeps(router, tool)
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(ctx, "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q want %q", reply, domain.CancelledReply)
	}
	// The cancel landed before the batch ran: NOT A SINGLE tool dispatched.
	if tool.dispatched != 0 {
		t.Fatalf("dispatched %d times, want 0 (cancel before the first dispatch)", tool.dispatched)
	}
	// Both queued calls still get a structurally-valid CANCELLED tool result so the
	// transcript replays cleanly (no dangling tool_calls).
	answered := map[string]string{}
	for _, m := range s.Messages() {
		if m.Role == "tool" {
			answered[m.ToolCallID] = m.StringContent
		}
	}
	for _, id := range []string{"call_a", "call_b"} {
		c, ok := answered[id]
		if !ok {
			t.Fatalf("%s has no tool result — transcript would be invalid", id)
		}
		if !strings.Contains(c, "CANCELLED") {
			t.Fatalf("%s result not marked CANCELLED: %q", id, c)
		}
	}
}

// preTurnAbortRouter aborts during the pre-turn auto-compact summarizer (Chat), so
// the abort lands after Send begins but before the user message is committed.
type preTurnAbortRouter struct {
	cancel context.CancelFunc
	rounds int
}

func (r *preTurnAbortRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	r.rounds++
	return models.ChatResult{Content: "x"}, nil
}
func (r *preTurnAbortRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.cancel() // Escape lands during auto-compact
	return models.ChatResult{Content: "summary"}, nil
}
func (r *preTurnAbortRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

func TestCancelDuringPreTurnAwaitsPushesNoUserMessage(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	router := &preTurnAbortRouter{cancel: cancel}
	sink := &orderSink{}
	deps := baseDeps(router, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	// Grow history past the auto-compact token threshold (and beyond CONTROL+1) so
	// the pre-turn summarizer actually runs and gives the abort a window to land. Two
	// notes (a lone one trips the "no real history" guard), together over the soft gate.
	s.InjectNote(strings.Repeat("A", (domain.AutoCompactTokenThreshold/2+10_000)*domain.CharsPerToken))
	s.InjectNote(strings.Repeat("B", (domain.AutoCompactTokenThreshold/2+10_000)*domain.CharsPerToken))

	reply, err := s.Send(ctx, "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q want %q", reply, domain.CancelledReply)
	}
	if router.rounds != 0 {
		t.Fatalf("stream reached the model %d times, want 0", router.rounds)
	}
	if !contains(sink.log, "cancelled:") {
		t.Fatal("expected a cancelled event")
	}
	// A pulled-back message must leave no trace in model history.
	for _, m := range s.Messages() {
		if m.StringContent == "hi" {
			t.Fatal("cancelled pre-turn must not push the user message")
		}
	}
}
