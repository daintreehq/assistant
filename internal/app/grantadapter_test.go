package app

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/tools/grant"
)

// grantStoreAdapter.RevokeGrant returns two adjacent bools of the same type, which is
// the classic silent-swap footgun: (didRevoke, found) instead of (found, didRevoke)
// compiles, and every OTHER test still passes — the storage tests never touch the
// adapter, and the handler tests use a hand-written fake that has its own ordering.
// The swap would resurrect the exact bug this all exists to fix (a second revoke
// reporting GRANT_NOT_FOUND), so it needs a test that crosses the seam: the real
// tools driving the real store through the real adapter.
func TestGrantRevokeThroughRealAdapter(t *testing.T) {
	s, err := storage.Open(":memory:", nil)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	deps := grant.Deps{Store: grantStoreAdapter{s: s}}
	create := findTool(t, grant.Tools(deps), "grant.create")
	revoke := findTool(t, grant.Tools(deps), "grant.revoke")
	ctx := &tools.ToolContext{
		Actor:   domain.ActorMain,
		Confirm: func(context.Context, tools.ConfirmRequest) (bool, error) { return true, nil },
	}

	created := create.Handle(context.Background(),
		json.RawMessage(`{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["git"],"ttlMs":600000,"maxUses":3}`), ctx)
	if !created.Ok {
		t.Fatalf("grant.create: %+v", created.Error)
	}
	id, _ := created.Result.(map[string]any)["id"].(string)
	if id == "" {
		t.Fatalf("grant.create returned no id: %+v", created.Result)
	}

	first := revoke.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`"}`), ctx)
	if !first.Ok {
		t.Fatalf("first revoke: %+v", first.Error)
	}
	if got := first.Result.(map[string]any)["alreadyRevoked"]; got != false {
		t.Fatalf("first revoke through the adapter: want alreadyRevoked=false, got %v", got)
	}

	// The regression that started issue #335. With the tuple swapped this returns
	// GRANT_NOT_FOUND.
	second := revoke.Handle(context.Background(), json.RawMessage(`{"id":"`+id+`"}`), ctx)
	if !second.Ok {
		t.Fatalf("second revoke through the adapter must succeed, got %+v", second.Error)
	}
	if got := second.Result.(map[string]any)["alreadyRevoked"]; got != true {
		t.Fatalf("second revoke through the adapter: want alreadyRevoked=true, got %v", got)
	}

	// And an id with no row behind it is still a real, unrecoverable failure.
	missing := revoke.Handle(context.Background(), json.RawMessage(`{"id":"grt_nope"}`), ctx)
	if missing.Ok || missing.Error.Recoverable {
		t.Fatalf("unknown id must fail unrecoverably, got %+v", missing)
	}
}

func findTool(t *testing.T, ts []*tools.Tool, name string) *tools.Tool {
	t.Helper()
	for _, tool := range ts {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return nil
}
