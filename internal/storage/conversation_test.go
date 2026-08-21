package storage

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
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

// TestConversationNameRoundTrip: the reserved `daintree_compaction` name on a
// server-delivered compacted context block survives Insert→List, and a row without one
// reads back nil rather than "".
//
// The distinction is the whole feature. That name is the ONLY thing telling the backend
// where already-frozen history ends; a block that rehydrated without it would be
// compacted a second time and the request would carry history the server had already
// replaced — a silent, permanent cost regression with no error anywhere to explain it.
func TestConversationNameRoundTrip(t *testing.T) {
	s := openTest(t, 100)
	name := "daintree_compaction"
	if _, err := s.InsertMessage(domain.ConversationMessageRecord{
		SessionID: "ses_c", Seq: 0, Role: "user", Content: "frozen state", Name: &name,
	}); err != nil {
		t.Fatalf("insert block: %v", err)
	}
	if _, err := s.InsertMessage(domain.ConversationMessageRecord{
		SessionID: "ses_c", Seq: 1, Role: "user", Content: "next",
	}); err != nil {
		t.Fatalf("insert user: %v", err)
	}

	msgs, err := s.ListMessages("ses_c")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Name == nil || *msgs[0].Name != name {
		t.Errorf("block name not preserved across Insert→List: %v", msgs[0].Name)
	}
	if msgs[1].Name != nil {
		t.Errorf("an ordinary row must read back a nil name, got %q", *msgs[1].Name)
	}
}
