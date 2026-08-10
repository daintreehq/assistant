package storage

import (
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// openTest opens an in-memory store with a frozen clock at `now`.
func openTest(t *testing.T, now int64) *Store {
	t.Helper()
	s, err := Open(":memory:", &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMemoryInsertRecallFTS(t *testing.T) {
	s := openTest(t, 1000)
	if _, err := s.InsertMemory(domain.MemoryRecord{Content: "the deploy pipeline uses DeepSeek tokens"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.InsertMemory(domain.MemoryRecord{Content: "watcher cadence floors at the scheduler tick"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.RecallMemories("deploy pipeline", MemoryRecallOptions{})
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 hit, got %d", len(got))
	}
	if got[0].Content != "the deploy pipeline uses DeepSeek tokens" {
		t.Fatalf("wrong memory: %q", got[0].Content)
	}
	// Implicit AND: a token in neither indexed doc set should yield no rows.
	none, err := s.RecallMemories("deploy nonexistentword", MemoryRecallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Fatalf("implicit-AND should exclude, got %d", len(none))
	}
}

func TestMemoryExists(t *testing.T) {
	s := openTest(t, 1000)
	m, err := s.InsertMemory(domain.MemoryRecord{Content: "exact content match target"})
	if err != nil {
		t.Fatal(err)
	}
	// Present (exact match).
	if ok, err := s.MemoryExists("exact content match target"); err != nil || !ok {
		t.Fatalf("exact match should exist: ok=%v err=%v", ok, err)
	}
	// Absent (different content) + near-match must NOT count (exact only).
	if ok, _ := s.MemoryExists("exact content match"); ok {
		t.Fatal("substring must not be treated as existing")
	}
	if ok, _ := s.MemoryExists("totally different"); ok {
		t.Fatal("absent content reported as existing")
	}
	// Soft-deleted content STILL counts as existing, so distillation does not resurrect
	// a memory the user explicitly forgot.
	if _, err := s.ForgetMemory(m.ID, 2000); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.MemoryExists("exact content match target"); err != nil || !ok {
		t.Fatalf("soft-deleted memory must still count as existing (no resurrection): ok=%v err=%v", ok, err)
	}
}

func TestRecallEmptyAndEscaping(t *testing.T) {
	s := openTest(t, 1000)
	if _, err := s.InsertMemory(domain.MemoryRecord{Content: `quote " and AND operators`}); err != nil {
		t.Fatal(err)
	}
	// Blank query short-circuits.
	if got, _ := s.RecallMemories("   ", MemoryRecallOptions{}); got != nil {
		t.Fatalf("blank query should be empty")
	}
	// A bare double-quote would be a MATCH syntax error if not escaped.
	got, err := s.RecallMemories(`"`, MemoryRecallOptions{})
	if err != nil {
		t.Fatalf("escaped quote must not error: %v", err)
	}
	_ = got
	// "AND" as a token must be quoted (else it's an FTS operator) — search works.
	hits, err := s.RecallMemories("operators", MemoryRecallOptions{})
	if err != nil || len(hits) != 1 {
		t.Fatalf("want 1 hit, got %d err %v", len(hits), err)
	}
}

func TestForgetThenSweepEvictsFTS(t *testing.T) {
	// Use a realistic large epoch so deletedAt stays positive (the now<=0 sentinel
	// only fires for non-positive clocks, which never occur in real epoch-ms use).
	now := int64(1_700_000_000_000)
	s := openTest(t, now)
	m, _ := s.InsertMemory(domain.MemoryRecord{Content: "secret recall token alpha"})
	// soft-delete in the distant past so the sweep window has elapsed.
	if _, err := s.ForgetMemory(m.ID, now-int64(40*dayMS)); err != nil {
		t.Fatal(err)
	}
	// recall must now exclude it (deletedAt filter), even though still FTS-indexed.
	if got, _ := s.RecallMemories("alpha", MemoryRecallOptions{}); len(got) != 0 {
		t.Fatalf("soft-deleted memory must not recall")
	}
	// run the sweep: hard-delete past the undo window → trigger evicts FTS cleanly.
	if err := s.GCRetentionSweep(now); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got, _ := s.GetMemory(m.ID, true); got != nil {
		t.Fatalf("memory should be hard-deleted")
	}
	// FTS index is not corrupted: a fresh insert + recall still works.
	if _, err := s.InsertMemory(domain.MemoryRecord{Content: "beta recall token"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.RecallMemories("beta", MemoryRecallOptions{}); err != nil || len(got) != 1 {
		t.Fatalf("post-sweep recall broken: %d %v", len(got), err)
	}
}

func TestEventDedupeUpsert(t *testing.T) {
	now := int64(5000)
	s, _ := Open(":memory:", &Options{Now: func() int64 { return now }})
	defer s.Close()
	a, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityInfo,
		Title: "t1", Summary: "s1", DedupeKey: "k", Evidence: []string{"e1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := a.CreatedAt
	now = 6000 // advance clock
	b, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityUrgent,
		Title: "t2", Summary: "s2", DedupeKey: "k", // evidence omitted → falls back
	})
	if err != nil {
		t.Fatal(err)
	}
	if b.ID != a.ID {
		t.Fatalf("dedupe should collapse into same row")
	}
	if b.Count != 2 {
		t.Fatalf("count want 2 got %d", b.Count)
	}
	if b.CreatedAt != createdAt {
		t.Fatalf("createdAt must not change on dedupe")
	}
	if b.UpdatedAt == nil || *b.UpdatedAt != 6000 {
		t.Fatalf("updatedAt must bump to 6000, got %v", b.UpdatedAt)
	}
	if b.Severity != domain.SeverityUrgent || b.Title != "t2" {
		t.Fatalf("refresh fields wrong: %v %v", b.Severity, b.Title)
	}
	if len(b.Evidence) != 1 || b.Evidence[0] != "e1" {
		t.Fatalf("evidence should fall back to existing, got %v", b.Evidence)
	}
}

// TestMarkNotifiedIsVersionConditional proves the acknowledgement SQL: a stale
// snapshot (the row was bumped after the digest read) must NOT stamp notifiedAt
// — the update stays digestable — while the fresh snapshot stamps it normally.
func TestMarkNotifiedIsVersionConditional(t *testing.T) {
	now := int64(5000)
	s, _ := Open(":memory:", &Options{Now: func() int64 { return now }})
	defer s.Close()
	stale, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "w: waiting", Summary: "v1", DedupeKey: "k",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Publisher bump lands AFTER the notifier digested `stale` (count+1, updatedAt
	// advanced) — the classic digest/ack race.
	now = 6000
	if _, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "w: tests failed", Summary: "v2", DedupeKey: "k",
	}); err != nil {
		t.Fatal(err)
	}

	// The stale acknowledgement must not stick.
	if err := s.MarkNotified([]domain.QueueEvent{stale}, 0); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.ListEvents(domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].ID != stale.ID || fresh[0].Summary != "v2" {
		t.Fatalf("bumped event must stay digestable after a stale ack, got %+v", fresh)
	}

	// The fresh snapshot acknowledges normally.
	if err := s.MarkNotified(fresh, 0); err != nil {
		t.Fatal(err)
	}
	left, _ := s.ListEvents(domain.QueueDigestOptions{NotifiedIsNull: true})
	if len(left) != 0 {
		t.Fatalf("fresh ack must stamp notifiedAt, %d events still digestable", len(left))
	}
}

// TestClearNotifiedRearmsForNextDigest is the store half of the cancelled-wake
// recovery: a burst handed to a dying process was already stamped notified, so
// the host re-arms it via ClearNotified — after which the next run's notify-pass
// digest (NotifiedIsNull) returns the events again for re-delivery.
func TestClearNotifiedRearmsForNextDigest(t *testing.T) {
	s := openTest(t, 5000)
	ev, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "agent finished", Summary: "terminal settled",
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scheduler delivered the burst to the (soon-cancelled) wake and stamped it.
	if err := s.MarkNotified([]domain.QueueEvent{ev}, 0); err != nil {
		t.Fatal(err)
	}
	if fresh, _ := s.ListEvents(domain.QueueDigestOptions{NotifiedIsNull: true}); len(fresh) != 0 {
		t.Fatalf("sanity: event should be suppressed after MarkNotified, got %d", len(fresh))
	}
	// Shutdown cancelled the wake: the host durably re-arms the burst.
	if err := s.ClearNotified([]string{ev.ID}); err != nil {
		t.Fatal(err)
	}
	// The next run's scheduler pass re-digests it.
	fresh, err := s.ListEvents(domain.QueueDigestOptions{NotifiedIsNull: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 1 || fresh[0].ID != ev.ID {
		t.Fatalf("re-armed event must be digestable again, got %+v", fresh)
	}
}

func TestListEventsSeverityOrder(t *testing.T) {
	s := openTest(t, 1)
	for _, sv := range []domain.Severity{domain.SeverityDone, domain.SeverityError, domain.SeverityInfo, domain.SeverityAttention} {
		if _, err := s.UpsertEvent(domain.QueuePublishArgs{
			Source: domain.SourceSystem, Severity: sv, Title: string(sv), Summary: "x",
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListEvents(domain.QueueDigestOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// error(6) > attention(3) > done(2) > info(1) — done ranks below attention.
	want := []domain.Severity{domain.SeverityError, domain.SeverityAttention, domain.SeverityDone, domain.SeverityInfo}
	if len(got) != len(want) {
		t.Fatalf("want %d events got %d", len(want), len(got))
	}
	for i := range want {
		if got[i].Severity != want[i] {
			t.Fatalf("pos %d want %s got %s", i, want[i], got[i].Severity)
		}
	}
	// severityAtLeast filter (attention) drops info & done.
	at := domain.SeverityAttention
	filtered, _ := s.ListEvents(domain.QueueDigestOptions{SeverityAtLeast: &at})
	if len(filtered) != 2 {
		t.Fatalf("severityAtLeast attention want 2 got %d", len(filtered))
	}
}

func TestConsumeGrantAtomicUnion(t *testing.T) {
	now := int64(1000)
	s := openTest(t, now)
	names := `["terminal.send"]`
	g, err := s.InsertGrant(domain.AutomationGrantRecord{
		ActorID: "wch_a", ActorType: domain.GrantActorWatcher,
		AllowedToolNamesJson: &names, ExpiresAt: now + 100000, MaxUses: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if g.UsesRemaining != 2 {
		t.Fatalf("usesRemaining default = maxUses, got %d", g.UsesRemaining)
	}
	// tool-name union match consumes one.
	c1, err := s.ConsumeGrant("wch_a", domain.GrantActorWatcher, "terminal.send", domain.RiskTerminal, now)
	if err != nil || c1 == nil {
		t.Fatalf("consume1: %v %v", c1, err)
	}
	if c1.UsesRemaining != 1 {
		t.Fatalf("after consume want 1 got %d", c1.UsesRemaining)
	}
	// wrong actorType must not authorize.
	if c, _ := s.ConsumeGrant("wch_a", domain.GrantActorTimer, "terminal.send", domain.RiskTerminal, now); c != nil {
		t.Fatalf("actorType mismatch must not consume")
	}
	// non-matching tool & risk → no consume.
	if c, _ := s.ConsumeGrant("wch_a", domain.GrantActorWatcher, "other.tool", domain.RiskRead, now); c != nil {
		t.Fatalf("non-matching must not consume")
	}
	// second valid consume exhausts; revokedAt stays nil (exhaustion != revoke).
	c2, _ := s.ConsumeGrant("wch_a", domain.GrantActorWatcher, "terminal.send", domain.RiskTerminal, now)
	if c2 == nil || c2.UsesRemaining != 0 {
		t.Fatalf("second consume want 0 remaining")
	}
	if c2.RevokedAt != nil {
		t.Fatalf("exhaustion must NOT stamp revokedAt")
	}
	// exhausted grant no longer consumable.
	if c, _ := s.ConsumeGrant("wch_a", domain.GrantActorWatcher, "terminal.send", domain.RiskTerminal, now); c != nil {
		t.Fatalf("exhausted grant must not consume")
	}
}

func TestGrantRiskClassUnion(t *testing.T) {
	now := int64(1000)
	s := openTest(t, now)
	risks := `["terminal"]`
	if _, err := s.InsertGrant(domain.AutomationGrantRecord{
		ActorID: "tmr_x", ActorType: domain.GrantActorTimer,
		AllowedRiskClassesJson: &risks, ExpiresAt: now + 1000, MaxUses: 1,
	}); err != nil {
		t.Fatal(err)
	}
	// match by risk class even though tool name not listed.
	c, err := s.ConsumeGrant("tmr_x", domain.GrantActorTimer, "anything", domain.RiskTerminal, now)
	if err != nil || c == nil {
		t.Fatalf("risk-class union should authorize: %v %v", c, err)
	}
}

func TestCancelLiveWatchersRevokesGrantsAndResolvesEvents(t *testing.T) {
	// Seed: an active watcher + its grant + an open watcher event. This is the
	// /clear teardown (the only wholesale watcher sweep left now that watchers
	// are project-scoped and adopted across process boundaries).
	now := int64(2000)
	s1, err := Open(":memory:", &Options{Now: func() int64 { return now }})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := s1.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w", Goal: "g", TargetsJson: `["t1"]`,
		CadenceMs: 3000, ModelTier: domain.ModelSmall, NextCheckAt: now,
	})
	if _, err := s1.InsertGrant(domain.AutomationGrantRecord{
		ActorID: w.ID, ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: now + 100000, MaxUses: 5,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityInfo, Title: "x", Summary: "y",
	}); err != nil {
		t.Fatal(err)
	}
	// A timer-sourced event must survive the sweep (scoped to watcher sources).
	if _, err := s1.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTimer, Severity: domain.SeverityInfo, Title: "keep", Summary: "z",
	}); err != nil {
		t.Fatal(err)
	}

	endedTitles, err := s1.CancelLiveWatchers(now, ReasonSessionCleared)
	if err != nil {
		t.Fatal(err)
	}
	// It returns the cancelled watcher's title for the caller's confirmation card.
	if len(endedTitles) != 1 || endedTitles[0] != "w" {
		t.Fatalf("CancelLiveWatchers titles want [w], got %v", endedTitles)
	}
	gw, _ := s1.GetWatcher(w.ID)
	if gw == nil || gw.Status != "cancelled" {
		t.Fatalf("watcher should be cancelled, got %v", gw)
	}
	// And stamps the caller's reason + time (distinct from a user cancel).
	if gw.EndedReason == nil || *gw.EndedReason != ReasonSessionCleared {
		t.Fatalf("want endedReason %q, got %v", ReasonSessionCleared, gw.EndedReason)
	}
	if gw.EndedAt == nil || *gw.EndedAt != now {
		t.Fatalf("want endedAt %d, got %v", now, gw.EndedAt)
	}
	live, _ := s1.ListGrants(w.ID, now)
	if len(live) != 0 {
		t.Fatalf("watcher grants should be revoked, got %d live", len(live))
	}
	open, _ := s1.ListEvents(domain.QueueDigestOptions{})
	for _, e := range open {
		if e.Source == domain.SourceTerminalWatcher {
			t.Fatalf("watcher event should be resolved")
		}
	}
	// timer event survives.
	var keptTimer bool
	for _, e := range open {
		if e.Source == domain.SourceTimer {
			keptTimer = true
		}
	}
	if !keptTimer {
		t.Fatalf("timer event must persist across the watcher sweep")
	}
	_ = s1.Close()
}

func TestResetStaleAgentLaunches(t *testing.T) {
	now := int64(3000)
	s := openTest(t, now)
	term := "terminal-9"
	// An in-flight saga (no terminal) is dead the moment the session ends → deleted.
	inflight, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "key1", AgentID: "ag", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.TerminalBound,
	})
	// A terminal-but-dead saga (failed) is pure history → deleted.
	failed, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "key2", AgentID: "ag", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchFailed,
	})
	// A confirmed saga with NO terminal is anomalous and not re-adoptable → deleted.
	confNoTerm, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "key3", AgentID: "ag", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchConfirmed,
	})
	// A confirmed saga that bound a terminal is the ONLY survivor (orphan re-adoption).
	keep, _ := s.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "key4", AgentID: "ag", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchConfirmed, TerminalID: &term,
	})
	if err := s.resetStaleAgentLaunches(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{inflight.ID, failed.ID, confNoTerm.ID} {
		if got, _ := s.GetAgentLaunch(id); got != nil {
			t.Fatalf("dead saga %s should be deleted, got stage %s", id, got.Stage)
		}
	}
	// FindActiveAgentLaunch must not return the cleared in-flight saga.
	if found, _ := s.FindActiveAgentLaunch("key1"); found != nil {
		t.Fatalf("cleared in-flight saga must not be active")
	}
	// The confirmed-with-terminal saga survives for boot reconciliation.
	if kg, _ := s.GetAgentLaunch(keep.ID); kg == nil || kg.Stage != domain.LaunchConfirmed {
		t.Fatalf("confirmed-with-terminal saga must survive, got %v", kg)
	}
}

func TestWorkflowUpdateNoopGuardAndForcedUpdatedAt(t *testing.T) {
	now := int64(1000)
	s := openTest(t, now)
	w, _ := s.InsertWorkflowRun(domain.WorkflowRunRecord{})
	orig := w.UpdatedAt
	// An empty patch is a no-op: updatedAt must NOT bump.
	if err := s.UpdateWorkflowRun(w.ID, map[string]any{}); err != nil {
		t.Fatal(err)
	}
	g1, _ := s.GetWorkflowRun(w.ID)
	if g1.UpdatedAt != orig {
		t.Fatalf("empty patch must not bump updatedAt")
	}
	// A real change forces updatedAt = now (caller can't set it).
	s.now = func() int64 { return 9999 }
	if err := s.UpdateWorkflowRun(w.ID, map[string]any{"status": "active", "updatedAt": int64(1)}); err != nil {
		t.Fatal(err)
	}
	g2, _ := s.GetWorkflowRun(w.ID)
	if g2.Status != domain.WorkflowActive {
		t.Fatalf("status not applied")
	}
	if g2.UpdatedAt != 9999 {
		t.Fatalf("updatedAt must be store-forced to 9999, got %d", g2.UpdatedAt)
	}
}

func TestSupervisorCadenceFloor(t *testing.T) {
	s := openTest(t, 1)
	sup := true
	w, _ := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w", Goal: "g", TargetsJson: "[]",
		CadenceMs: 100, IsSupervisor: &sup, ModelTier: domain.ModelSmall, NextCheckAt: 1,
	})
	if w.CadenceMs != schedulerTickMS {
		t.Fatalf("supervisor cadence should floor to %d, got %d", schedulerTickMS, w.CadenceMs)
	}
	got, _ := s.GetWatcher(w.ID)
	if got.IsSupervisor == nil || !*got.IsSupervisor {
		t.Fatalf("isSupervisor should round-trip true")
	}
}

func TestWatcherDefaultLifetime(t *testing.T) {
	s := openTest(t, 1)
	// No stopAfterMs supplied ⇒ InsertWatcher stamps the 24h ceiling so the
	// watcher can't poll forever (the timeout check is gated on stopAfterMs != nil).
	w, err := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w", Goal: "g", TargetsJson: "[]",
		CadenceMs: 120000, ModelTier: domain.ModelSmall, NextCheckAt: 1,
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if w.StopAfterMs == nil || *w.StopAfterMs != domain.WatcherDefaultLifetimeMS {
		t.Fatalf("default stopAfterMs want %d, got %v", domain.WatcherDefaultLifetimeMS, w.StopAfterMs)
	}
	// The default must persist, not just live on the returned record.
	got, _ := s.GetWatcher(w.ID)
	if got == nil || got.StopAfterMs == nil || *got.StopAfterMs != domain.WatcherDefaultLifetimeMS {
		t.Fatalf("persisted stopAfterMs want %d, got %v", domain.WatcherDefaultLifetimeMS, got.StopAfterMs)
	}

	// An explicit stopAfterMs is preserved verbatim — the default never overrides it.
	explicit := int64(3_600_000)
	w2, err := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w2", Goal: "g", TargetsJson: "[]",
		CadenceMs: 120000, ModelTier: domain.ModelSmall, NextCheckAt: 1,
		StopAfterMs: &explicit,
	})
	if err != nil {
		t.Fatalf("insert explicit: %v", err)
	}
	if w2.StopAfterMs == nil || *w2.StopAfterMs != explicit {
		t.Fatalf("explicit stopAfterMs want %d, got %v", explicit, w2.StopAfterMs)
	}

	// A large startAfterMs is folded into the default ceiling so the watcher still gets
	// a full lifetime of watching past the start delay (timeout is measured from
	// createdAt) — it must not time out before its first check.
	startAfter := int64(90_000_000) // ~25h, > the 24h default
	w3, err := s.InsertWatcher(domain.WatcherRecord{
		Kind: "terminal", Title: "w3", Goal: "g", TargetsJson: "[]",
		CadenceMs: 120000, ModelTier: domain.ModelSmall, NextCheckAt: 1,
		StartAfterMs: &startAfter,
	})
	if err != nil {
		t.Fatalf("insert startAfter: %v", err)
	}
	wantStop := domain.WatcherDefaultLifetimeMS + startAfter
	if w3.StopAfterMs == nil || *w3.StopAfterMs != wantStop {
		t.Fatalf("startAfter default stopAfterMs want %d, got %v", wantStop, w3.StopAfterMs)
	}
	if *w3.StopAfterMs <= startAfter {
		t.Fatalf("default ceiling %d must exceed startAfterMs %d", *w3.StopAfterMs, startAfter)
	}
}

