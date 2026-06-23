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
		// The sweep stamps WHY (session_ended) + WHEN, so the row is distinguishable
		// from a deliberate user cancel.
		if w.EndedReason == nil || *w.EndedReason != "session_ended" {
			t.Fatalf("wch_%s want endedReason session_ended, got %v", st, w.EndedReason)
		}
		if w.EndedAt == nil || *w.EndedAt != now {
			t.Fatalf("wch_%s want endedAt %d, got %v", st, now, w.EndedAt)
		}
	}
	for _, st := range terminal {
		w, _ := s.GetWatcher("wch_" + st)
		if w == nil || w.Status != st {
			t.Fatalf("wch_%s should be untouched, got %v", st, w)
		}
		// A pre-existing terminal row is NOT re-stamped — including a 'cancelled' row
		// that carried no reason, which stays reasonless (the sweep only touches
		// non-terminal rows).
		if w.EndedReason != nil {
			t.Fatalf("terminal wch_%s should keep nil endedReason, got %q", st, *w.EndedReason)
		}
	}

	// The sweep carries the cancelled titles forward so the composition root can surface
	// a one-time NOTE. Exactly the three non-terminal watchers' titles, no terminal ones.
	ended := s.SessionEndedWatchers()
	if len(ended) != len(nonTerminal) {
		t.Fatalf("SessionEndedWatchers want %d titles, got %v", len(nonTerminal), ended)
	}
	wantTitles := map[string]bool{"w wch_active": true, "w wch_created": true, "w wch_paused": true}
	for _, ti := range ended {
		if !wantTitles[ti] {
			t.Fatalf("unexpected session-ended title %q", ti)
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
	} else if w.EndedReason == nil || *w.EndedReason != "session_ended" {
		t.Fatalf("pr_state watcher want endedReason session_ended, got %v", w.EndedReason)
	}
	if ended := s.SessionEndedWatchers(); len(ended) != 1 || ended[0] != "PR #5" {
		t.Fatalf("SessionEndedWatchers want [PR #5], got %v", ended)
	}
	if g, _ := s.GetGrant("grt_pr"); g == nil || g.RevokedAt == nil {
		t.Fatalf("pr watcher grant should be revoked")
	}
	if dw, _ := s.DueWatchers(now); len(dw) != 0 {
		t.Fatalf("no due watcher expected")
	}
}

// TestReopenResolvesWatcherInboxEvents — open watcher-sourced events (terminal /
// TestReopenResolvesAllInboxEvents — a fresh launch starts with a COMPLETELY empty
// inbox: every open event from a prior session (watcher, worktree, pr, timer, AND
// system) is resolved on reopen so nothing resurfaces (the !N badge starts at 0).
// An ALREADY-resolved event keeps its original resolvedAt (the `resolvedAt IS NULL`
// guard never re-stamps).
func TestReopenResolvesAllInboxEvents(t *testing.T) {
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
	// An already-resolved event: its resolvedAt must survive untouched.
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

	// Fresh start: NO open events of ANY source survive the reopen.
	if open, _ := s.ListEvents(domain.QueueDigestOptions{}); len(open) != 0 {
		t.Fatalf("a fresh launch must have an empty inbox, got %d open event(s): %v", len(open), open)
	}

	// All five freshly-published events are resolved (not deleted) — still visible
	// with IncludeResolved and stamped — EXCEPT the already-resolved one keeps its
	// original stamp.
	all, _ := s.ListEvents(domain.QueueDigestOptions{IncludeResolved: true})
	sweptCount := 0
	for _, e := range all {
		if e.Title == "earlier alert" {
			if e.ResolvedAt == nil || origResolvedAt == nil || *e.ResolvedAt != *origResolvedAt {
				t.Fatalf("already-resolved event must keep original resolvedAt %v, got %v", origResolvedAt, e.ResolvedAt)
			}
			continue
		}
		sweptCount++
		if e.ResolvedAt == nil || *e.ResolvedAt != now2 {
			t.Fatalf("swept event %q must be stamped resolved at %d, got %v", e.Title, now2, e.ResolvedAt)
		}
	}
	if sweptCount != 5 {
		t.Fatalf("want 5 swept events (all sources), got %d", sweptCount)
	}
}

