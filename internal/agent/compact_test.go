package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// recordingStore captures persisted conversation rows so seq + marker writes can be
// asserted (the agent MessageStore seam — storage.Store satisfies it in prod).
type recordingStore struct {
	msgs []domain.ConversationMessageRecord
}

func (s *recordingStore) InsertMessage(rec domain.ConversationMessageRecord) (domain.ConversationMessageRecord, error) {
	s.msgs = append(s.msgs, rec)
	return rec, nil
}
func (s *recordingStore) InsertSkillSelection(rec domain.SkillSelectionLogRecord) (domain.SkillSelectionLogRecord, error) {
	return rec, nil
}

// compactSession builds a session with the real registry + a recording store.
func compactSession(t *testing.T, r Router) (*Session, *recordingStore) {
	t.Helper()
	store := &recordingStore{}
	reg, err := skills.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	deps := SessionDeps{
		Router:        r,
		Tools:         &fakeTools{},
		SkillSelector: fakeSelector{},
		SkillCatalog:  reg,
		Store:         store,
		SessionID:     "ses_compact",
		Events:        NoopEventSink{},
	}
	return NewSession(deps), store
}

func TestCompactKeepsControlsPlusSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("first")
	s.InjectNote("second")
	s.InjectNote("third")
	if len(s.Messages()) <= 4 {
		t.Fatal("expected accumulated history")
	}
	s.Compact("goals: X. open: none. next: Y.")
	msgs := s.Messages()
	if len(msgs) != 4 {
		t.Fatalf("messages = %d want 4 (3 controls + 1 summary)", len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("msg[0] role = %q", msgs[0].Role)
	}
	if !strings.Contains(msgs[1].StringContent, "# Runtime context") {
		t.Fatal("msg[1] should be runtime context")
	}
	if !strings.Contains(msgs[2].StringContent, "# Loaded skills") {
		t.Fatal("msg[2] should be loaded skills")
	}
	if msgs[3].Role != "user" || !strings.Contains(msgs[3].StringContent, "compacted summary") ||
		!strings.Contains(msgs[3].StringContent, "goals: X") {
		t.Fatalf("msg[3] = %+v want a compacted summary note", msgs[3])
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "first") {
			t.Fatal("old turns must be gone from context")
		}
	}
}

func TestClearKeepsControlsNoSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("alpha")
	s.InjectNote("beta")
	s.Clear()
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d want 3 (controls only, no summary)", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "alpha") || strings.Contains(m.StringContent, "compacted summary") {
			t.Fatal("clear must drop turns and any summary note")
		}
	}
}

func TestClearAppendsMarkerWithoutResettingSeq(t *testing.T) {
	s, store := compactSession(t, plainRouter())
	s.InjectNote("history-row")
	maxBefore := 0
	for _, r := range store.msgs {
		if r.Seq > maxBefore {
			maxBefore = r.Seq
		}
	}
	s.Clear()
	// The clear marker is appended ABOVE all prior rows (seq keeps climbing — a
	// reset would collide on the UNIQUE (sessionId, seq) index).
	var markerSeq = -1
	var historyKept bool
	for _, r := range store.msgs {
		if r.Content == domain.ClearMarker {
			markerSeq = r.Seq
		}
		if r.Content == injectNotePrefix+"history-row" {
			historyKept = true
		}
	}
	if markerSeq < 0 {
		t.Fatal("clear marker was not persisted")
	}
	if markerSeq <= maxBefore {
		t.Fatalf("marker seq %d should sit above prior max %d (no reset)", markerSeq, maxBefore)
	}
	if !historyKept {
		t.Fatal("clear is append-only — the history row must remain in the durable log")
	}
}

func TestClearIsIdempotent(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("once")
	s.Clear()
	s.Clear()
	if len(s.Messages()) != 3 {
		t.Fatalf("a second clear must keep exactly 3 control messages, got %d", len(s.Messages()))
	}
}

func TestClearDropsPriorCompactionSummary(t *testing.T) {
	s, _ := compactSession(t, plainRouter())
	s.InjectNote("pre")
	s.Compact("goals: X. open: none. next: Y.")
	if len(s.Messages()) != 4 {
		t.Fatalf("compact should leave 4 messages, got %d", len(s.Messages()))
	}
	s.Clear()
	msgs := s.Messages()
	if len(msgs) != 3 {
		t.Fatalf("clear after compact should leave 3, got %d", len(msgs))
	}
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "compacted summary") || strings.Contains(m.StringContent, "goals: X") {
			t.Fatal("clear must drop a prior compaction summary")
		}
	}
}

// --- auto-compaction threshold ---

// chatCountRouter counts Chat (auto-compact summarizer) invocations and returns a
// fixed summary; Stream returns a plain answer.
type chatCountRouter struct {
	chatCalls int
	summary   string
}

func (r *chatCountRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{Content: "ok"}, nil
}
func (r *chatCountRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	r.chatCalls++
	return models.ChatResult{Content: r.summary}, nil
}
func (r *chatCountRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

func TestAutoCompactsAboveThreshold(t *testing.T) {
	r := &chatCountRouter{summary: "AUTO_SUMMARY"}
	s, _ := compactSession(t, r)
	s.InjectNote("keep-small")
	// One huge note pushes the estimate past the 60k-token threshold (≈240k chars).
	s.InjectNote("GIANT_MARKER" + strings.Repeat("x", 260_000))
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 1 {
		t.Fatalf("auto-compact summarizer called %d times, want 1", r.chatCalls)
	}
	msgs := s.Messages()
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "GIANT_MARKER") {
			t.Fatal("oversized history should have been summarized away")
		}
	}
	var replaced bool
	for _, m := range msgs {
		if strings.Contains(m.StringContent, "compacted summary") && strings.Contains(m.StringContent, "AUTO_SUMMARY") {
			replaced = true
		}
	}
	if !replaced {
		t.Fatal("history should be replaced with the compacted summary note")
	}
}

func TestDoesNotAutoCompactSmallConversation(t *testing.T) {
	r := &chatCountRouter{summary: "X"}
	s, _ := compactSession(t, r)
	s.InjectNote("small note one")
	s.InjectNote("small note two")
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if r.chatCalls != 0 {
		t.Fatalf("small conversation must NOT auto-compact (chat called %d)", r.chatCalls)
	}
	var kept bool
	for _, m := range s.Messages() {
		if strings.Contains(m.StringContent, "small note one") {
			kept = true
		}
	}
	if !kept {
		t.Fatal("small conversation history must survive untouched")
	}
}