// TestSupervisorWatcherDefaultLifetime proves the storage chokepoint caps supervisor
// watchers too: BuildSupervisorWatcherRecord deliberately omits StopAfterMs, so a
// supervising watcher would otherwise poll forever. Routing it through the real
// InsertWatcher must stamp the 24h ceiling.
func TestSupervisorWatcherDefaultLifetime(t *testing.T) {
	s := openTest(t, 1)
	rec := domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
		TerminalID: "t1", Title: "watch edits", Goal: "supervise the edit agent",
		CadenceMs: 3000, SpawnMode: "edit",
	})
	if rec.StopAfterMs != nil {
		t.Fatalf("precondition: supervisor builder must leave StopAfterMs nil, got %v", rec.StopAfterMs)
	}
	w, err := s.InsertWatcher(rec)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if w.StopAfterMs == nil || *w.StopAfterMs != domain.WatcherDefaultLifetimeMS {
		t.Fatalf("supervisor default stopAfterMs want %d, got %v", domain.WatcherDefaultLifetimeMS, w.StopAfterMs)
	}
}

func TestRetentionPrunesRunWithAuditPairing(t *testing.T) {
	now := int64(1_700_000_000_000)
	// tiny windows so a single old run/audit is swept.
	ret := Retention{
		RunEventsMaxAge: time.Millisecond, RunEventsKeepRuns: 0,
		AuditLogMaxAge: time.Millisecond, AuditLogKeepRows: 0,
		ConversationMaxAge: time.Hour, ConversationKeepRows: 100,
		EventsTerminalAge: time.Hour, MemoriesDeletedAge: time.Hour,
	}
	s, err := Open(":memory:", &Options{Now: func() int64 { return now }, Retention: &ret})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	old := now - 10*dayMS
	// old run + its audit row, both keyed on runId.
	s.InsertRunEvent(domain.RunEventRecord{RunID: "run1", Seq: 0, Ts: old, Type: "assistant:start"})
	s.InsertAudit(domain.AuditRecord{Ts: old, Actor: domain.ActorMain, ToolName: "x",
		ArgsJson: "{}", Outcome: "ok", Summary: "s", RunID: strPtr("run1")})
	if err := s.GCRetentionSweep(now); err != nil {
		t.Fatal(err)
	}
	re, _ := s.ListRunEvents("run1")
	if len(re) != 0 {
		t.Fatalf("old run_events should be pruned, got %d", len(re))
	}
	au, _ := s.ListAuditByRunID("run1")
	if len(au) != 0 {
		t.Fatalf("run's audit rows should be co-deleted, got %d", len(au))
	}
}

func TestQueryAuditFilters(t *testing.T) {
	s := openTest(t, 1)
	s.InsertAudit(domain.AuditRecord{Ts: 10, Actor: domain.ActorMain, ToolName: "a", ArgsJson: "{}", Outcome: "ok", Summary: "x"})
	s.InsertAudit(domain.AuditRecord{Ts: 20, Actor: domain.ActorWatcher, ToolName: "b", ArgsJson: "{}", Outcome: "denied", Summary: "y"})
	mainActor := domain.ActorMain
	got, err := s.QueryAudit(AuditFilters{Actor: &mainActor})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ToolName != "a" {
		t.Fatalf("actor filter failed: %v", got)
	}
	// ts bounds inclusive.
	from, to := int64(15), int64(25)
	win, _ := s.QueryAudit(AuditFilters{TsFrom: &from, TsTo: &to})
	if len(win) != 1 || win[0].ToolName != "b" {
		t.Fatalf("ts window filter failed: %v", win)
	}
}

func strPtr(s string) *string { return &s }
