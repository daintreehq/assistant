package storage

import (
	"encoding/json"
	"regexp"
	"strconv"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// ---- grants: defaults, list scoping, revoke-by-actor ----

func TestGrantDefaultsAndListScoping(t *testing.T) {
	now := int64(10_000_000)
	s := openTest(t, now)
	mk := func(id, actor string, over func(*domain.AutomationGrantRecord)) domain.AutomationGrantRecord {
		r := domain.AutomationGrantRecord{
			ID: id, ActorID: actor, ActorType: domain.GrantActorWatcher,
			AllowedRiskClassesJson: strPtr(`["git"]`), ExpiresAt: now + 60_000, MaxUses: 3, CreatedAt: now,
		}
		if over != nil {
			over(&r)
		}
		g, err := s.InsertGrant(r)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	// usesRemaining defaults to maxUses; revokedAt null; source 'local'.
	g := mk("g_a", "wch_a", func(r *domain.AutomationGrantRecord) { r.MaxUses = 5 })
	fetched, _ := s.GetGrant(g.ID)
	if fetched.UsesRemaining != 5 || fetched.RevokedAt != nil || fetched.Source != domain.GrantSourceLocal {
		t.Fatalf("grant defaults wrong: uses=%d revoked=%v src=%s", fetched.UsesRemaining, fetched.RevokedAt, fetched.Source)
	}
	if live, _ := s.ListGrants("wch_a", now); len(live) == 0 || live[0].Source != domain.GrantSourceLocal {
		t.Fatalf("listGrants should expose source 'local'")
	}

	// listGrants returns only live grants, scoped to an actor.
	mk("g_b", "wch_b", nil)
	mk("g_exp", "wch_expired", func(r *domain.AutomationGrantRecord) { r.ID = "g_exp"; r.ExpiresAt = now - 1 })
	// An exhausted grant: maxUses 1, consumed once → usesRemaining 0, no longer live.
	mk("g_used", "wch_used", func(r *domain.AutomationGrantRecord) { r.ID = "g_used"; r.MaxUses = 1 })
	if c, _ := s.ConsumeGrant("wch_used", domain.GrantActorWatcher, "git.commit", domain.RiskGit, now); c == nil {
		t.Fatalf("setup: should consume the single use")
	}

	if got, _ := s.ListGrants("wch_expired", now); len(got) != 0 {
		t.Fatalf("expired grant must not be live")
	}
	if got, _ := s.ListGrants("wch_used", now); len(got) != 0 {
		t.Fatalf("exhausted grant must not be live")
	}
	all, _ := s.ListGrants("", now)
	actors := map[string]bool{}
	for _, gr := range all {
		actors[gr.ActorID] = true
	}
	if !actors["wch_a"] || !actors["wch_b"] || actors["wch_expired"] || actors["wch_used"] {
		t.Fatalf("unscoped list should be exactly the live grants, got %v", actors)
	}
}

func TestRevokeGrantIdempotentAndByActorCount(t *testing.T) {
	now := int64(10_000_000)
	s := openTest(t, now)
	mk := func(actor string) domain.AutomationGrantRecord {
		g, _ := s.InsertGrant(domain.AutomationGrantRecord{
			ActorID: actor, ActorType: domain.GrantActorWatcher,
			AllowedRiskClassesJson: strPtr(`["git"]`), ExpiresAt: now + 60_000, MaxUses: 3, CreatedAt: now,
		})
		return g
	}
	g := mk("wch_solo")
	if found, didRevoke, _ := s.RevokeGrant(g.ID, now); !found || !didRevoke {
		t.Fatalf("first revoke should find and change the row, got found=%v didRevoke=%v", found, didRevoke)
	}
	if c, _ := s.ConsumeGrant("wch_solo", domain.GrantActorWatcher, "git.commit", domain.RiskGit, now); c != nil {
		t.Fatalf("revoked grant must not consume")
	}
	// The second revoke is the case that used to be indistinguishable from a typo'd
	// id: the row is right there, already in the requested state. found stays true so
	// the tool can answer "already revoked" instead of "no such grant".
	if found, didRevoke, _ := s.RevokeGrant(g.ID, now); !found || didRevoke {
		t.Fatalf("second revoke: want found=true didRevoke=false, got found=%v didRevoke=%v", found, didRevoke)
	}
	// The revokedAt stamp from the FIRST revoke must survive the second call verbatim.
	if again, _ := s.GetGrant(g.ID); again.RevokedAt == nil || *again.RevokedAt != now {
		t.Fatalf("second revoke must not restamp revokedAt, got %v", again.RevokedAt)
	}
	if found, _, _ := s.RevokeGrant("grt_does_not_exist", now); found {
		t.Fatalf("an unknown id must report found=false")
	}

	mk("wch_x")
	mk("wch_x")
	mk("wch_y")
	if n, _ := s.RevokeGrantsByActor("wch_x", now); n != 2 {
		t.Fatalf("revokeGrantsByActor want 2, got %d", n)
	}
	if got, _ := s.ListGrants("wch_x", now); len(got) != 0 {
		t.Fatalf("wch_x grants should all be revoked")
	}
	if got, _ := s.ListGrants("wch_y", now); len(got) != 1 {
		t.Fatalf("wch_y grant must survive")
	}
}

// A grant that is dead by EXPIRY or by EXHAUSTION — never explicitly revoked — is
// reported, never written. revokedAt is an explicit-revoke marker only
// (domain.AutomationGrantRecord), so neither revoke path may stamp it, and neither
// may count such a grant as authority it withdrew.
func TestRevokeGrantReportsInertGrantsWithoutStampingThem(t *testing.T) {
	now := int64(10_000_000)
	s := openTest(t, now)
	mk := func(id, actor string, over func(*domain.AutomationGrantRecord)) domain.AutomationGrantRecord {
		r := domain.AutomationGrantRecord{
			ID: id, ActorID: actor, ActorType: domain.GrantActorWatcher,
			AllowedRiskClassesJson: strPtr(`["git"]`), ExpiresAt: now + 60_000, MaxUses: 3, CreatedAt: now,
		}
		if over != nil {
			over(&r)
		}
		g, err := s.InsertGrant(r)
		if err != nil {
			t.Fatal(err)
		}
		return g
	}

	expired := mk("g_expired", "wch_dead", func(r *domain.AutomationGrantRecord) { r.ExpiresAt = now - 1 })
	// Exhaustion has to be earned: InsertGrant defaults usesRemaining to maxUses, so a
	// one-use grant is drained by consuming it. (The expired grant above is not live,
	// so ConsumeGrant can only pick this one.)
	exhausted := mk("g_used", "wch_dead", func(r *domain.AutomationGrantRecord) { r.MaxUses = 1 })
	if c, _ := s.ConsumeGrant("wch_dead", domain.GrantActorWatcher, "git.commit", domain.RiskGit, now); c == nil || c.ID != exhausted.ID || c.UsesRemaining != 0 {
		t.Fatalf("setup: g_used should consume to 0 uses, got %+v", c)
	}

	for _, g := range []domain.AutomationGrantRecord{expired, exhausted} {
		found, didRevoke, err := s.RevokeGrant(g.ID, now)
		if err != nil {
			t.Fatalf("%s: %v", g.ID, err)
		}
		if !found || didRevoke {
			t.Fatalf("%s: want found=true didRevoke=false, got found=%v didRevoke=%v", g.ID, found, didRevoke)
		}
		got, _ := s.GetGrant(g.ID)
		if got.RevokedAt != nil {
			t.Fatalf("%s: an inert grant must NOT be stamped revoked, got %v", g.ID, *got.RevokedAt)
		}
	}

	// A live grant for the same actor, so the cascade has something real to withdraw.
	mk("g_live", "wch_dead", nil)
	n, err := s.RevokeGrantsByActor("wch_dead", now)
	if err != nil {
		t.Fatal(err)
	}
	// 1, not 3: the count is model-visible as timer.cancel/watcher.cancel's
	// revokedGrants, so it must mean "authority actually withdrawn".
	if n != 1 {
		t.Fatalf("revokeGrantsByActor should count only the live grant, want 1 got %d", n)
	}
	for _, id := range []string{"g_expired", "g_used"} {
		got, _ := s.GetGrant(id)
		if got.RevokedAt != nil {
			t.Fatalf("%s: cascade must not stamp an already-inert grant", id)
		}
	}
	if got, _ := s.GetGrant("g_live"); got.RevokedAt == nil {
		t.Fatalf("g_live: cascade must revoke the live grant")
	}
}

func TestConsumeGrantExpiredDenied(t *testing.T) {
	now := int64(10_000_000)
	s := openTest(t, now)
	s.InsertGrant(domain.AutomationGrantRecord{
		ActorID: "wch_e", ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["git"]`), ExpiresAt: now + 1000, MaxUses: 3, CreatedAt: now,
	})
	// now past expiry → denied.
	if c, _ := s.ConsumeGrant("wch_e", domain.GrantActorWatcher, "git.commit", domain.RiskGit, now+2000); c != nil {
		t.Fatalf("expired grant must not consume")
	}
}

// ---- events: dedupe refresh + resolve hide ----

func TestUpsertEventDedupeRefreshAndResolveHides(t *testing.T) {
	s := openTest(t, 1)
	a, _ := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "first", Summary: "first summary", DedupeKey: "dup-1",
	})
	if a.Count != 1 {
		t.Fatalf("first count want 1 got %d", a.Count)
	}
	b, _ := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityUrgent,
		Title: "refreshed on bump", Summary: "second summary", DedupeKey: "dup-1",
	})
	if b.ID != a.ID || b.Count != 2 {
		t.Fatalf("dedupe should collapse: id %s==%s count %d", b.ID, a.ID, b.Count)
	}
	if b.Title != "refreshed on bump" || b.Summary != "second summary" || b.Severity != domain.SeverityUrgent {
		t.Fatalf("bump must refresh title/summary/severity, got %+v", b)
	}
	if open, _ := s.ListEvents(domain.QueueDigestOptions{}); len(open) != 1 {
		t.Fatalf("dedupe leaves one row, got %d", len(open))
	}

	// resolveEvent hides from default digest, visible with includeResolved.
	ok, _ := s.ResolveEvent(b.ID)
	if !ok {
		t.Fatalf("resolve should report a change")
	}
	open, _ := s.ListEvents(domain.QueueDigestOptions{})
	for _, e := range open {
		if e.ID == b.ID {
			t.Fatalf("resolved event must be hidden by default")
		}
	}
	all, _ := s.ListEvents(domain.QueueDigestOptions{IncludeResolved: true})
	var found bool
	for _, e := range all {
		if e.ID == b.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("resolved event must be visible with includeResolved")
	}
}

// ---- workflow runs: round-trip + NULL→nil + immutable cols + list filter/order ----

func TestWorkflowRunRoundTripAndDefaults(t *testing.T) {
	idRe := regexp.MustCompile(`^wfr_[0-9a-f]{8}$`)
	s := openTest(t, 5000)
	rec, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{IssueNumber: ptrInt(25)})
	if !idRe.MatchString(rec.ID) {
		t.Fatalf("workflow id format: %s", rec.ID)
	}
	if rec.Status != domain.WorkflowPending || rec.UpdatedAt != rec.CreatedAt || rec.CompletedAt != nil {
		t.Fatalf("workflow defaults wrong: %+v", rec)
	}

	full, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{
		IssueNumber: ptrInt(25), PRNumber: ptrInt(99),
		TerminalIdsJson: strPtr(`["term_1","term_2"]`), WatcherIdsJson: strPtr(`["wch_1"]`),
		QueueEventIdsJson: strPtr(`["evt_1"]`), Status: domain.WorkflowActive,
		NextActionJson: strPtr(`{"label":"Open the PR","toolName":"workflow.update"}`),
		NotesJson:      strPtr(`["seeded from issue body"]`),
	})
	got, _ := s.GetWorkflowRun(full.ID)
	if got.IssueNumber == nil || *got.IssueNumber != 25 || got.PRNumber == nil || *got.PRNumber != 99 {
		t.Fatalf("int columns not round-tripped: %+v", got)
	}
	var terms []string
	json.Unmarshal([]byte(*got.TerminalIdsJson), &terms)
	if len(terms) != 2 {
		t.Fatalf("terminalIdsJson round-trip wrong: %v", terms)
	}

	// unknown id → nil; NULL columns → nil (not empty pointers).
	if g, _ := s.GetWorkflowRun("wfr_missing"); g != nil {
		t.Fatalf("unknown id should be nil")
	}
	bare, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{})
	gb, _ := s.GetWorkflowRun(bare.ID)
	if gb.IssueNumber != nil || gb.TerminalIdsJson != nil || gb.NextActionJson != nil || gb.CompletedAt != nil {
		t.Fatalf("NULL columns must map to nil: %+v", gb)
	}
}

func TestWorkflowUpdateImmutableAndListFilter(t *testing.T) {
	s := openTest(t, 1)
	rec, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{IssueNumber: ptrInt(7), CreatedAt: 1000})
	// id/createdAt are immutable; only allowlisted columns apply.
	s.now = func() int64 { return 2000 }
	if err := s.UpdateWorkflowRun(rec.ID, map[string]any{
		"id": "wfr_hacked", "createdAt": int64(5), "status": "active", "prNumber": 42,
		"terminalIdsJson": `["term_9"]`, "bogus": "nope",
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetWorkflowRun(rec.ID)
	if got.ID != rec.ID || got.CreatedAt != 1000 {
		t.Fatalf("id/createdAt must be immutable: %+v", got)
	}
	if got.Status != domain.WorkflowActive || got.PRNumber == nil || *got.PRNumber != 42 {
		t.Fatalf("allowed columns not applied: %+v", got)
	}
	if got.UpdatedAt != 2000 {
		t.Fatalf("updatedAt should advance to store clock, got %d", got.UpdatedAt)
	}

	// list filter + updatedAt DESC order (rec was bumped to updatedAt=2000 above,
	// so it leads; b (300) precedes a (100)).
	a, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive, CreatedAt: 100, UpdatedAt: 100})
	b, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive, CreatedAt: 300, UpdatedAt: 300})
	active, _ := s.ListWorkflowRuns("active")
	if len(active) != 3 {
		t.Fatalf("active filter want 3 rows, got %d", len(active))
	}
	pos := map[string]int{}
	for i, r := range active {
		pos[r.ID] = i
		if r.Status != domain.WorkflowActive {
			t.Fatalf("non-active row leaked into filter: %s", r.Status)
		}
	}
	if pos[rec.ID] != 0 || pos[b.ID] >= pos[a.ID] {
		t.Fatalf("updatedAt DESC order wrong: %v", pos)
	}
}

// ListNonTerminalWorkflowRuns filters to pending|active|blocked IN SQL, orders
// updatedAt DESC, bounds by limit, and treats limit<=0 as "want nothing".
func TestListNonTerminalWorkflowRuns(t *testing.T) {
	s := openTest(t, 1)
	// One row per status; updatedAt encodes the expected DESC order among the
	// non-terminal three (blocked=300 > active=200 > pending=100).
	mk := func(st domain.WorkflowRunStatus, updated int64) domain.WorkflowRunRecord {
		r, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: st, CreatedAt: updated, UpdatedAt: updated})
		return r
	}
	pending := mk(domain.WorkflowPending, 100)
	active := mk(domain.WorkflowActive, 200)
	blocked := mk(domain.WorkflowBlocked, 300)
	mk(domain.WorkflowDone, 400)      // terminal: must be excluded
	mk(domain.WorkflowCancelled, 500) // terminal: must be excluded
	mk(domain.WorkflowFailed, 600)    // terminal: must be excluded

	got, err := s.ListNonTerminalWorkflowRuns(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want 3 non-terminal rows, got %d (%+v)", len(got), got)
	}
	// updatedAt DESC: blocked, active, pending. Terminal rows never appear.
	if got[0].ID != blocked.ID || got[1].ID != active.ID || got[2].ID != pending.ID {
		t.Fatalf("updatedAt DESC order wrong: %s,%s,%s", got[0].ID, got[1].ID, got[2].ID)
	}
	for _, r := range got {
		switch r.Status {
		case domain.WorkflowPending, domain.WorkflowActive, domain.WorkflowBlocked:
		default:
			t.Fatalf("terminal status leaked into result: %s", r.Status)
		}
	}

	// limit caps the row count, keeping the most-recently-updated.
	capped, err := s.ListNonTerminalWorkflowRuns(2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped) != 2 || capped[0].ID != blocked.ID || capped[1].ID != active.ID {
		t.Fatalf("limit=2 should yield the 2 newest non-terminal runs, got %+v", capped)
	}

	// limit<=0 is "want nothing" — no query, nil result, no error.
	if rows, err := s.ListNonTerminalWorkflowRuns(0); err != nil || rows != nil {
		t.Fatalf("limit=0 must return (nil,nil); got %v,%v", rows, err)
	}
	if rows, err := s.ListNonTerminalWorkflowRuns(-5); err != nil || rows != nil {
		t.Fatalf("negative limit must return (nil,nil); got %v,%v", rows, err)
	}
}

// A run transitioned to a terminal status drops out of the non-terminal listing — the
// filter tracks the CURRENT status, not the status at insert.
func TestListNonTerminalWorkflowRuns_ExcludesRunUpdatedToTerminal(t *testing.T) {
	s := openTest(t, 1)
	rec, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive, CreatedAt: 100, UpdatedAt: 100})
	if got, _ := s.ListNonTerminalWorkflowRuns(10); len(got) != 1 {
		t.Fatalf("active run should be listed, got %d", len(got))
	}
	if err := s.UpdateWorkflowRun(rec.ID, map[string]any{"status": string(domain.WorkflowDone)}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ListNonTerminalWorkflowRuns(10); len(got) != 0 {
		t.Fatalf("run transitioned to done must be excluded, got %d", len(got))
	}
}

// With more tied-updatedAt rows than the limit, the cap is deterministic (id DESC
// tiebreaker) — two identical queries return the same rows in the same order.
func TestListNonTerminalWorkflowRuns_DeterministicTieBreak(t *testing.T) {
	s := openTest(t, 1)
	for i := 0; i < 12; i++ {
		if _, err := s.InsertWorkflowRun(domain.WorkflowRunRecord{
			Status: domain.WorkflowActive, CreatedAt: 500, UpdatedAt: 500,
		}); err != nil {
			t.Fatal(err)
		}
	}
	first, _ := s.ListNonTerminalWorkflowRuns(10)
	second, _ := s.ListNonTerminalWorkflowRuns(10)
	if len(first) != 10 || len(second) != 10 {
		t.Fatalf("want 10 rows each (capped), got %d and %d", len(first), len(second))
	}
	for i := range first {
		if first[i].ID != second[i].ID {
			t.Fatalf("tied updatedAt must yield a deterministic order: pos %d %s vs %s", i, first[i].ID, second[i].ID)
		}
	}
}

// ---- agent launches: defaults + findActive excludes terminal + most-recent ----

func TestAgentLaunchDefaultsAndFindActive(t *testing.T) {
	idRe := regexp.MustCompile(`^agt_[0-9a-f]{8}$`)
	s := openTest(t, 7000)
	rec, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k1", AgentID: "claude", Mode: "edit", Title: "Fix", Name: "Claude: Fix",
	})
	if !idRe.MatchString(rec.ID) || rec.Stage != domain.LaunchRequested || rec.UpdatedAt != rec.CreatedAt || rec.TerminalID != nil {
		t.Fatalf("agent launch defaults wrong: %+v", rec)
	}

	// findActive excludes terminal stages (confirmed/failed).
	s.UpdateAgentLaunch(rec.ID, map[string]any{"stage": "ambiguous"})
	if a, _ := s.FindActiveAgentLaunch("k1"); a == nil || a.ID != rec.ID {
		t.Fatalf("ambiguous saga should be active")
	}
	s.UpdateAgentLaunch(rec.ID, map[string]any{"stage": "confirmed"})
	if a, _ := s.FindActiveAgentLaunch("k1"); a != nil {
		t.Fatalf("confirmed saga is terminal — not active")
	}

	// most-recently-touched among same key.
	s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "same", AgentID: "c", Mode: "edit", Title: "t", Name: "n", CreatedAt: 100, UpdatedAt: 100,
	})
	newer, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "same", AgentID: "c", Mode: "edit", Title: "t", Name: "n", CreatedAt: 200, UpdatedAt: 200,
	})
	if a, _ := s.FindActiveAgentLaunch("same"); a == nil || a.ID != newer.ID {
		t.Fatalf("findActive should return most-recently-touched, got %v", a)
	}
}

// ---- agent launches: list newest-first + limit + default-clamp + empty ----

func TestListAgentLaunches(t *testing.T) {
	s := openTest(t, 5000)
	// Empty store returns an empty (non-nil-error) result, never an error.
	if got, err := s.ListAgentLaunches(5); err != nil || len(got) != 0 {
		t.Fatalf("empty list want 0 rows no error, got %d rows, err %v", len(got), err)
	}
	// Seed three sagas with distinct updatedAt so DESC order is unambiguous and
	// independent of insertion order (200 inserted last but is the middle row).
	// k2 is created OLDEST (createdAt 1) yet updated NEWEST (300) — so an accidental
	// ORDER BY createdAt regression would reorder it and fail the assertion.
	seed := []struct {
		key         string
		created, up int64
	}{{"k1", 100, 100}, {"k2", 1, 300}, {"k3", 200, 200}}
	for _, sd := range seed {
		if _, err := s.InsertAgentLaunch(domain.AgentLaunchRecord{
			IdempotencyKey: sd.key, AgentID: "c", Mode: "edit", Title: sd.key, Name: "n",
			CreatedAt: sd.created, UpdatedAt: sd.up,
		}); err != nil {
			t.Fatal(err)
		}
	}
	// Newest-first by updatedAt: 300, 200, 100.
	all, err := s.ListAgentLaunches(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 rows, got %d", len(all))
	}
	if all[0].UpdatedAt != 300 || all[1].UpdatedAt != 200 || all[2].UpdatedAt != 100 {
		t.Fatalf("want updatedAt DESC 300,200,100; got %d,%d,%d",
			all[0].UpdatedAt, all[1].UpdatedAt, all[2].UpdatedAt)
	}
	// limit honored: only the two newest.
	top, _ := s.ListAgentLaunches(2)
	if len(top) != 2 || top[0].UpdatedAt != 300 || top[1].UpdatedAt != 200 {
		t.Fatalf("limit 2 should return the newest two, got %+v", top)
	}
	// limit <= 0 clamps to the default (20), returning all three here.
	def, _ := s.ListAgentLaunches(0)
	if len(def) != 3 {
		t.Fatalf("limit<=0 should clamp to the default and return all 3, got %d", len(def))
	}
}

// The default clamp (limit<=0 ⇒ 20) must actually TRUNCATE, not just floor a small
// store. Seed 25 rows and prove the unbounded-looking call caps at 20.
func TestListAgentLaunchesDefaultCapTruncates(t *testing.T) {
	s := openTest(t, 4000)
	for i := 0; i < 25; i++ {
		if _, err := s.InsertAgentLaunch(domain.AgentLaunchRecord{
			IdempotencyKey: "k" + strconv.Itoa(i), AgentID: "c", Mode: "edit",
			Title: "t", Name: "n", CreatedAt: int64(i), UpdatedAt: int64(i),
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.ListAgentLaunches(0); len(got) != 20 {
		t.Fatalf("limit<=0 must clamp to 20, got %d", len(got))
	}
	if got, _ := s.ListAgentLaunches(5); len(got) != 5 {
		t.Fatalf("explicit limit 5 must cap at 5, got %d", len(got))
	}
}

// ---- agent launches: boot-reconcile candidates (confirmed + bound terminal) ----

func TestListConfirmedAgentLaunchesWithTerminal(t *testing.T) {
	s := openTest(t, 6000)
	// Empty store → no rows, no error.
	if got, err := s.ListConfirmedAgentLaunchesWithTerminal(10); err != nil || len(got) != 0 {
		t.Fatalf("empty want 0 rows no error, got %d rows, err %v", len(got), err)
	}
	term := func(v string) *string { return &v }
	seed := []domain.AgentLaunchRecord{
		// confirmed + terminal → INCLUDED (oldest update).
		{IdempotencyKey: "a", AgentID: "claude", Mode: "edit", Title: "A", Name: "n",
			Stage: domain.LaunchConfirmed, TerminalID: term("term_a"), CreatedAt: 100, UpdatedAt: 100},
		// confirmed but NO terminal → excluded.
		{IdempotencyKey: "b", AgentID: "claude", Mode: "edit", Title: "B", Name: "n",
			Stage: domain.LaunchConfirmed, CreatedAt: 200, UpdatedAt: 200},
		// failed + terminal → excluded (terminal stage, but not confirmed).
		{IdempotencyKey: "c", AgentID: "claude", Mode: "edit", Title: "C", Name: "n",
			Stage: domain.LaunchFailed, TerminalID: term("term_c"), CreatedAt: 300, UpdatedAt: 300},
		// confirmed + terminal → INCLUDED (newest update — must lead).
		{IdempotencyKey: "d", AgentID: "claude", Mode: "edit", Title: "D", Name: "n",
			Stage: domain.LaunchConfirmed, TerminalID: term("term_d"), CreatedAt: 400, UpdatedAt: 400},
	}
	for _, r := range seed {
		if _, err := s.InsertAgentLaunch(r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListConfirmedAgentLaunchesWithTerminal(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 confirmed-with-terminal rows, got %d: %+v", len(got), got)
	}
	// newest-first by updatedAt: term_d (400) before term_a (100).
	if got[0].TerminalID == nil || *got[0].TerminalID != "term_d" ||
		got[1].TerminalID == nil || *got[1].TerminalID != "term_a" {
		t.Fatalf("want [term_d, term_a] newest-first, got %+v", got)
	}
	// limit honored: only the newest.
	top, _ := s.ListConfirmedAgentLaunchesWithTerminal(1)
	if len(top) != 1 || top[0].TerminalID == nil || *top[0].TerminalID != "term_d" {
		t.Fatalf("limit 1 should return only term_d, got %+v", top)
	}
}

// ---- skill run state: unique key throws + list order + immutable cols ----

func TestSkillRunStateUniqueAndImmutable(t *testing.T) {
	idRe := regexp.MustCompile(`^rrs_[0-9a-f]{8}$`)
	s := openTest(t, 9000)
	rec, _ := s.InsertSkillRunState(domain.SkillRunStateRecord{SessionID: "ses_a", SkillID: "r.one"})
	if !idRe.MatchString(rec.ID) || rec.Status != domain.SkillRunActive || rec.CurrentStep != 0 || rec.StepsJson != "[]" {
		t.Fatalf("skill run defaults wrong: %+v", rec)
	}
	// natural-key uniqueness: duplicate insert errors.
	if _, err := s.InsertSkillRunState(domain.SkillRunStateRecord{SessionID: "ses_a", SkillID: "r.one"}); err == nil {
		t.Fatalf("duplicate (session,skill) insert must error")
	}

	// immutable session/skill/startedAt; updatedAt force-advances.
	rec2, _ := s.InsertSkillRunState(domain.SkillRunStateRecord{SessionID: "ses_a", SkillID: "r.imm", StartedAt: 1000})
	s.now = func() int64 { return 2000 }
	s.UpdateSkillRunState(rec2.ID, map[string]any{
		"sessionId": "ses_other", "skillId": "r.other", "startedAt": int64(5),
		"currentStep": 2, "bogus": "nope",
	})
	got, _ := s.GetSkillRunState("ses_a", "r.imm")
	if got == nil || got.SessionID != "ses_a" || got.SkillID != "r.imm" || got.StartedAt != 1000 {
		t.Fatalf("session/skill/startedAt must be immutable: %+v", got)
	}
	if got.CurrentStep != 2 || got.UpdatedAt != 2000 {
		t.Fatalf("allowed columns / forced updatedAt wrong: %+v", got)
	}

	// list filters by session, updatedAt DESC.
	s.now = func() int64 { return 100 }
	s.InsertSkillRunState(domain.SkillRunStateRecord{SessionID: "ses_b", SkillID: "x.1", StartedAt: 100, UpdatedAt: 100})
	if got, _ := s.ListSkillRunStates("ses_a"); len(got) != 2 {
		t.Fatalf("ses_a should have 2 states, got %d", len(got))
	}
	if all, _ := s.ListSkillRunStates(""); len(all) != 3 {
		t.Fatalf("unscoped list want 3, got %d", len(all))
	}
}

// ---- audit runId stamping + absent→nil ----

func TestAuditRunIDRoundTrip(t *testing.T) {
	s := openTest(t, 1)
	with, _ := s.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}", Outcome: "ok",
		DurationMs: 5, Summary: "ok", RunID: strPtr("run_abc123"),
	})
	if with.RunID == nil || *with.RunID != "run_abc123" {
		t.Fatalf("runId not stamped")
	}
	rows, _ := s.ListAudit(50)
	var seen *string
	for _, r := range rows {
		if r.ID == with.ID {
			seen = r.RunID
		}
	}
	if seen == nil || *seen != "run_abc123" {
		t.Fatalf("runId not round-tripped through listAudit")
	}

	without, _ := s.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorTimer, ToolName: "fs.read", ArgsJson: "{}", Outcome: "ok",
		DurationMs: 5, Summary: "ok",
	})
	rows, _ = s.ListAudit(50)
	for _, r := range rows {
		if r.ID == without.ID && r.RunID != nil {
			t.Fatalf("absent runId must map to nil, got %v", r.RunID)
		}
	}
}

// The workflowRunId back-link round-trips on both the watcher and agent-launch
// records, defaults to nil when absent, and is settable through the allowlisted
// update (issue #206 — the durable-ledger link).
func TestWatcherAndAgentLaunchWorkflowRunIDRoundTrip(t *testing.T) {
	s := openTest(t, 1)

	// Watcher: insert carrying a back-link → read it back.
	w, _ := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w", Goal: "g", TargetsJson: "[]", CadenceMs: 1000,
		ModelTier: domain.ModelSmall, NextCheckAt: 1, WorkflowRunID: strPtr("wfr_1"),
	})
	if gw, _ := s.GetWatcher(w.ID); gw.WorkflowRunID == nil || *gw.WorkflowRunID != "wfr_1" {
		t.Fatalf("watcher workflowRunId not round-tripped: %v", gw.WorkflowRunID)
	}
	// Absent → nil; settable via the allowlisted update.
	bare, _ := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w2", Goal: "g", TargetsJson: "[]", CadenceMs: 1000,
		ModelTier: domain.ModelSmall, NextCheckAt: 1,
	})
	if gb, _ := s.GetWatcher(bare.ID); gb.WorkflowRunID != nil {
		t.Fatalf("absent watcher workflowRunId must be nil, got %v", gb.WorkflowRunID)
	}
	if err := s.UpdateWatcher(bare.ID, map[string]any{"workflowRunId": "wfr_2"}); err != nil {
		t.Fatal(err)
	}
	if gb, _ := s.GetWatcher(bare.ID); gb.WorkflowRunID == nil || *gb.WorkflowRunID != "wfr_2" {
		t.Fatalf("watcher workflowRunId update not applied: %v", gb.WorkflowRunID)
	}

	// Agent launch: same round-trip + allowlisted update.
	a, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "k", AgentID: "claude", Mode: "edit", Title: "t", Name: "n",
		WorkflowRunID: strPtr("wfr_3"),
	})
	if ga, _ := s.GetAgentLaunch(a.ID); ga.WorkflowRunID == nil || *ga.WorkflowRunID != "wfr_3" {
		t.Fatalf("agent launch workflowRunId not round-tripped: %v", ga.WorkflowRunID)
	}
	if err := s.UpdateAgentLaunch(a.ID, map[string]any{"workflowRunId": "wfr_4"}); err != nil {
		t.Fatal(err)
	}
	if ga, _ := s.GetAgentLaunch(a.ID); ga.WorkflowRunID == nil || *ga.WorkflowRunID != "wfr_4" {
		t.Fatalf("agent launch workflowRunId update not applied: %v", ga.WorkflowRunID)
	}
}
