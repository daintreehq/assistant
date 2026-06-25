package agent

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

func controlRows() []domain.ConversationMessageRecord {
	return []domain.ConversationMessageRecord{
		msgRow(0, "system", "You are Daintree Assistant"),
		msgRow(1, "system", "# Runtime context"),
		msgRow(2, "system", "# Loaded skills"),
	}
}

func TestRehydrateOnlyControlRowsEmptyHistory(t *testing.T) {
	res, ok := RehydrateSession(controlRows())
	if !ok {
		t.Fatal("control-only session should resume (ok=true), not start fresh")
	}
	if len(res.RestoredMessages) != 0 {
		t.Fatalf("restored %d want 0", len(res.RestoredMessages))
	}
	if res.InitialSeq != 3 {
		t.Fatalf("initialSeq = %d want 3", res.InitialSeq)
	}
}

func TestRehydrateKeepsOnlyLatestSummary(t *testing.T) {
	rows := append(controlRows(),
		msgRow(3, "user", "turn one"),
		msgRow(4, "system", compactionMarker),
		msgRow(5, "user", compactionNotePrefix(1)+"FIRST"),
		msgRow(6, "user", "turn two"),
		msgRow(7, "system", compactionMarker),
		msgRow(8, "user", compactionNotePrefix(2)+"SECOND"),
	)
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if res.InitialSeq != 9 {
		t.Fatalf("initialSeq = %d want 9", res.InitialSeq)
	}
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1 (only the latest summary)", len(res.RestoredMessages))
	}
	if !strings.Contains(res.RestoredMessages[0].StringContent, "SECOND") {
		t.Fatalf("latest summary not kept: %q", res.RestoredMessages[0].StringContent)
	}
	for _, m := range res.RestoredMessages {
		if strings.Contains(m.StringContent, "FIRST") {
			t.Fatal("the earlier summary must be dropped")
		}
	}
}

func TestRehydrateClearMarkerExactConstantCanary(t *testing.T) {
	// Canary: RehydrateSession matches the clear marker by the SAME constant Clear()
	// writes, so the two sides can't drift. If the marker text changes, this fails.
	rows := append(controlRows(),
		msgRow(3, "user", injectNotePrefix+"old"),
		msgRow(4, "system", domain.ClearMarker),
	)
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if len(res.RestoredMessages) != 0 {
		t.Fatalf("clear marker should empty history, got %d", len(res.RestoredMessages))
	}
}

func TestRehydrateRestoresOnlyPostClearTurns(t *testing.T) {
	rows := append(controlRows(),
		msgRow(3, "user", injectNotePrefix+"old"),
		msgRow(4, "system", domain.ClearMarker),
		msgRow(5, "user", injectNotePrefix+"after clear"),
	)
	res, _ := RehydrateSession(rows)
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1 (only post-clear)", len(res.RestoredMessages))
	}
	if !strings.Contains(res.RestoredMessages[0].StringContent, "after clear") {
		t.Fatalf("unexpected survivor: %q", res.RestoredMessages[0].StringContent)
	}
}

func TestRehydrateClearAfterCompactDropsAll(t *testing.T) {
	// The LAST marker is the boundary regardless of kind — a clear after a compact
	// drops even the compaction summary.
	rows := append(controlRows(),
		msgRow(3, "user", "turn one"),
		msgRow(4, "system", compactionMarker),
		msgRow(5, "user", compactionNotePrefix(1)+"SUMMARY"),
		msgRow(6, "system", domain.ClearMarker),
	)
	res, _ := RehydrateSession(rows)
	if len(res.RestoredMessages) != 0 {
		t.Fatalf("clear-after-compact should drop all, got %d", len(res.RestoredMessages))
	}
}

func TestRehydrateMalformedToolCallJSONKeepsText(t *testing.T) {
	bad := "{not json"
	rows := append(controlRows(),
		domain.ConversationMessageRecord{Seq: 3, Role: "assistant", Content: "text", ToolCallsJson: &bad},
	)
	res, _ := RehydrateSession(rows)
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1", len(res.RestoredMessages))
	}
	if res.DroppedRows != 1 {
		t.Fatalf("DroppedRows = %d want 1 (the lost tool-call list)", res.DroppedRows)
	}
	m := res.RestoredMessages[0]
	if m.Role != "assistant" || m.StringContent != "text" {
		t.Fatalf("message mangled: %+v", m)
	}
	if len(m.ToolCalls) != 0 {
		t.Fatal("malformed tool-call JSON should drop only the calls, keeping the text")
	}
}

