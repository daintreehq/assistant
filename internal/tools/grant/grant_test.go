package grant

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memStore struct {
	inserted []domain.AutomationGrantRecord
}

func (m *memStore) InsertGrant(_ context.Context, rec domain.AutomationGrantRecord) (string, error) {
	m.inserted = append(m.inserted, rec)
	return rec.ID, nil
}
func (m *memStore) ListGrants(context.Context, string) ([]domain.AutomationGrantRecord, error) {
	return m.inserted, nil
}
func (m *memStore) RevokeGrant(context.Context, string) (bool, error) { return true, nil }

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

func decode(t *testing.T, tool *tools.Tool, raw string) json.RawMessage {
	t.Helper()
	p, err := tool.Decode(json.RawMessage(raw))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return p
}

// A non-main actor can never mint a grant.
func TestGrantCreateForbidsNonMain(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "grant.create")
	ctx := &tools.ToolContext{Actor: domain.ActorWatcher}
	p := decode(t, tool, `{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["read"],"ttlMs":1000,"maxUses":1}`)
	res := tool.Handle(context.Background(), p, ctx)
	if res.Ok || res.Error.Code != codeActorForbidden {
		t.Fatalf("expected GRANT_ACTOR_FORBIDDEN, got %+v", res)
	}
}

// An empty scope (no risks, no tools) is rejected.
func TestGrantCreateEmptyScope(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "grant.create")
	ctx := &tools.ToolContext{Actor: domain.ActorMain}
	p := decode(t, tool, `{"actorId":"wch_1","actorType":"watcher","ttlMs":1000,"maxUses":1}`)
	res := tool.Handle(context.Background(), p, ctx)
	if res.Ok || res.Error.Code != codeEmptyScope {
		t.Fatalf("expected GRANT_EMPTY_SCOPE, got %+v", res)
	}
}

// Granting the grant tools themselves — or the raw daintree.call escape hatch — is
// forbidden. daintree.call would let a watcher/timer reach ANY MCP method unattended,
// bypassing the per-method typed-wrapper gating.
func TestGrantCreateUngrantable(t *testing.T) {
	for _, tn := range []string{"grant.create", "grant.revoke", "daintree.call"} {
		tool := find(Tools(Deps{Store: &memStore{}}), "grant.create")
		ctx := &tools.ToolContext{Actor: domain.ActorMain}
		p := decode(t, tool, `{"actorId":"wch_1","actorType":"watcher","allowedToolNames":["`+tn+`"],"ttlMs":1000,"maxUses":1}`)
		res := tool.Handle(context.Background(), p, ctx)
		if res.Ok || res.Error.Code != codeUngrantableTool {
			t.Fatalf("granting %q must be ungrantable, got %+v", tn, res)
		}
	}
}

// A read-only scope does NOT mutate → no confirm needed → mints directly.
func TestGrantCreateReadScopeNoConfirm(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "grant.create")
	confirmed := false
	ctx := &tools.ToolContext{
		Actor:   domain.ActorMain,
		Confirm: func(context.Context, tools.ConfirmRequest) (bool, error) { confirmed = true; return false, nil },
	}
	p := decode(t, tool, `{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["read"],"ttlMs":1000,"maxUses":2}`)
	res := tool.Handle(context.Background(), p, ctx)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if confirmed {
		t.Fatal("read-only scope should not trigger a confirm prompt")
	}
	if len(st.inserted) != 1 || st.inserted[0].UsesRemaining != 2 {
		t.Fatalf("grant not minted correctly: %+v", st.inserted)
	}
}

// A mutating scope requires confirm; a declined confirm fails as USER_DECLINED.
func TestGrantCreateMutatingScopeConfirmDeclined(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "grant.create")
	ctx := &tools.ToolContext{
		Actor:   domain.ActorMain,
		Confirm: func(context.Context, tools.ConfirmRequest) (bool, error) { return false, nil },
	}
	p := decode(t, tool, `{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["git"],"ttlMs":1000,"maxUses":1}`)
	res := tool.Handle(context.Background(), p, ctx)
	if res.Ok || res.Error.Code != codeUserDeclined {
		t.Fatalf("expected USER_DECLINED, got %+v", res)
	}
}

// autoApprove bypasses the mutating-scope confirm.
func TestGrantCreateAutoApprove(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "grant.create")
	ctx := &tools.ToolContext{Actor: domain.ActorMain, Config: config.AppConfig{AutoApprove: true}}
	p := decode(t, tool, `{"actorId":"tmr_1","actorType":"timer","allowedToolNames":["terminal.sendCommand"],"ttlMs":1000,"maxUses":1}`)
	res := tool.Handle(context.Background(), p, ctx)
	if !res.Ok {
		t.Fatalf("expected ok under autoApprove, got %+v", res.Error)
	}
	if len(st.inserted) != 1 {
		t.Fatalf("grant not minted: %+v", st.inserted)
	}
}
