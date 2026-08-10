package storage

import (
	"path/filepath"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// TestReopenAdoptsNonTerminalWatchers — supervision is PROJECT-scoped: a second
// Open of the same file leaves non-terminal watchers (active/created/paused)
// exactly as the prior owner left them, keeps their automation grants live, and
// BeginOwnership reports their titles as resumed. Terminal-state watchers stay
// terminal. A due watcher fires on the new owner's first tick (DueWatchers
// still returns it).
func TestReopenAdoptsNonTerminalWatchers(t *testing.T) {
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
	// A live grant for each live watcher — all must SURVIVE the reopen (the
	// watcher keeps running, so its authority must too).
	for _, s := range nonTerminal {
		first.InsertGrant(domain.AutomationGrantRecord{
			ID: "grt_" + s, ActorID: "wch_" + s, ActorType: domain.GrantActorWatcher,
			AllowedRiskClassesJson: strPtr(`["terminal"]`), ExpiresAt: 9999999999999, MaxUses: 5,
		})
	}
	first.InsertTimer(domain.TimerRecord{ID: "tmr_keep", Title: "keep", FireAt: 9999999999999, PayloadType: "enqueue", PayloadJson: "{}"})
	_ = first.Close()

	s := openFile(t, path, now)
	defer s.Close()

	for _, st := range nonTerminal {
		w, _ := s.GetWatcher("wch_" + st)
		if w == nil || w.Status != st {
			t.Fatalf("wch_%s should be ADOPTED untouched, got %v", st, w)
		}
		if w.EndedReason != nil {
			t.Fatalf("adopted wch_%s must keep nil endedReason, got %q", st, *w.EndedReason)
		}
	}
	for _, st := range terminal {
		w, _ := s.GetWatcher("wch_" + st)
		if w == nil || w.Status != st {
			t.Fatalf("wch_%s should be untouched, got %v", st, w)
		}
	}

	// Ownership boot reports the adopted live watchers so the first turn can
	// surface the one-time resumed-supervision note.
	sum, err := s.BeginOwnership(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.ResumedWatcherTitles) != len(nonTerminal) {
		t.Fatalf("ResumedWatcherTitles want %d, got %v", len(nonTerminal), sum.ResumedWatcherTitles)
	}
	wantTitles := map[string]bool{"w wch_active": true, "w wch_created": true, "w wch_paused": true}
	for _, ti := range sum.ResumedWatcherTitles {
		if !wantTitles[ti] {
			t.Fatalf("unexpected resumed title %q", ti)
		}
	}

	// The 'active' watcher with nextCheckAt=0 is due immediately for the new
	// owner's scheduler — that is what "resumes automatically" means.
	dw, _ := s.DueWatchers(now)
	if len(dw) != 1 || dw[0].ID != "wch_active" {
		t.Fatalf("want the adopted active watcher due, got %v", dw)
	}
	// Grants survive with their watchers.
	for _, st := range nonTerminal {
		g, _ := s.GetGrant("grt_" + st)
		if g == nil || g.RevokedAt != nil {
			t.Fatalf("grt_%s must survive adoption unrevoked", st)
		}
	}
	if tm, _ := s.GetTimer("tmr_keep"); tm == nil || tm.Status != "scheduled" {
		t.Fatalf("persistent timer must survive untouched")
	}
}

// TestReopenAdoptsPRStateWatcher — a pr_state watcher is project-scoped like a
// terminal one: adopted on reopen with its grant intact.
func TestReopenAdoptsPRStateWatcher(t *testing.T) {
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
	if w, _ := s.GetWatcher("wch_pr"); w == nil || w.Status != "active" {
		t.Fatalf("pr_state watcher should be adopted active, got %v", w)
	}
	sum, err := s.BeginOwnership(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(sum.ResumedWatcherTitles) != 1 || sum.ResumedWatcherTitles[0] != "PR #5" {
		t.Fatalf("ResumedWatcherTitles want [PR #5], got %v", sum.ResumedWatcherTitles)
	}
	if g, _ := s.GetGrant("grt_pr"); g == nil || g.RevokedAt != nil {
		t.Fatalf("pr watcher grant should survive adoption")
	}
	if dw, _ := s.DueWatchers(now); len(dw) != 1 {
		t.Fatalf("adopted pr watcher should be due, got %d", len(dw))
	}
}

// TestReopenKeepsInboxOpen — the attention inbox is project-scoped: open events
// survive a reopen un-resolved (the next owner surfaces them), and an
// already-resolved event keeps its original stamp.
func TestReopenKeepsInboxOpen(t *testing.T) {
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
	pub(domain.SourceAsyncTool, domain.SeverityAttention, "async done", "async:asy_1")
	pub(domain.SourceTimer, domain.SeverityInfo, "timer fired", "")
	resolved := pub(domain.SourceTerminalWatcher, domain.SeverityDone, "earlier alert", "earlier")
	if _, err := first.ResolveEvent(resolved.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := first.GetEvent(resolved.ID)
	origResolvedAt := got.ResolvedAt
	_ = first.Close()

	s := openFile(t, path, int64(6000))
	defer s.Close()

	open, _ := s.ListEvents(domain.QueueDigestOptions{})
	if len(open) != 3 {
		t.Fatalf("the inbox must carry over: want 3 open events, got %d: %v", len(open), open)
	}
	re, _ := s.GetEvent(resolved.ID)
	if re == nil || re.ResolvedAt == nil || origResolvedAt == nil || *re.ResolvedAt != *origResolvedAt {
		t.Fatalf("already-resolved event must keep original resolvedAt %v, got %v", origResolvedAt, re)
	}
	sum, err := s.BeginOwnership(6000)
	if err != nil {
		t.Fatal(err)
	}
	if sum.OpenAttentionCount != 3 {
		t.Fatalf("OpenAttentionCount want 3, got %d", sum.OpenAttentionCount)
	}
}

// TestReopenAdoptsLiveAsyncInvocations — async futures are project-scoped: a
// live invocation survives the reopen for the next owner's coordinator to
// re-poll, and BeginOwnership counts it.
func TestReopenAdoptsLiveAsyncInvocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	first := openFile(t, path, 1000)
	live, err := first.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "left running", SessionID: "ses_1",
		TerminalIdsJson: `["term-1"]`, Status: domain.AsyncRunning, ExpiresAt: 100_000,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	s := openFile(t, path, 2000)
	defer s.Close()
	got, _ := s.GetAsyncInvocation(live.ID)
	if got == nil || got.Status != domain.AsyncRunning {
		t.Fatalf("live async row must survive reopen untouched, got %v", got)
	}
	sum, err := s.BeginOwnership(2000)
	if err != nil {
		t.Fatal(err)
	}
	if sum.ResumedAsyncCount != 1 {
		t.Fatalf("ResumedAsyncCount want 1, got %d", sum.ResumedAsyncCount)
	}
}

// TestCancelLiveWatchersClearsMidSession — the /clear path. CancelLiveWatchers tears
// down EVERY live watcher mid-session (no reopen) stamping ReasonSessionCleared,
// revokes its grant, and resolves its open watcher event — a clean slate while the
// session stays open. /clear is now the ONLY wholesale watcher teardown.
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

// TestBeginOwnershipResetsStaleAgentLaunches — ownership boot CLEARS the dead
// spawn roster: a prior owner's in-flight/ambiguous/failed (and
// confirmed-without-terminal) sagas are DELETED, so a stale "× FAILED" never
// greets the user and a fresh idempotencyKey isn't blocked. The one survivor is
// a confirmed saga that bound a terminal — kept so a still-running orphan agent
// can be re-adopted.
func TestBeginOwnershipResetsStaleAgentLaunches(t *testing.T) {
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
	// A bare reopen leaves the roster alone; the reset is an OWNERSHIP action.
	if got, _ := s.GetAgentLaunch(inflight.ID); got == nil {
		t.Fatalf("bare reopen must not delete sagas")
	}
	if _, err := s.BeginOwnership(2000); err != nil {
		t.Fatal(err)
	}
	if a, _ := s.FindActiveAgentLaunch("stale"); a != nil {
		t.Fatalf("cleared saga must not be active")
	}
	if got, _ := s.GetAgentLaunch(inflight.ID); got != nil {
		t.Fatalf("stale saga should be deleted at ownership boot, got %v", got)
	}
	if cg, _ := s.GetAgentLaunch(kept.ID); cg == nil || cg.Stage != domain.LaunchConfirmed {
		t.Fatalf("confirmed-with-terminal saga must survive, got %v", cg)
	}
}

// TestRuntimeStateRoundTrip — the cross-process handoff KV: put/get/delete, the
// empty-value-deletes contract, and the session-scoped backend-state helpers.
func TestRuntimeStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s := openFile(t, path, 1000)

	if v, _ := s.GetRuntimeState(RuntimeKeyCurrentSession); v != "" {
		t.Fatalf("fresh DB should have no current session, got %q", v)
	}
	if err := s.PutRuntimeState(RuntimeKeyCurrentSession, "ses_abc"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSessionBackendState("ses_abc", "tok_1"); err != nil {
		t.Fatal(err)
	}
	// Overwrite refreshes.
	if err := s.PutSessionBackendState("ses_abc", "tok_2"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()

	// The values survive a reopen — that is their whole purpose.
	s2 := openFile(t, path, 2000)
	defer s2.Close()
	if v, _ := s2.GetRuntimeState(RuntimeKeyCurrentSession); v != "ses_abc" {
		t.Fatalf("current session = %q, want ses_abc", v)
	}
	if tok, _ := s2.GetSessionBackendState("ses_abc"); tok != "tok_2" {
		t.Fatalf("backend state = %q, want tok_2", tok)
	}
	// Empty value deletes (the /clear contract).
	if err := s2.PutSessionBackendState("ses_abc", ""); err != nil {
		t.Fatal(err)
	}
	if tok, _ := s2.GetSessionBackendState("ses_abc"); tok != "" {
		t.Fatalf("cleared backend state should be empty, got %q", tok)
	}
	if err := s2.DeleteRuntimeState(RuntimeKeyCurrentSession); err != nil {
		t.Fatal(err)
	}
	if v, _ := s2.GetRuntimeState(RuntimeKeyCurrentSession); v != "" {
		t.Fatalf("deleted key should read empty, got %q", v)
	}
}
