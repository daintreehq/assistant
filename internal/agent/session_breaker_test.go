package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// countToolReplies counts role:"tool" messages in the transcript — every assistant
// tool_call must have exactly one matching reply (dispatched or stubbed) or DeepSeek
// 400s on replay.
func countToolReplies(msgs []models.ChatMessage) int {
	n := 0
	for _, m := range msgs {
		if m.Role == "tool" {
			n++
		}
	}
	return n
}

// TestMidBatchBreakerStopsHugeBatch is the regression for the 87-call dump: when the
// model crams many identical failing calls into ONE batch, the breaker must abort
// MID-batch (after RepeatFailureAbort) and stub the rest, NOT dispatch all of them
// before stopping.
func TestMidBatchBreakerStopsHugeBatch(t *testing.T) {
	tools := &fakeTools{result: domain.Fail("ARTIFACT_NOT_FOUND", "gone", domain.Unrecoverable())}
	const n = 20
	calls := make([]models.ToolCallRequest, n)
	for i := range calls {
		calls[i] = toolCall("c"+itoa(i), "artifact__read", `{"artifactId":"art-x","offset":0}`) // identical args
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Stopped: called ") {
		t.Fatalf("expected mid-batch breaker abort, got %q", reply)
	}
	// Aborted after the 3rd identical failure — the other 17 were NEVER dispatched.
	if tools.dispatched != domain.RepeatFailureAbort {
		t.Fatalf("dispatched %d calls, want %d (abort must stop the batch, not run all %d)", tools.dispatched, domain.RepeatFailureAbort, n)
	}
	// Transcript stays well-formed: all 20 tool_calls get a reply (3 real + 17 skip stubs).
	if got := countToolReplies(s.Messages()); got != n {
		t.Fatalf("tool replies = %d, want %d (every tool_call needs a matching reply)", got, n)
	}
}

// TestCoarseBreakerArgVariedUnrecoverableLoop: same tool, same UNRECOVERABLE error, but
// a new offset each call (paging a pruned artifact). Each call has a distinct FINE
// signature so the exact-args breaker never trips; the coarse (pagination-insensitive)
// breaker must catch it at CoarseRepeatFailureAbort — mid-batch.
func TestCoarseBreakerArgVariedUnrecoverableLoop(t *testing.T) {
	tools := &fakeTools{result: domain.Fail("ARTIFACT_NOT_FOUND", "pruned", domain.Unrecoverable())}
	const n = domain.CoarseRepeatFailureAbort
	calls := make([]models.ToolCallRequest, n)
	for i := range calls {
		calls[i] = toolCall("c"+itoa(i), "artifact__read", `{"artifactId":"art-x","offset":`+itoa(i*3500)+`}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Stopped: called ") || !strings.Contains(reply, "unrecoverable") {
		t.Fatalf("expected coarse unrecoverable-loop abort, got %q", reply)
	}
	if !IsWakeFailureReply(reply) {
		t.Fatal("coarse breaker reply must be a wake-failure sentinel")
	}
}

// TestCoarseBreakerIgnoresRecoverableErrors: a RECOVERABLE error repeated with varied
// args must NOT trip the coarse breaker — the model may legitimately retry transient
// failures, so the turn proceeds.
func TestCoarseBreakerIgnoresRecoverableErrors(t *testing.T) {
	tools := &fakeTools{result: domain.Fail("MCP_RATE_LIMITED", "slow down")} // Recoverable defaults true
	calls := make([]models.ToolCallRequest, domain.CoarseRepeatFailureAbort+2)
	for i := range calls {
		calls[i] = toolCall("c"+itoa(i), "terminal__read", `{"terminalId":"t`+itoa(i)+`"}`)
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("recoverable errors must not trip the coarse breaker; got %q", reply)
	}
}
