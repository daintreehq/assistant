package memory

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memStore struct {
	lastRecallLimit int
	lastListLimit   int
	inserted        []domain.MemoryRecord
	forgetFound     bool
}

func (m *memStore) RecallMemories(_ context.Context, _ string, opts RecallOptions) ([]domain.MemoryRecord, error) {
	m.lastRecallLimit = opts.Limit
	return nil, nil
}
func (m *memStore) ListMemories(_ context.Context, opts ListOptions) ([]domain.MemoryRecord, error) {
	m.lastListLimit = opts.Limit
	return nil, nil
}
func (m *memStore) InsertMemory(_ context.Context, rec domain.MemoryRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ForgetMemory(context.Context, string) (bool, error) { return m.forgetFound, nil }
func (m *memStore) PinMemory(_ context.Context, id string) (*domain.MemoryRecord, error) {
	now := domain.NowMS()
	return &domain.MemoryRecord{ID: id, PinnedAt: &now}, nil
}
func (m *memStore) UnpinMemory(_ context.Context, id string) (*domain.MemoryRecord, error) {
	return &domain.MemoryRecord{ID: id}, nil
}

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// recall clamps an over-max limit down to 50.
func TestRecallClampsLimit(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.recall")
	res := tool.Handle(context.Background(), json.RawMessage(`{"query":"x","limit":9999}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.lastRecallLimit != 50 {
		t.Fatalf("limit not clamped: got %d want 50", st.lastRecallLimit)
	}
}

// list applies the default limit (50) when omitted.
func TestListDefaultLimit(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.list")
	res := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.lastListLimit != 50 {
		t.Fatalf("default limit: got %d want 50", st.lastListLimit)
	}
}

// save defaults source to assistant and rejects the reserved "compact".
func TestSaveSourceDefaultAndReserved(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"hi"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.inserted[0].Source != domain.MemoryAssistant {
		t.Fatalf("default source: got %s", st.inserted[0].Source)
	}
	// "compact" is not in the enum → strict decode rejects it as INVALID_ARGS.
	res = tool.Handle(context.Background(), json.RawMessage(`{"content":"hi","source":"compact"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for reserved source, got %+v", res)
	}
}

// forget of a missing memory is a non-recoverable MEMORY_NOT_FOUND.
func TestForgetNotFound(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{forgetFound: false}}), "memory.forget")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"mem_x"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeMemoryNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable MEMORY_NOT_FOUND, got %+v", res)
	}
}

// pin/unpin/forget fire the OnChange hook (so the caller can refresh the injected
// pinned-memory block); save and the read tools do not.
func TestPinStateToolsFireOnChange(t *testing.T) {
	for _, name := range []string{"memory.pin", "memory.unpin", "memory.forget"} {
		t.Run(name, func(t *testing.T) {
			st := &memStore{forgetFound: true}
			fired := 0
			tool := find(Tools(Deps{Store: st, OnChange: func() { fired++ }}), name)
			res := tool.Handle(context.Background(), json.RawMessage(`{"id":"mem_x"}`), &tools.ToolContext{})
			if !res.Ok {
				t.Fatalf("%s failed: %+v", name, res.Error)
			}
			if fired != 1 {
				t.Fatalf("%s OnChange fired %d times, want 1", name, fired)
			}
		})
	}
}

func TestSaveDoesNotFireOnChange(t *testing.T) {
	st := &memStore{}
	fired := 0
	tool := find(Tools(Deps{Store: st, OnChange: func() { fired++ }}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"x"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("save failed: %+v", res.Error)
	}
	if fired != 0 {
		t.Fatalf("save must not fire OnChange, fired %d", fired)
	}
}

// save with ttlMs stamps an absolute expiresAt (now + ttlMs) and surfaces it in the view.
func TestSaveStampsTTL(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	before := domain.NowMS()
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"x","ttlMs":60000}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	got := st.inserted[0].ExpiresAt
	if got == nil {
		t.Fatal("ttlMs should stamp a non-nil ExpiresAt")
	}
	if *got < before+60000 || *got > domain.NowMS()+60000 {
		t.Fatalf("expiresAt %d outside [now+ttl] window", *got)
	}
	view := res.Result.(map[string]any)["memory"].(map[string]any)
	if view["expiresAt"] != *got {
		t.Fatalf("view expiresAt = %v, want %d", view["expiresAt"], *got)
	}
}

