package grant

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// liveStore models the real grant store closely enough to assert list filtering,
// source provenance, and idempotent revocation. ListGrants(actorID) filters by
// actor (empty actorID ⇒ all); RevokeGrant zeroes a live grant once.
type liveStore struct {
	grants []domain.AutomationGrantRecord
}

func (s *liveStore) InsertGrant(_ context.Context, rec domain.AutomationGrantRecord) (string, error) {
	if rec.ID == "" {
		rec.ID = domain.NewID(domain.PrefixGrant)
	}
	s.grants = append(s.grants, rec)
	return rec.ID, nil
}

func (s *liveStore) ListGrants(_ context.Context, actorID string) ([]domain.AutomationGrantRecord, error) {
	var out []domain.AutomationGrantRecord
	for _, g := range s.grants {
		if actorID != "" && g.ActorID != actorID {
			continue
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *liveStore) RevokeGrant(_ context.Context, id string) (bool, error) {
	for i := range s.grants {
		g := &s.grants[i]
		// A live grant: not revoked, has uses remaining. Revoking zeroes it.
		if g.ID == id && g.RevokedAt == nil && g.UsesRemaining > 0 {
			now := domain.NowMS()
			g.RevokedAt = &now
			g.UsesRemaining = 0
			return true, nil
		}
	}
	return false, nil
}

func mainCtx() *tools.ToolContext {
	return &tools.ToolContext{
		Actor:   domain.ActorMain,
		Confirm: func(context.Context, tools.ConfirmRequest) (bool, error) { return true, nil },
	}
}

// grant.create stamps source="local" and grant.list reports live grants and
// filters by actor, exposing provenance.
func TestGrantCreateSourceLocalAndListFiltersByActor(t *testing.T) {
	st := &liveStore{}
	create := find(Tools(Deps{Store: st}), "grant.create")
	list := find(Tools(Deps{Store: st}), "grant.list")

	mk := func(actor string) {
		p := decode(t, create, `{"actorId":"`+actor+`","actorType":"watcher","allowedRiskClasses":["git"],"ttlMs":60000,"maxUses":3}`)
		if r := create.Handle(context.Background(), p, mainCtx()); !r.Ok {
			t.Fatalf("create: %+v", r.Error)
		}
	}
	mk("wch_1")
	mk("wch_2")

	// Provenance is stamped local.
	if st.grants[0].Source != domain.GrantSourceLocal {
		t.Fatalf("source: got %s want local", st.grants[0].Source)
	}

	all := list.Handle(context.Background(), decode(t, list, `{}`), mainCtx())
	if grants := all.Result.(map[string]any)["grants"].([]map[string]any); len(grants) != 2 {
		t.Fatalf("expected 2 grants, got %d", len(grants))
	}

	scoped := list.Handle(context.Background(), decode(t, list, `{"actorId":"wch_1"}`), mainCtx())
	grants := scoped.Result.(map[string]any)["grants"].([]map[string]any)
	if len(grants) != 1 {
		t.Fatalf("expected 1 scoped grant, got %d", len(grants))
	}
	if grants[0]["source"] != domain.GrantSourceLocal {
		t.Fatalf("list source: %v", grants[0]["source"])
	}
}

// An over-long ttl is rejected at validation, leaving no ghost row.
func TestGrantCreateRejectsOverLongTTLNoGhostRow(t *testing.T) {
	st := &liveStore{}
	create := find(Tools(Deps{Store: st}), "grant.create")
	// maxGrantTTLMs is 30 days; one ms beyond is rejected.
	args := json.RawMessage(`{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["read"],"ttlMs":2592000001,"maxUses":1}`)
	res := create.Handle(context.Background(), args, mainCtx())
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS for over-long ttl, got %+v", res)
	}
	if len(st.grants) != 0 {
		t.Fatalf("over-long ttl left a ghost row: %+v", st.grants)
	}
}

// grant.revoke is main-only, idempotent (second revoke 404s), and a revoked
// grant has zero uses remaining.
func TestGrantRevokeForbiddenNonMainAndIdempotent(t *testing.T) {
	st := &liveStore{}
	create := find(Tools(Deps{Store: st}), "grant.create")
	revoke := find(Tools(Deps{Store: st}), "grant.revoke")

	created := create.Handle(context.Background(),
		decode(t, create, `{"actorId":"wch_1","actorType":"watcher","allowedRiskClasses":["git"],"ttlMs":60000,"maxUses":3}`), mainCtx())
	id := created.Result.(map[string]any)["id"].(string)

	// A non-interactive actor cannot revoke.
	denied := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`),
		&tools.ToolContext{Actor: domain.ActorWatcher})
	if denied.Ok || denied.Error.Code != codeActorForbidden {
		t.Fatalf("expected GRANT_ACTOR_FORBIDDEN, got %+v", denied)
	}

	if r := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`), mainCtx()); !r.Ok {
		t.Fatalf("first revoke: %+v", r.Error)
	}
	// Revoked grant carries zero uses remaining.
	if st.grants[0].UsesRemaining != 0 {
		t.Fatalf("revoked grant should have 0 uses remaining, got %d", st.grants[0].UsesRemaining)
	}
	// Second revoke finds nothing live → 404.
	again := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`), mainCtx())
	if again.Ok || again.Error.Code != codeGrantNotFound {
		t.Fatalf("expected GRANT_NOT_FOUND on second revoke, got %+v", again)
	}
}
