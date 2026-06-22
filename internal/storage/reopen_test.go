package storage

import (
	"path/filepath"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestReopenCancelsNonTerminalWatchersAndRevokesGrants exercises the full session
// boundary on a SECOND Open of the same file: non-terminal watchers (active /
// created / paused) flip to cancelled and their grants are revoked, while
// terminal-state watchers and their grants survive, and a same-actorId timer
// grant is untouched (scope-by-actorType). The sweep cancels non-terminal
// watchers from a prior session and revokes their grants on reopen.
func TestReopenCancelsNonTerminalWatchersAndRevokesGrants(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := int64(2000)
	nonTerminal := []string{"active", "created", "paused"}
	terminal := []string{"condition_met", "timeout", "cancelled", "error"}

	first := openFile(t, path, now)
	mkWatcher := func(id, status string) {
		if _, err := first.InsertWatcher(domain.WatcherRecord{
			ID: id, Kind: "terminal", Title: "w " + id, Goal: "supervise",
			TargetsJson: `["term_1"]`, CadenceMs: 5000, ModelTier: domain.ModelSmall,
			NextCheckAt: 0, Status: status,
		}); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range nonTerminal {
		mkWatcher("wch_"+s, s)
	}
	for _, s := range terminal {
		mkWatcher("wch_"+s, s)
	}
	// A live grant for each stale watcher — all must be revoked.
	for _, s := range nonTerminal {
		first.InsertGrant(domain.AutomationGrantRecord{
			ID: "grt_" + s, ActorID: "wch_" + s, ActorType: domain.GrantActorWatcher,
			AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: 9999999999999, MaxUses: 5,
		})
	}
	// A grant for a terminal-state watcher — survives (not swept).
	first.InsertGrant(domain.AutomationGrantRecord{
		ID: "grt_terminal_state", ActorID: "wch_condition_met", ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: 9999999999999, MaxUses: 5,
	})
	// A TIMER grant sharing wch_active's actorId — must NOT be revoked (actorType scope).
	first.InsertGrant(domain.AutomationGrantRecord{
		ID: "grt_timer", ActorID: "wch_active", ActorType: domain.GrantActorTimer,
		AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: 9999999999999, MaxUses: 5,
	})
	first.InsertTimer(domain.TimerRecord{ID: "tmr_keep", Title: "keep", FireAt: 9999999999999, PayloadType: "enqueue", PayloadJson: "{}"})
	_ = first.Close()

	// Reopen: construction runs cancelStaleWatchers.
	s := openFile(t, path, now)
	defer s.Close()

	for _, st := range nonTerminal {
		w, _ := s.GetWatcher("wch_" + st)
		if w == nil || w.Status != "cancelled" {
			t.Fatalf("wch_%s want cancelled, got %v", st, w)
		}
	}
	for _, st := range terminal {
		w, _ := s.GetWatcher("wch_" + st)
		if w == nil || w.Status != st {
			t.Fatalf("wch_%s should be untouched, got %v", st, w)
		}
	}
	if dw, _ := s.DueWatchers(now); len(dw) != 0 {
		t.Fatalf("no watcher should be due after sweep, got %d", len(dw))
	}
	for _, st := range nonTerminal {
		g, _ := s.GetGrant("grt_" + st)
		if g == nil || g.RevokedAt == nil {
			t.Fatalf("grt_%s should be revoked", st)
		}
	}
	if g, _ := s.GetGrant("grt_terminal_state"); g == nil || g.RevokedAt != nil {
		t.Fatalf("terminal-state grant must survive")
	}
	if g, _ := s.GetGrant("grt_timer"); g == nil || g.RevokedAt != nil {
		t.Fatalf("timer grant (same actorId) must survive")
	}
	if tm, _ := s.GetTimer("tmr_keep"); tm == nil || tm.Status != "scheduled" {
		t.Fatalf("persistent timer must survive untouched")
	}
}

// TestReopenCancelsPRStateWatcher — a pr_state watcher is session-scoped like a
// terminal one; the kind-agnostic sweep cancels it and revokes its grant.
func TestReopenCancelsPRStateWatcher(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := int64(2000)
	first := openFile(t, path, now)
	first.InsertWatcher(domain.WatcherRecord{
		ID: "wch_pr", Kind: "pr_state", Title: "PR #5", Goal: "watch pr",
		TargetsJson: `["PR #5"]`, CadenceMs: 60000, ModelTier: domain.ModelSmall,
		OptionsJson: strPtr(`{"prNumber":5,"lastState":"open"}`), NextCheckAt: 0, Status: "active",
	})
	first.InsertGrant(domain.AutomationGrantRecord{
		ID: "grt_pr", ActorID: "wch_pr", ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["read"]`), ExpiresAt: 9999999999999, MaxUses: 5,
	})
	_ = first.Close()

	s := openFile(t, path, now)
	defer s.Close()
	if w, _ := s.GetWatcher("wch_pr"); w == nil || w.Status != "cancelled" {
		t.Fatalf("pr_state watcher should be cancelled, got %v", w)
	}
	if g, _ := s.GetGrant("grt_pr"); g == nil || g.RevokedAt == nil {
		t.Fatalf("pr watcher grant should be revoked")
	}
	if dw, _ := s.DueWatchers(now); len(dw) != 0 {
		t.Fatalf("no due watcher expected")
	}
}

// TestReopenResolvesWatcherInboxEvents — open watcher-sourced events (terminal /
// worktree / pr) are resolved on reopen so they don't resurface, while non-watcher
// sources persist and an ALREADY-resolved watcher event keeps its original
// resolvedAt (the `resolvedAt IS NULL` guard never re-stamps). Open
// watcher-sourced inbox events are resolved on reopen, sparing other sources.
func TestReopenResolvesWatcherInboxEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := int64(5000)
	first := openFile(t, path, now)

	pub := func(src domain.EventSource, sev domain.Severity, title, dedupe string) domain.QueueEvent {
		e, err := first.UpsertEvent(domain.QueuePublishArgs{
			Source: src, Severity: sev, Title: title, Summary: "s", DedupeKey: dedupe,
		})
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	pub(domain.SourceTerminalWatcher, domain.SeverityAttention, "term exited", "watcher:wch_old:term_1")
	pub(domain.SourceWorktreeWatcher, domain.SeverityAttention, "worktree gone", "")
	pub(domain.SourcePRWatcher, domain.SeverityAttention, "PR #7 merged", "pr_watcher:wch_old:state_change")
	pub(domain.SourceTimer, domain.SeverityInfo, "timer fired", "")
	pub(domain.SourceSystem, domain.SeverityInfo, "system note", "")
	// An already-resolved watcher event: its resolvedAt must survive untouched.
	resolved := pub(domain.SourceTerminalWatcher, domain.SeverityDone, "earlier alert", "earlier")
	if _, err := first.ResolveEvent(resolved.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := first.GetEvent(resolved.ID)
	origResolvedAt := got.ResolvedAt
	_ = first.Close()

	now2 := int64(6000)
	s := openFile(t, path, now2)
	defer s.Close()

	open, _ := s.ListEvents(domain.QueueDigestOptions{})
	for _, e := range open {
		if e.Source == domain.SourceTerminalWatcher || e.Source == domain.SourceWorktreeWatcher || e.Source == domain.SourcePRWatcher {
			t.Fatalf("watcher event %q should be resolved", e.Title)
		}
	}
	var sawTimer, sawSystem bool
	for _, e := range open {
		if e.Source == domain.SourceTimer {
			sawTimer = true
		}
		if e.Source == domain.SourceSystem {
			sawSystem = true
		}
	}
	if !sawTimer || !sawSystem {
		t.Fatalf("timer + system events must persist (timer=%v system=%v)", sawTimer, sawSystem)
	}

	// The three open watcher events are resolved (not deleted) — visible with
	// IncludeResolved and stamped, EXCEPT the already-resolved one keeps its stamp.
	all, _ := s.ListEvents(domain.QueueDigestOptions{IncludeResolved: true})
	sweptCount := 0
	for _, e := range all {
		isWatcher := e.Source == domain.SourceTerminalWatcher || e.Source == domain.SourceWorktreeWatcher || e.Source == domain.SourcePRWatcher
		if isWatcher && e.Title != "earlier alert" {
			sweptCount++
			if e.ResolvedAt == nil {
				t.Fatalf("swept watcher event %q must be stamped resolved", e.Title)
			}
		}
		if e.Title == "earlier alert" {
			if e.ResolvedAt == nil || origResolvedAt == nil || *e.ResolvedAt != *origResolvedAt {
				t.Fatalf("already-resolved event must keep original resolvedAt %v, got %v", origResolvedAt, e.ResolvedAt)
			}
		}
	}
	if sweptCount != 3 {
		t.Fatalf("want 3 swept watcher events, got %d", sweptCount)
	}
}

// TestReopenFailsStaleAgentLaunches — a non-terminal saga from a prior session is
// failed (errorCode SESSION_ENDED) so its idempotencyKey no longer blocks a fresh
// launch; a confirmed saga is untouched. Stale non-terminal records from a prior
// session are retired on reopen.
func TestReopenFailsStaleAgentLaunches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := openFile(t, path, 1000)
	inflight, _ := first.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "stale", AgentID: "claude", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchAmbiguous,
	})
	done, _ := first.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "done", AgentID: "claude", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchConfirmed,
	})
	_ = first.Close()

	s := openFile(t, path, 2000)
	defer s.Close()
	if a, _ := s.FindActiveAgentLaunch("stale"); a != nil {
		t.Fatalf("failed saga must not be active")
	}
	got, _ := s.GetAgentLaunch(inflight.ID)
	if got == nil || got.Stage != domain.LaunchFailed {
		t.Fatalf("stale saga should be failed, got %v", got)
	}
	if got.ErrorCode == nil || *got.ErrorCode != "SESSION_ENDED" {
		t.Fatalf("errorCode want SESSION_ENDED, got %v", got.ErrorCode)
	}
	if cg, _ := s.GetAgentLaunch(done.ID); cg == nil || cg.Stage != domain.LaunchConfirmed {
		t.Fatalf("confirmed saga must be untouched")
	}
}