// TestCancelLiveWatchersClearsMidSession — the /clear path. CancelLiveWatchers tears
// down EVERY live watcher mid-session (no reopen) stamping ReasonSessionCleared,
// revokes its grant, and resolves its open watcher event — a clean slate while the
// session stays open. Mirrors the session-boundary sweep but with the clear reason.
func TestCancelLiveWatchersClearsMidSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := int64(7000)
	s := openFile(t, path, now)
	defer s.Close()

	if _, err := s.InsertWatcher(domain.WatcherRecord{
		ID: "wch_live", Kind: "terminal", Title: "supervise edits", Goal: "supervise",
		TargetsJson: `["term_1"]`, CadenceMs: 5000, ModelTier: domain.ModelSmall,
		NextCheckAt: 0, Status: "active",
	}); err != nil {
		t.Fatal(err)
	}
	s.InsertGrant(domain.AutomationGrantRecord{
		ID: "grt_live", ActorID: "wch_live", ActorType: domain.GrantActorWatcher,
		AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: 9999999999999, MaxUses: 5,
	})
	if _, err := s.UpsertEvent(domain.QueuePublishArgs{
		Source: domain.SourceTerminalWatcher, Severity: domain.SeverityAttention,
		Title: "agent waiting", Summary: "s", DedupeKey: "watcher:wch_live:term_1",
	}); err != nil {
		t.Fatal(err)
	}

	titles, err := s.CancelLiveWatchers(now, ReasonSessionCleared)
	if err != nil {
		t.Fatal(err)
	}
	if len(titles) != 1 || titles[0] != "supervise edits" {
		t.Fatalf("want [supervise edits], got %v", titles)
	}

	w, _ := s.GetWatcher("wch_live")
	if w == nil || w.Status != "cancelled" {
		t.Fatalf("watcher should be cancelled, got %v", w)
	}
	if w.EndedReason == nil || *w.EndedReason != ReasonSessionCleared {
		t.Fatalf("want endedReason %q, got %v", ReasonSessionCleared, w.EndedReason)
	}
	if g, _ := s.GetGrant("grt_live"); g == nil || g.RevokedAt == nil {
		t.Fatal("watcher grant must be revoked on clear")
	}
	if dw, _ := s.DueWatchers(now); len(dw) != 0 {
		t.Fatalf("no watcher should be due after clear, got %d", len(dw))
	}
	open, _ := s.ListEvents(domain.QueueDigestOptions{})
	for _, e := range open {
		if e.Source == domain.SourceTerminalWatcher {
			t.Fatalf("watcher event %q should be resolved after clear", e.Title)
		}
	}
}

// TestResolveAllOpenEvents — the /clear inbox wipe primitive. Every open event of
// any source resolves, the call is idempotent, and it returns the count.
func TestResolveAllOpenEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	now := int64(8000)
	s := openFile(t, path, now)
	defer s.Close()

	mk := func(src domain.EventSource, title, dedupe string) {
		if _, err := s.UpsertEvent(domain.QueuePublishArgs{
			Source: src, Severity: domain.SeverityAttention, Title: title, Summary: "s", DedupeKey: dedupe,
		}); err != nil {
			t.Fatal(err)
		}
	}
	mk(domain.SourceTerminalWatcher, "agent waiting", "watcher:w:t")
	mk(domain.SourceSystem, "system note", "sys")
	mk(domain.SourceTimer, "timer fired", "tmr")

	n, err := s.ResolveAllOpenEvents(now + 1)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Fatalf("want 3 resolved, got %d", n)
	}
	if open, _ := s.ListEvents(domain.QueueDigestOptions{}); len(open) != 0 {
		t.Fatalf("inbox must be empty after ResolveAllOpenEvents, got %d", len(open))
	}
	// Idempotent: a second call resolves nothing.
	if n2, _ := s.ResolveAllOpenEvents(now + 2); n2 != 0 {
		t.Fatalf("second call should resolve 0, got %d", n2)
	}
}

// TestReopenResetsStaleAgentLaunches — the session-open reset CLEARS the dead spawn
// roster across a real reopen: a prior session's in-flight/ambiguous/failed (and
// confirmed-without-terminal) sagas are DELETED, so a stale "× FAILED" never greets the
// user and a fresh idempotencyKey isn't blocked. The one survivor is a confirmed saga
// that bound a terminal — kept so a still-running orphan agent can be re-adopted.
func TestReopenResetsStaleAgentLaunches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := openFile(t, path, 1000)
	inflight, _ := first.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "stale", AgentID: "claude", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchAmbiguous,
	})
	term := "terminal-7"
	kept, _ := first.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "done", AgentID: "claude", Mode: "edit", Title: "t", Name: "n",
		Stage: domain.LaunchConfirmed, TerminalID: &term,
	})
	_ = first.Close()

	s := openFile(t, path, 2000)
	defer s.Close()
	if a, _ := s.FindActiveAgentLaunch("stale"); a != nil {
		t.Fatalf("cleared saga must not be active")
	}
	if got, _ := s.GetAgentLaunch(inflight.ID); got != nil {
		t.Fatalf("stale saga should be deleted on reopen, got %v", got)
	}
	if cg, _ := s.GetAgentLaunch(kept.ID); cg == nil || cg.Stage != domain.LaunchConfirmed {
		t.Fatalf("confirmed-with-terminal saga must survive, got %v", cg)
	}
}