func TestRehydrateMalformedToolCallsTextlessAvoidsNullContent(t *testing.T) {
	// A malformed tool-call row with EMPTY content must not resume as a null-content
	// assistant with no tool_calls ({role:"assistant", content:null} is only valid
	// alongside tool_calls). It must downgrade to an empty string instead.
	bad := "{not json"
	rows := append(controlRows(),
		domain.ConversationMessageRecord{Seq: 3, Role: "assistant", Content: "", ToolCallsJson: &bad},
	)
	res, _ := RehydrateSession(rows)
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1", len(res.RestoredMessages))
	}
	m := res.RestoredMessages[0]
	if m.ContentNull {
		t.Fatal("a textless malformed-tool-call row must NOT resume with null content")
	}
	if len(m.ToolCalls) != 0 || m.StringContent != "" {
		t.Fatalf("want empty-string content and no tool calls, got %+v", m)
	}
	if res.DroppedRows != 1 {
		t.Fatalf("DroppedRows = %d want 1", res.DroppedRows)
	}
}

func TestRehydrateTrimsPartialMultiToolBatch(t *testing.T) {
	calls := `[{"id":"call_1","type":"function","function":{"name":"fs.read","arguments":"{}"}},` +
		`{"id":"call_2","type":"function","function":{"name":"fs.list","arguments":"{}"}}]`
	rows := append(controlRows(),
		msgRow(3, "user", "do both"),
		domain.ConversationMessageRecord{Seq: 4, Role: "assistant", Content: "", ToolCallsJson: &calls},
		domain.ConversationMessageRecord{Seq: 5, Role: "tool", Content: "r1", ToolCallID: strp("call_1")},
		// call_2 result never written — the assistant turn is incomplete.
	)
	res, _ := RehydrateSession(rows)
	// The incomplete assistant turn AND its partial result are trimmed.
	if len(res.RestoredMessages) != 1 {
		t.Fatalf("restored %d want 1 (whole partial batch trimmed)", len(res.RestoredMessages))
	}
	if res.DroppedRows != 2 {
		t.Fatalf("DroppedRows = %d want 2 (assistant row + its partial result)", res.DroppedRows)
	}
	if res.RestoredMessages[0].StringContent != "do both" {
		t.Fatalf("survivor = %q want 'do both'", res.RestoredMessages[0].StringContent)
	}
}

func TestDropOrphanToolResultsIsForwardPass(t *testing.T) {
	// A tool result that PRECEDES the assistant message declaring its id is an orphan
	// in transcript order and must be dropped — even though a LATER assistant message
	// declares the same id. The old whole-history pre-collect wrongly kept it. #4.
	msgs := []models.ChatMessage{
		// Tool result for call_x appears BEFORE any declaration.
		{Role: "tool", ToolCallID: "call_x", StringContent: "premature"},
		// Later, an assistant message declares call_x — must NOT rescue the earlier result.
		{Role: "assistant", ToolCalls: []models.ToolCallRequest{
			{ID: "call_x", Type: "function", Function: models.ToolCallFunction{Name: "fs.read"}},
		}},
		// A properly-ordered result AFTER its declaration is kept.
		{Role: "tool", ToolCallID: "call_x", StringContent: "answered"},
	}
	out, dropped := dropOrphanToolResults(msgs)

	// The premature tool result is dropped; the assistant + the in-order result stay.
	if len(out) != 2 {
		t.Fatalf("kept %d messages want 2 (premature orphan dropped)", len(out))
	}
	if dropped != 1 {
		t.Fatalf("dropped = %d want 1 (the premature orphan)", dropped)
	}
	if out[0].Role != "assistant" {
		t.Fatalf("out[0] = %q want assistant (premature tool result must be gone)", out[0].Role)
	}
	if out[1].Role != "tool" || out[1].StringContent != "answered" {
		t.Fatalf("out[1] = %+v want the in-order 'answered' tool result", out[1])
	}
}

