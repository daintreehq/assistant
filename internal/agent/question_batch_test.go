package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// question_batch_test.go locks the "a question must be asked ALONE" batch guard: when
// the model bundles user.askMultipleChoice with other tools, only the question is
// dispatched and every sibling is skipped with a recoverable QUESTION_BATCH_SKIPPED stub
// — so no side-effecting tool runs before the user has answered.

func TestQuestionMustBeAskedAlone(t *testing.T) {
	tools := &fakeTools{result: domain.Ok("recorded choice", nil)}
	calls := []models.ToolCallRequest{
		toolCall("c0", "user__askMultipleChoice", `{"question":"Which?","options":["A","B"]}`),
		toolCall("c1", "terminal__sendCommand", `{"terminalId":"t1","command":"do it"}`),
		toolCall("c2", "fs__read", `{"path":"x"}`),
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want final", reply)
	}
	// Only the question dispatched; both siblings were skipped, never dispatched.
	if tools.dispatched != 1 {
		t.Fatalf("dispatched %d calls, want 1 (only the question)", tools.dispatched)
	}
	if len(tools.dispatchSeen) != 1 || tools.dispatchSeen[0] != "user.askMultipleChoice" {
		t.Fatalf("dispatched %v, want [user.askMultipleChoice]", tools.dispatchSeen)
	}
	// The transcript stays well-formed: every tool_call gets a reply (1 real + 2 stubs).
	msgs := s.Messages()
	if got := countToolReplies(msgs); got != 3 {
		t.Fatalf("tool replies = %d, want 3", got)
	}
	skips := 0
	for _, m := range msgs {
		if m.Role == "tool" && strings.Contains(m.StringContent, "QUESTION_BATCH_SKIPPED") {
			skips++
		}
	}
	if skips != 2 {
		t.Fatalf("QUESTION_BATCH_SKIPPED stubs = %d, want 2", skips)
	}
}

func TestQuestionNotFirstStillIsolated(t *testing.T) {
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	calls := []models.ToolCallRequest{
		toolCall("c0", "fs__read", `{"path":"x"}`),
		toolCall("c1", "user__askMultipleChoice", `{"question":"Which?","options":["A","B"]}`),
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.dispatched != 1 || len(tools.dispatchSeen) != 1 || tools.dispatchSeen[0] != "user.askMultipleChoice" {
		t.Fatalf("only the question should dispatch even when it is not first; got %v", tools.dispatchSeen)
	}
}

// TestQuestionSkipDoesNotTripBreaker is the regression for the review finding: the
// skipped-sibling stubs are SYNTHETIC, not real tool failures, so 4 identical-arg
// siblings bundled ahead of the question must NOT trip the repeat-failure breaker
// (RepeatFailureAbort=3) and kill the turn before the question dispatches.
func TestQuestionSkipDoesNotTripBreaker(t *testing.T) {
	tools := &fakeTools{result: domain.Ok("recorded choice", nil)}
	calls := []models.ToolCallRequest{
		toolCall("c0", "fs__read", `{"path":"x"}`),
		toolCall("c1", "fs__read", `{"path":"x"}`),
		toolCall("c2", "fs__read", `{"path":"x"}`),
		toolCall("c3", "fs__read", `{"path":"x"}`),
		toolCall("c4", "user__askMultipleChoice", `{"question":"Which?","options":["A","B"]}`),
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))
	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "final" {
		t.Fatalf("reply = %q, want final (identical skipped siblings must not trip the breaker)", reply)
	}
	if tools.dispatched != 1 {
		t.Fatalf("only the question should dispatch; got %d", tools.dispatched)
	}
}

func TestSingleQuestionDispatchesNormally(t *testing.T) {
	tools := &fakeTools{result: domain.Ok("recorded choice", nil)}
	calls := []models.ToolCallRequest{
		toolCall("c0", "user__askMultipleChoice", `{"question":"Which?","options":["A","B"]}`),
	}
	r := &fakeRouter{results: []models.ChatResult{{ToolCalls: calls}, {Content: "final"}}}
	s := NewSession(baseDeps(r, tools))
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if tools.dispatched != 1 {
		t.Fatalf("a lone question should dispatch; dispatched %d", tools.dispatched)
	}
}
