package storage

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestConversationReasoningRoundTrip: an assistant turn's reasoningContent (DeepSeek
// chain-of-thought) survives Insert→List so a resumed session replays it verbatim;
// a row without it reads back nil, never "".
func TestConversationReasoningRoundTrip(t *testing.T) {
	s := openTest(t, 100)
	reasoning := "let me think about this"
	if _, err := s.InsertMessage(domain.ConversationMessageRecord{
		SessionID: "ses_x", Seq: 0, Role: "assistant", Content: "answer",
		ReasoningContent: &reasoning,
	}); err != nil {
		t.Fatalf("insert assistant: %v", err)
	}
	if _, err := s.InsertMessage(domain.ConversationMessageRecord{
		SessionID: "ses_x", Seq: 1, Role: "user", Content: "next",
	}); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	msgs, err := s.ListMessages("ses_x")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].ReasoningContent == nil || *msgs[0].ReasoningContent != reasoning {
		t.Errorf("assistant reasoning not preserved across Insert→List: %v", msgs[0].ReasoningContent)
	}
	if msgs[1].ReasoningContent != nil {
		t.Errorf("a row with no reasoning must read back nil, got %q", *msgs[1].ReasoningContent)
	}
}