func TestRehydrateLargeRowSetInitialSeq(t *testing.T) {
	rows := controlRows()
	for i := 3; i < 1500; i++ {
		rows = append(rows, msgRow(i, "user", "n"))
	}
	res, ok := RehydrateSession(rows)
	if !ok {
		t.Fatal("expected resume")
	}
	if res.InitialSeq != 1500 {
		t.Fatalf("initialSeq = %d want 1500", res.InitialSeq)
	}
	if len(res.RestoredMessages) != 1497 {
		t.Fatalf("restored %d want 1497", len(res.RestoredMessages))
	}
}

// --- resume integration (NewSession with RestoredMessages) ---

// resumeDeps builds a resumed-session SessionDeps from a rehydrate result. A
// non-nil RestoredMessages (even empty) is the resume discriminator.
func resumeDeps(t *testing.T, store *recordingStore, restore RehydrateResult) SessionDeps {
	t.Helper()
	reg, err := skills.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	restored := restore.RestoredMessages
	if restored == nil {
		restored = []models.ChatMessage{}
	}
	return SessionDeps{
		Router:           plainRouter(),
		Tools:            &fakeTools{},
		SkillSelector:    fakeSelector{},
		SkillCatalog:     reg,
		Store:            store,
		SessionID:        "ses_resume",
		Events:           NoopEventSink{},
		RestoredMessages: restored,
		InitialSeq:       restore.InitialSeq,
	}
}

func TestResumeRebuildsControlsFreshNotRepersisted(t *testing.T) {
	store := &recordingStore{}
	// First run: write 3 controls + 2 notes into the recording store.
	first := newOnStore(t, store)
	first.InjectNote("one")
	first.InjectNote("two")
	firstRowCount := len(store.msgs)

	// Rehydrate from the persisted rows, resume a fresh session.
	restore, ok := RehydrateSession(store.msgs)
	if !ok {
		t.Fatal("expected resume")
	}
	resumed := NewSession(resumeDeps(t, store, restore))

	msgs := resumed.Messages()
	if len(msgs) != 5 {
		t.Fatalf("resumed messages = %d want 5 (3 controls + 2 notes)", len(msgs))
	}
	if !strings.Contains(msgs[1].StringContent, "# Runtime context") {
		t.Fatal("controls should be rebuilt fresh")
	}
	if !strings.Contains(msgs[3].StringContent, "one") || !strings.Contains(msgs[4].StringContent, "two") {
		t.Fatalf("restored notes wrong: %+v", msgs[3:])
	}
	// Resume must NOT re-persist the 3 control rows.
	if len(store.msgs) != firstRowCount {
		t.Fatalf("resume re-persisted rows: %d != %d", len(store.msgs), firstRowCount)
	}
}

func TestResumeIsIdempotentNoDBGrowth(t *testing.T) {
	store := &recordingStore{}
	first := newOnStore(t, store)
	first.InjectNote("only note")
	baseline := len(store.msgs)

	r1, _ := RehydrateSession(store.msgs)
	a := NewSession(resumeDeps(t, store, r1))
	if len(store.msgs) != baseline {
		t.Fatalf("first resume grew the DB: %d != %d", len(store.msgs), baseline)
	}
	r2, _ := RehydrateSession(store.msgs)
	b := NewSession(resumeDeps(t, store, r2))
	if len(store.msgs) != baseline {
		t.Fatalf("second resume grew the DB: %d != %d", len(store.msgs), baseline)
	}
	if len(a.Messages()) != len(b.Messages()) {
		t.Fatalf("resume not idempotent: %d != %d", len(a.Messages()), len(b.Messages()))
	}
}

// newOnStore builds a fresh (non-resumed) session that persists to the given store.
func newOnStore(t *testing.T, store *recordingStore) *Session {
	t.Helper()
	reg, err := skills.BuiltinRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return NewSession(SessionDeps{
		Router:        plainRouter(),
		Tools:         &fakeTools{},
		SkillSelector: fakeSelector{},
		SkillCatalog:  reg,
		Store:         store,
		SessionID:     "ses_resume",
		Events:        NoopEventSink{},
	})
}
