package agent

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

func msgRow(seq int, role, content string) domain.ConversationMessageRecord {
	return domain.ConversationMessageRecord{Seq: seq, Role: role, Content: content}
}

func TestRehydrateEmptyStartsFresh(t *testing.T) {
	if _, ok := RehydrateSession(nil); ok {
		t.Fatal("empty rows should start fresh (ok=false)")
	}
}

func TestRehydrateDupSeqResumesEmptyAtMaxSeqPlusOne(t *testing.T) {
	// A dup-seq tangle is a safe fresh start, but it must RESUME (ok=true) with an
	// EMPTY working history and continue numbering at maxSeq+1 — NOT return "fresh"
	// (which would restart persisting at seq 0 and keep colliding with the dirty
	// rows on every later resume). Spec §7.2 / §17.4.
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "a"), msgRow(0, "system", "b"), // duplicate seq 0
		msgRow(7, "user", "later"), // the largest seq among the dirty rows
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("dup-seq tangle should RESUME empty (ok=true), not start fresh")
	}
	if res.RestoredMessages == nil || len(res.RestoredMessages) != 0 {
		t.Fatalf("dup-seq resume must restore an EMPTY working history, got %v", res.RestoredMessages)
	}
	if !res.DirtyFreshStart {
		t.Fatal("dup-seq resume must be marked DirtyFreshStart so a clear breadcrumb is persisted")
	}
	// initialSeq = max(seq)+1 = 8, so new rows never collide with the dirty rows.
	if res.InitialSeq != 8 {
		t.Fatalf("initialSeq=%d want 8 (maxSeq+1, past the dirty rows)", res.InitialSeq)
	}
}

func TestDirtyFreshStartSessionContinuesSeqPastDirtyRows(t *testing.T) {
	// End-to-end of #5: a DirtyFreshStart resume must persist its clear breadcrumb
	// (and every later append) at seq >= maxSeq+1, so a new row NEVER collides with
	// the dirty dup-seq rows. The clear marker proves a clean post-marker history.
	store := &recordingStore{}
	deps := SessionDeps{
		Router:        plainRouter(),
		Tools:         &fakeTools{},
		SkillSelector: fakeSelector{},
		SkillCatalog:  fakeCatalog{},
		Store:         store,
		SessionID:     "ses_dirty",
		// Resume EMPTY at maxSeq+1 = 8, flagged dirty so a clear breadcrumb persists.
		RestoredMessages: []models.ChatMessage{},
		InitialSeq:       8,
		DirtyFreshStart:  true,
		Events:           NoopEventSink{},
	}
	s := NewSession(deps)

	// The clear breadcrumb was the only row persisted on construction (controls are
	// NOT re-persisted on resume), and it landed at the resumed seq 8 — past 0..7.
	if len(store.msgs) != 1 {
		t.Fatalf("persisted rows = %d want 1 (the clear breadcrumb)", len(store.msgs))
	}
	if store.msgs[0].Seq != 8 || store.msgs[0].Content != domain.ClearMarker {
		t.Fatalf("breadcrumb = {seq:%d content:%q} want {seq:8 ClearMarker}", store.msgs[0].Seq, store.msgs[0].Content)
	}

	// A subsequent append continues from 9 — still collision-free with the dirty rows.
	s.InjectNote("after the dirty-fresh-start")
	last := store.msgs[len(store.msgs)-1]
	if last.Seq != 9 {
		t.Fatalf("next append seq = %d want 9 (continues past the breadcrumb)", last.Seq)
	}
}

func TestRehydrateDropsControlRows(t *testing.T) {
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		msgRow(3, "user", "hello"), msgRow(4, "assistant", "hi"),
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if res.InitialSeq != 5 {
		t.Fatalf("initialSeq=%d want 5", res.InitialSeq)
	}
	if len(res.RestoredMessages) != 2 {
		t.Fatalf("restored %d msgs want 2 (controls dropped)", len(res.RestoredMessages))
	}
	if res.RestoredMessages[0].StringContent != "hello" {
		t.Fatalf("first restored = %q want hello", res.RestoredMessages[0].StringContent)
	}
}

func TestRehydrateClearMarkerEmptiesHistory(t *testing.T) {
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		msgRow(3, "user", "hello"), msgRow(4, "assistant", "hi"),
		msgRow(5, "system", domain.ClearMarker),
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if len(res.RestoredMessages) != 0 {
		t.Fatalf("clear marker should empty working history, got %d", len(res.RestoredMessages))
	}
	if res.InitialSeq != 6 {
		t.Fatalf("initialSeq=%d want 6", res.InitialSeq)
	}
}

func TestRehydrateCompactMarkerKeepsAfter(t *testing.T) {
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		msgRow(3, "user", "old"), msgRow(4, "assistant", "old reply"),
		msgRow(5, "system", compactionMarker),
		msgRow(6, "user", compactedNotePrefix+"the summary"),
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("after-compact history len=%d want 1", len(res.RestoredMessages))
	}
	if res.RestoredMessages[0].StringContent != compactedNotePrefix+"the summary" {
		t.Fatalf("unexpected restored content %q", res.RestoredMessages[0].StringContent)
	}
}

func TestDropOrphanToolResults(t *testing.T) {
	toolCalls := `[{"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{}"}}]`
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		{Seq: 3, Role: "assistant", Content: "", ToolCallsJson: &toolCalls},
		{Seq: 4, Role: "tool", Content: "{}", ToolCallID: strp("call_1")},   // matched — kept
		{Seq: 5, Role: "tool", Content: "{}", ToolCallID: strp("orphan_9")}, // orphan — dropped
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	// assistant + matched tool result = 2; orphan dropped.
	if len(res.RestoredMessages) != 2 {
		t.Fatalf("restored %d want 2 (orphan tool result dropped)", len(res.RestoredMessages))
	}
	for _, m := range res.RestoredMessages {
		if m.Role == "tool" && m.ToolCallID == "orphan_9" {
			t.Fatal("orphan tool result was not dropped")
		}
	}
}

func TestDropOrphanToolCallTail(t *testing.T) {
	toolCalls := `[{"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{}"}}]`
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		msgRow(3, "user", "hello"),
		{Seq: 4, Role: "assistant", Content: "", ToolCallsJson: &toolCalls}, // unanswered tail
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	// The unanswered trailing assistant tool-call exchange is cut; only "hello" survives.
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1 (incomplete tool-call tail cut)", len(res.RestoredMessages))
	}
	if res.RestoredMessages[0].Role != "user" {
		t.Fatalf("survivor role = %q want user", res.RestoredMessages[0].Role)
	}
}

func strp(s string) *string { return &s }