// save stamps runId from the turn context (provenance) and, for episodic kind, the
// sessionId — while semantic saves never carry a sessionId. sessionId stays internal
// (absent from the view); runId surfaces.
func TestSaveStampsProvenanceAndEpisodic(t *testing.T) {
	tctx := &tools.ToolContext{RunID: "run_42", SessionID: "ses_7"}

	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"x","kind":"episodic"}`), tctx)
	if !res.Ok {
		t.Fatalf("episodic save failed: %+v", res.Error)
	}
	rec := st.inserted[0]
	if rec.Kind != domain.MemoryKindEpisodic {
		t.Fatalf("kind = %q, want episodic", rec.Kind)
	}
	if rec.RunID == nil || *rec.RunID != "run_42" {
		t.Fatalf("runId = %v, want run_42", rec.RunID)
	}
	if rec.SessionID == nil || *rec.SessionID != "ses_7" {
		t.Fatalf("episodic sessionId = %v, want ses_7", rec.SessionID)
	}
	view := res.Result.(map[string]any)["memory"].(map[string]any)
	if view["runId"] != "run_42" {
		t.Fatalf("view runId = %v, want run_42", view["runId"])
	}
	if _, present := view["sessionId"]; present {
		t.Fatalf("sessionId must stay internal, leaked into view")
	}

	// Semantic (default) save with the same context: runId stamped, sessionId NOT.
	st2 := &memStore{}
	tool2 := find(Tools(Deps{Store: st2}), "memory.save")
	if res2 := tool2.Handle(context.Background(), json.RawMessage(`{"content":"y","kind":"semantic"}`), tctx); !res2.Ok {
		t.Fatalf("semantic save failed: %+v", res2.Error)
	}
	rec2 := st2.inserted[0]
	if rec2.Kind != domain.MemoryKindSemantic {
		t.Fatalf("kind = %q, want semantic", rec2.Kind)
	}
	if rec2.RunID == nil || *rec2.RunID != "run_42" {
		t.Fatalf("semantic runId = %v, want run_42", rec2.RunID)
	}
	if rec2.SessionID != nil {
		t.Fatalf("semantic save must not carry a sessionId, got %v", rec2.SessionID)
	}
}

// save rejects an out-of-enum kind with INVALID_ARGS (handler switch).
func TestSaveRejectsUnknownKind(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"x","kind":"bogus"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for unknown kind, got %+v", res)
	}
}

// the Decode/Validate path bounds ttlMs to [1, maxTtlMs]; 0 and an absurd value reject.
func TestSaveTTLBoundsViaDecode(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "memory.save")
	for _, raw := range []string{
		`{"content":"x","ttlMs":0}`,
		`{"content":"x","ttlMs":9999999999999999}`,
	} {
		if _, err := tool.Decode(json.RawMessage(raw)); err == nil {
			t.Fatalf("expected ttlMs out-of-bounds reject for %s", raw)
		}
	}
	// A valid ttlMs decodes cleanly.
	if _, err := tool.Decode(json.RawMessage(`{"content":"x","ttlMs":1000}`)); err != nil {
		t.Fatalf("valid ttlMs should decode, got %v", err)
	}
}

// the default (no kind) save always surfaces kind=semantic in the view.
func TestSaveViewDefaultsKind(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"x"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("save failed: %+v", res.Error)
	}
	view := res.Result.(map[string]any)["memory"].(map[string]any)
	if view["kind"] != domain.MemoryKindSemantic {
		t.Fatalf("view kind = %v, want semantic", view["kind"])
	}
	if _, present := view["expiresAt"]; present {
		t.Fatalf("expiresAt must be omitted when unset")
	}
	if _, present := view["runId"]; present {
		t.Fatalf("runId must be omitted when no turn context")
	}
}

// pin returns a view with pinned=true.
func TestPinView(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "memory.pin")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"mem_x"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	view := m["memory"].(map[string]any)
	if view["pinned"] != true {
		t.Fatalf("expected pinned=true, got %v", view["pinned"])
	}
}
