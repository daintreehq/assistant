package grant

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// liveStore models the real grant store closely enough to assert list filtering,
// source provenance, and idempotent revocation. ListGrants(actorID) filters by actor
// (empty actorID ⇒ all — it does NOT filter to live grants; the handler does that).
// RevokeGrant stamps revokedAt whenever it is unset and reports the PRE-call
// liveness, exactly like storage.Store.RevokeGrant.
type liveStore struct {
	grants []domain.AutomationGrantRecord
	// revokeErr, when set, makes RevokeGrant fail — so the handler's storage-error
	// branch is reachable and provably distinct from GRANT_NOT_FOUND.
	revokeErr error
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

func (s *liveStore) RevokeGrant(_ context.Context, id string) (found, didRevoke bool, err error) {
	if s.revokeErr != nil {
		return false, false, s.revokeErr
	}
	// One captured timestamp for both the liveness test and the stamp, matching the
	// real adapter (which passes a single domain.NowMS() into the store).
	now := domain.NowMS()
	for i := range s.grants {
		g := &s.grants[i]
		if g.ID != id {
			continue
		}
		// Live = not revoked, not expired, uses remaining — the real store's predicate.
		live := g.RevokedAt == nil && g.ExpiresAt > now && g.UsesRemaining > 0
		// The stamp lands whenever revokedAt is unset, INCLUDING for an expired or
		// used-up grant: that is the explicit revoke the column exists to record, and
		// it is the only kill a backwards clock step cannot undo. Note what is NOT
		// touched — usesRemaining. Revocation and exhaustion are separate states, and
		// the real store never conflates them.
		if g.RevokedAt == nil {
			stamp := now
			g.RevokedAt = &stamp
		}
		return true, live, nil
	}
	return false, false, nil
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

// grant.revoke is main-only and idempotent: revoking twice SUCCEEDS both times,
// the second reporting alreadyRevoked. Only an id with no grant at all is an error.
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

	first := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`), mainCtx())
	if !first.Ok {
		t.Fatalf("first revoke: %+v", first.Error)
	}
	// alreadyRevoked is present on BOTH branches, so the model reads a value rather
	// than inferring meaning from a missing key.
	if got := first.Result.(map[string]any)["alreadyRevoked"]; got != false {
		t.Fatalf("first revoke should report alreadyRevoked=false, got %v", got)
	}
	if st.grants[0].RevokedAt == nil {
		t.Fatalf("first revoke should stamp revokedAt")
	}
	// Revocation and exhaustion are separate states — revoking must not drain uses.
	if st.grants[0].UsesRemaining != 3 {
		t.Fatalf("revoke must not touch usesRemaining, got %d", st.grants[0].UsesRemaining)
	}

	// The second revoke is the bug this test now pins: the grant is right there, in
	// exactly the state asked for. That is a success, not a GRANT_NOT_FOUND — the red
	// × it used to produce cost a whole model round explaining a non-problem.
	again := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`), mainCtx())
	if !again.Ok {
		t.Fatalf("second revoke must succeed, got %+v", again.Error)
	}
	if got := again.Result.(map[string]any)["alreadyRevoked"]; got != true {
		t.Fatalf("second revoke should report alreadyRevoked=true, got %v", got)
	}

	// An id with no grant behind it is still a real failure.
	missing := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"grt_nope"}`), mainCtx())
	if missing.Ok || missing.Error.Code != codeGrantNotFound {
		t.Fatalf("expected GRANT_NOT_FOUND for an unknown id, got %+v", missing)
	}
}

// A grant that went inert on its own — expired, or out of uses, but never explicitly
// revoked — is the other half of the same bug: grant.revoke reported it as missing.
func TestGrantRevokeSucceedsOnExpiredAndExhaustedGrants(t *testing.T) {
	now := domain.NowMS()
	st := &liveStore{grants: []domain.AutomationGrantRecord{
		{ID: "grt_expired", ActorID: "wch_1", ActorType: domain.GrantActorWatcher,
			ExpiresAt: now - 1, MaxUses: 3, UsesRemaining: 3},
		{ID: "grt_used", ActorID: "wch_1", ActorType: domain.GrantActorWatcher,
			ExpiresAt: now + 60_000, MaxUses: 1, UsesRemaining: 0},
	}}
	revoke := find(Tools(Deps{Store: st}), "grant.revoke")

	for _, id := range []string{"grt_expired", "grt_used"} {
		res := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"`+id+`"}`), mainCtx())
		if !res.Ok {
			t.Fatalf("%s: revoking an inert grant must succeed, got %+v", id, res.Error)
		}
		if got := res.Result.(map[string]any)["alreadyRevoked"]; got != true {
			t.Fatalf("%s: want alreadyRevoked=true, got %v", id, got)
		}
	}
	// alreadyRevoked says "there was no authority left", NOT "nothing was written":
	// the explicit revoke still lands its permanent stamp.
	for i := range st.grants {
		if st.grants[i].RevokedAt == nil {
			t.Fatalf("%s: an explicit revoke must stamp revokedAt", st.grants[i].ID)
		}
	}
}

// A storage failure is not a missing grant. The tool promises that GRANT_NOT_FOUND
// means exactly one thing, so every other failure has to come back as CodeInternal —
// otherwise the model is told, unrecoverably, to stop asking about a grant that may
// still be live.
func TestGrantRevokeStorageFailureIsInternalNotNotFound(t *testing.T) {
	st := &liveStore{revokeErr: errors.New("disk on fire")}
	revoke := find(Tools(Deps{Store: st}), "grant.revoke")
	res := revoke.Handle(context.Background(), decode(t, revoke, `{"id":"grt_x"}`), mainCtx())
	if res.Ok || res.Error.Code != domain.CodeInternal {
		t.Fatalf("want CodeInternal on a storage error, got %+v", res)
	}

	// Same reasoning for an entirely absent store.
	nilStore := find(Tools(Deps{}), "grant.revoke")
	res = nilStore.Handle(context.Background(), decode(t, nilStore, `{"id":"grt_x"}`), mainCtx())
	if res.Ok || res.Error.Code != domain.CodeInternal {
		t.Fatalf("want CodeInternal when storage is unavailable, got %+v", res)
	}
}
