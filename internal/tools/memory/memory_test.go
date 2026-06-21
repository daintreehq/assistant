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
