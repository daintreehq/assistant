package memory

import (
	"context"
	"encoding/json"
	"regexp"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

var memIDRe = regexp.MustCompile(`^mem_[0-9a-f]{8}$`)

// save returns an id of shape mem_<8 hex> and defaults source to assistant;
// memory.save is a local-risk (not read-risk) tool.
func TestSaveIDShapeAndLocalRisk(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	if tool.Risk != domain.RiskLocal {
		t.Fatalf("memory.save risk: got %s want local", tool.Risk)
	}
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"always run tsc directly"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	id := res.Result.(map[string]any)["id"].(string)
	if !memIDRe.MatchString(id) {
		t.Fatalf("id shape: %q", id)
	}
}

// save accepts an explicit user source.
func TestSaveAcceptsUserSource(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.save")
	res := tool.Handle(context.Background(), json.RawMessage(`{"content":"deploy from main","source":"user"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.inserted[0].Source != domain.MemoryUser {
		t.Fatalf("source: got %s want user", st.inserted[0].Source)
	}
}

// recall is read-risk and tolerates FTS operators/quotes without erroring
// (the handler delegates a raw query to the store; we assert no decode/guard
// rejection and the default recall limit of 10).
func TestRecallReadRiskToleratesFTSOperators(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "memory.recall")
	if tool.Risk != domain.RiskRead {
		t.Fatalf("memory.recall risk: got %s want read", tool.Risk)
	}
	for _, q := range []string{`"`, `watch "for"`, `watch OR operators`, `(unbalanced`} {
		body, _ := json.Marshal(map[string]any{"query": q})
		res := tool.Handle(context.Background(), body, &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("query %q should not error: %+v", q, res.Error)
		}
	}
	// Default recall limit is 10 (not the list default of 50).
	if st.lastRecallLimit != 10 {
		t.Fatalf("default recall limit: got %d want 10", st.lastRecallLimit)
	}
}

// unpin returns a view with pinned=false.
func TestUnpinView(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "memory.unpin")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"mem_x"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	view := res.Result.(map[string]any)["memory"].(map[string]any)
	if view["pinned"] != false {
		t.Fatalf("expected pinned=false, got %v", view["pinned"])
	}
}
