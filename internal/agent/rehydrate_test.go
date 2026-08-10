package agent

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

func msgRow(seq int, role, content string) domain.ConversationMessageRecord {
	return domain.ConversationMessageRecord{Seq: seq, Role: role, Content: content}
}

func TestRehydrateEmptyStartsFresh(t *testing.T) {
	if _, ok := RehydrateSession(nil); ok {
		t.Fatal("empty rows should start fresh (ok=false)")
	}
}

func TestResumeCheckpointNil(t *testing.T) {
	if depth, ok := ResumeCheckpoint(nil, RehydrateResult{InitialSeq: 5}); ok || depth != 0 {
		t.Fatalf("nil checkpoint want (0,false), got (%d,%v)", depth, ok)
	}
}

func TestResumeCheckpointConsistent(t *testing.T) {
	cp := &domain.ContextCheckpointRecord{CompactionDepth: 4, LastSeq: 9}
	depth, ok := ResumeCheckpoint(cp, RehydrateResult{InitialSeq: 12})
	if !ok || depth != 4 {
		t.Fatalf("consistent checkpoint want (4,true), got (%d,%v)", depth, ok)
	}
}

func TestResumeCheckpointAheadOfHistoryRejected(t *testing.T) {
	// LastSeq >= InitialSeq ⇒ the checkpoint claims a seq the conversation never
	// reached, so its depth is not trusted.
	cp := &domain.ContextCheckpointRecord{CompactionDepth: 4, LastSeq: 12}
	if depth, ok := ResumeCheckpoint(cp, RehydrateResult{InitialSeq: 12}); ok || depth != 0 {
		t.Fatalf("ahead-of-history checkpoint want (0,false), got (%d,%v)", depth, ok)
	}
}

func TestResumeCheckpointNegativeRejected(t *testing.T) {
	cp := &domain.ContextCheckpointRecord{CompactionDepth: -1, LastSeq: 1}
	if depth, ok := ResumeCheckpoint(cp, RehydrateResult{InitialSeq: 5}); ok || depth != 0 {
		t.Fatalf("negative-depth checkpoint want (0,false), got (%d,%v)", depth, ok)
	}
}

func TestResumeCheckpointSeqBoundaryAccepted(t *testing.T) {
	// LastSeq exactly one behind InitialSeq is the newest valid boundary (the >=
	// comparison rejects only at/after InitialSeq).
	cp := &domain.ContextCheckpointRecord{CompactionDepth: 7, LastSeq: 11}
	if depth, ok := ResumeCheckpoint(cp, RehydrateResult{InitialSeq: 12}); !ok || depth != 7 {
		t.Fatalf("boundary checkpoint want (7,true), got (%d,%v)", depth, ok)
	}
}

func TestSelectResumeCheckpointEmpty(t *testing.T) {
	if _, depth, ok := SelectResumeCheckpoint(nil, RehydrateResult{InitialSeq: 5}); ok || depth != 0 {
		t.Fatalf("empty slice want (_,0,false), got (%d,%v)", depth, ok)
	}
}

func TestSelectResumeCheckpointPicksLatestWhenValid(t *testing.T) {
	cps := []domain.ContextCheckpointRecord{
		{Slot: "latest", CompactionDepth: 5, LastSeq: 8},
		{Slot: "prev", CompactionDepth: 4, LastSeq: 6},
	}
	cp, depth, ok := SelectResumeCheckpoint(cps, RehydrateResult{InitialSeq: 12})
	if !ok || depth != 5 || cp.Slot != "latest" {
		t.Fatalf("want latest/depth5, got slot=%s depth=%d ok=%v", cp.Slot, depth, ok)
	}
}

func TestSelectResumeCheckpointFallsBackToPrevWhenLatestStale(t *testing.T) {
	// 'latest' is semantically stale (seq ahead of the replayed delta) but JSON-valid,
	// so SelectResumeCheckpoint must fall back to the valid 'prev' — the cross-failure-
	// mode fallback the issue requires.
	cps := []domain.ContextCheckpointRecord{
		{Slot: "latest", CompactionDepth: 9, LastSeq: 99}, // ahead of InitialSeq=12 ⇒ rejected
		{Slot: "prev", CompactionDepth: 4, LastSeq: 6},    // valid ⇒ chosen
	}
	cp, depth, ok := SelectResumeCheckpoint(cps, RehydrateResult{InitialSeq: 12})
	if !ok || depth != 4 || cp.Slot != "prev" {
		t.Fatalf("want fallback to prev/depth4, got slot=%s depth=%d ok=%v", cp.Slot, depth, ok)
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
	r := plainRouter()
	deps := SessionDeps{
		Backend:   backendFromRouter{r: r},
		Tools:     &fakeTools{},
		Store:     store,
		SessionID: "ses_dirty",
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
		{Seq: 0, Role: "assistant", Content: "", ToolCallsJson: &toolCalls},
		{Seq: 1, Role: "tool", Content: "{}", ToolCallID: strp("call_1")},   // matched — kept
		{Seq: 2, Role: "tool", Content: "{}", ToolCallID: strp("orphan_9")}, // orphan — dropped
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
		msgRow(0, "user", "hello"),
		{Seq: 1, Role: "assistant", Content: "", ToolCallsJson: &toolCalls}, // unanswered tail
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
		// (a) malformed tool-call JSON: text kept, calls dropped → +1 (this row survives).
		{Seq: 0, Role: "assistant", Content: "kept", ToolCallsJson: strp(bad)},
		// (b) orphan tool result — its id is never declared → +1 in the orphan pass.
		{Seq: 1, Role: "tool", Content: "{}", ToolCallID: strp("ghost")},
		// (c) incomplete trailing exchange — declares call_a + call_b, only call_a
		//     answered. The tail cut removes the assistant row + the call_a result → +2.
		{Seq: 2, Role: "assistant", Content: "", ToolCallsJson: strp(tail)},
		{Seq: 3, Role: "tool", Content: "{}", ToolCallID: strp("call_a")},
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
