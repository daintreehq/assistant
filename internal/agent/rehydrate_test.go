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
	// rows on every later resume).
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
		msgRow(6, "user", compactionNotePrefix(1)+"the summary"),
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("after-compact history len=%d want 1", len(res.RestoredMessages))
	}
	if res.RestoredMessages[0].StringContent != compactionNotePrefix(1)+"the summary" {
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
	if res.DroppedRows != 1 {
		t.Fatalf("DroppedRows = %d want 1 (the orphan tool result)", res.DroppedRows)
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
	if res.DroppedRows != 1 {
		t.Fatalf("DroppedRows = %d want 1 (the cut assistant tail row)", res.DroppedRows)
	}
	if res.RestoredMessages[0].Role != "user" {
		t.Fatalf("survivor role = %q want user", res.RestoredMessages[0].Role)
	}
}

// TestRehydrateDroppedRowsAccumulatesAllKinds proves DroppedRows sums every drop
// kind across the three passes: a malformed tool-call row (calls lost, +1), an
// orphan tool result with no declaration (+1), and an incomplete trailing tool-call
// exchange — the tail assistant declares two calls but only one is answered, so the
// cut removes the assistant row plus its one answered (now partial) result (+2).
// Total 4.
func TestRehydrateDroppedRowsAccumulatesAllKinds(t *testing.T) {
	// The tail assistant declares call_a AND call_b; only call_a is answered. The
	// call_a result is declared (survives the orphan pass) but the whole exchange is
	// incomplete, so the tail cut removes both the assistant row and that result.
	tail := `[{"id":"call_a","type":"function","function":{"name":"fs.read","arguments":"{}"}},` +
		`{"id":"call_b","type":"function","function":{"name":"fs.list","arguments":"{}"}}]`
	bad := "{not json"
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		// (a) malformed tool-call JSON: text kept, calls dropped → +1 (this row survives).
		{Seq: 3, Role: "assistant", Content: "kept", ToolCallsJson: strp(bad)},
		// (b) orphan tool result — its id is never declared → +1 in the orphan pass.
		{Seq: 4, Role: "tool", Content: "{}", ToolCallID: strp("ghost")},
		// (c) incomplete trailing exchange — declares call_a + call_b, only call_a
		//     answered. The tail cut removes the assistant row + the call_a result → +2.
		{Seq: 5, Role: "assistant", Content: "", ToolCallsJson: strp(tail)},
		{Seq: 6, Role: "tool", Content: "{}", ToolCallID: strp("call_a")},
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if res.DroppedRows != 4 {
		t.Fatalf("DroppedRows = %d want 4 (1 malformed + 1 orphan + 2 tail-cut)", res.DroppedRows)
	}
	// Only the malformed-but-text-bearing assistant row survives.
	if len(res.RestoredMessages) != 1 || res.RestoredMessages[0].StringContent != "kept" {
		t.Fatalf("survivors = %+v want exactly the 'kept' assistant row", res.RestoredMessages)
	}
}

// TestRehydrateCleanResumeNoDrops guards against false positives: a well-formed
// history reports zero drops.
func TestRehydrateCleanResumeNoDrops(t *testing.T) {
	call := `[{"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{}"}}]`
	rows := []domain.ConversationMessageRecord{
		msgRow(0, "system", "base"), msgRow(1, "system", "rt"), msgRow(2, "system", "skills"),
		msgRow(3, "user", "hello"),
		{Seq: 4, Role: "assistant", Content: "", ToolCallsJson: strp(call)},
		{Seq: 5, Role: "tool", Content: "{}", ToolCallID: strp("call_1")},
		msgRow(6, "assistant", "done"),
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if res.DroppedRows != 0 {
		t.Fatalf("DroppedRows = %d want 0 (clean history)", res.DroppedRows)
	}
}

func strp(s string) *string { return &s }
