package ui

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/daintreehq/assistant/internal/daemon"
	"github.com/daintreehq/assistant/internal/domain"
)

// dashboard_build_test.go covers the live-preview + durable-roster wiring: the
// PreviewPollMS throttle gate (resolvePreviews), the watcher⟕launch merge
// (BuildAgentRows), and the target/cache helpers (previewWatchers, filterPreviews).

// fakePreviewMCP is a daemon.MCP that records how many read calls it served, so the
// throttle gate can be asserted (a poll fires CallRead per target; a reuse fires
// none). It returns empty results — preview CONTENT folding is exercised separately
// against BuildAgentRows with hand-built previews. The counter is atomic because
// FetchPreviews calls CallRead from concurrent per-terminal goroutines.
type fakePreviewMCP struct {
	connected bool
	calls     int32
}

func (f *fakePreviewMCP) CallRead(ctx context.Context, name string, args map[string]any) (daemon.MCPResult, error) {
	atomic.AddInt32(&f.calls, 1)
	return daemon.MCPResult{}, nil
}
func (f *fakePreviewMCP) Connected() bool                               { return f.connected }
func (f *fakePreviewMCP) SupportsSubscribe() bool                       { return false }
func (f *fakePreviewMCP) Subscribe(_ context.Context, _ string) error   { return nil }
func (f *fakePreviewMCP) Unsubscribe(_ context.Context, _ string) error { return nil }

func TestResolvePreviews_FetchesWhenGateElapsed(t *testing.T) {
	mcp := &fakePreviewMCP{connected: true}
	targets := []daemon.PreviewTarget{{TerminalID: "term_1", WatcherID: "wch_1"}}
	// 10000ms since the last fetch (0) ≥ PreviewPollMS (2500) and connected → poll.
	previews, fetchedAt := resolvePreviews(context.Background(),
		dashboardBuildOptions{MCP: mcp, NowMS: 10_000, LastPreviewFetchedAt: 0}, targets)
	if atomic.LoadInt32(&mcp.calls) == 0 {
		t.Fatal("gate elapsed + connected: expected an MCP poll, got none")
	}
	if fetchedAt != 10_000 {
		t.Errorf("fetchedAt = %d, want 10000 (a real poll stamps NowMS)", fetchedAt)
	}
	if len(previews) != 1 || previews[0].TerminalID != "term_1" {
		t.Errorf("previews = %+v, want one card for term_1", previews)
	}
}

func TestResolvePreviews_ReusesCacheWithinGate(t *testing.T) {
	mcp := &fakePreviewMCP{connected: true}
	targets := []daemon.PreviewTarget{{TerminalID: "term_1"}}
	cached := []daemon.TerminalPreview{{TerminalID: "term_1", AgentState: "running"}}
	// 1000ms since the last fetch < PreviewPollMS (2500): must reuse, not poll.
	previews, fetchedAt := resolvePreviews(context.Background(),
		dashboardBuildOptions{MCP: mcp, NowMS: 11_000, LastPreviewFetchedAt: 10_000, CachedPreviews: cached}, targets)
	if c := atomic.LoadInt32(&mcp.calls); c != 0 {
		t.Fatalf("within gate: expected no MCP poll, got %d calls", c)
	}
	if fetchedAt != 0 {
		t.Errorf("fetchedAt = %d, want 0 (reuse must not advance the gate)", fetchedAt)
	}
	if len(previews) != 1 || previews[0].AgentState != "running" {
		t.Errorf("previews = %+v, want the cached term_1 card reused", previews)
	}
}

func TestResolvePreviews_SkipsWhenDisconnected(t *testing.T) {
	mcp := &fakePreviewMCP{connected: false}
	targets := []daemon.PreviewTarget{{TerminalID: "term_1"}}
	_, fetchedAt := resolvePreviews(context.Background(),
		dashboardBuildOptions{MCP: mcp, NowMS: 99_000, LastPreviewFetchedAt: 0}, targets)
	if c := atomic.LoadInt32(&mcp.calls); c != 0 {
		t.Fatalf("disconnected: expected no MCP poll, got %d", c)
	}
	if fetchedAt != 0 {
		t.Errorf("fetchedAt = %d, want 0 when the link is down", fetchedAt)
	}
}

func TestResolvePreviews_FirstBuildAlwaysFetches(t *testing.T) {
	// The first build (LastPreviewFetchedAt == 0) must always fetch, even when
	// NowMS is less than PreviewPollMS. This ensures prior-session dead terminals
	// are detected immediately on startup rather than lingering until the gate elapses.
	mcp := &fakePreviewMCP{connected: true}
	targets := []daemon.PreviewTarget{{TerminalID: "term_1"}}
	// 1000ms < PreviewPollMS (2500): without the fix, this would skip fetch.
	// With LastPreviewFetchedAt == 0, it must fetch regardless.
	previews, fetchedAt := resolvePreviews(context.Background(),
		dashboardBuildOptions{MCP: mcp, NowMS: 1_000, LastPreviewFetchedAt: 0}, targets)
	if atomic.LoadInt32(&mcp.calls) == 0 {
		t.Fatal("first build (LastPreviewFetchedAt == 0): expected an MCP poll even within gate, got none")
	}
	if fetchedAt != 1_000 {
		t.Errorf("fetchedAt = %d, want 1000 (first build always stamps NowMS)", fetchedAt)
	}
	if len(previews) != 1 || previews[0].TerminalID != "term_1" {
		t.Errorf("previews = %+v, want one card for term_1", previews)
	}
}

func TestResolvePreviews_NoTargets(t *testing.T) {
	mcp := &fakePreviewMCP{connected: true}
	previews, fetchedAt := resolvePreviews(context.Background(),
		dashboardBuildOptions{MCP: mcp, NowMS: 99_000}, nil)
	if previews != nil || fetchedAt != 0 || atomic.LoadInt32(&mcp.calls) != 0 {
		t.Errorf("no targets: want (nil,0,no calls), got (%+v,%d,%d)", previews, fetchedAt, mcp.calls)
	}
}

func TestBuildAgentRows_PopulatesPreview(t *testing.T) {
	w := watcherRec("wch_1", string(domain.ClassStillWorking), nil)
	w.TargetsJson = `["term_1"]`
	previews := []daemon.TerminalPreview{{TerminalID: "term_1", AgentState: "running", Tail: "compiling...\n$ go test\nPASS\n"}}
	rows := BuildAgentRows([]domain.WatcherRecord{w}, previews, nil)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].AgentState != "running" {
		t.Errorf("AgentState = %q, want running (folded from preview)", rows[0].AgentState)
	}
	if rows[0].Preview != "PASS" {
		t.Errorf("Preview = %q, want last non-blank tail line PASS", rows[0].Preview)
	}
}

func TestBuildAgentRows_LaunchOnlyRowStaysVisible(t *testing.T) {
	// No watchers (the agent's watcher was cancelled), but the saga is on the roster AND
	// its terminal is still live this session: the agent must NOT vanish from the deck.
	term := "term_9"
	launches := []domain.AgentLaunchRecord{{
		ID: "agt_1", Title: "fix flaky test", TerminalID: &term, Stage: domain.LaunchConfirmed, CreatedAt: 5,
	}}
	previews := []daemon.TerminalPreview{{TerminalID: "term_9", AgentState: "running"}}
	rows := BuildAgentRows(nil, previews, launches)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (launch-only agent with a live terminal must stay visible)", len(rows))
	}
	if rows[0].ID != "term_9" {
		t.Errorf("ID = %q, want term_9 (prefers terminal id)", rows[0].ID)
	}
	if rows[0].Title != "fix flaky test" || rows[0].Badge != "WORKING" {
		t.Errorf("row = %+v, want a titled WORKING row", rows[0])
	}
}

func TestBuildAgentRows_JoinsLaunchToLiveWatcherNoDuplicate(t *testing.T) {
	w := watcherRec("wch_live", string(domain.ClassStillWorking), nil)
	wid := "wch_live"
	launches := []domain.AgentLaunchRecord{{
		ID: "agt_1", Title: "same agent", WatcherID: &wid, Stage: domain.LaunchConfirmed, CreatedAt: 5,
	}}
	rows := BuildAgentRows([]domain.WatcherRecord{w}, nil, launches)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (launch joined to its live watcher, not duplicated)", len(rows))
	}
	if rows[0].ID != "wch_live" {
		t.Errorf("ID = %q, want wch_live (the live watcher owns the row)", rows[0].ID)
	}
}

func TestBuildAgentRows_SettledLaunchWithoutLiveTerminalHidden(t *testing.T) {
	// A settled saga (failed or confirmed) whose terminal is no longer live is dead
	// history from a prior session — it must NOT resurface on the deck. This is the
	// "× FAILED greets you on restart" bug: the strict session reset hides it.
	gone := "wch_gone"
	term := "term_dead"
	launches := []domain.AgentLaunchRecord{
		{ID: "agt_failed", Title: "old fail", WatcherID: &gone, Stage: domain.LaunchFailed, CreatedAt: 5},
		{ID: "agt_conf", Title: "old confirmed", TerminalID: &term, Stage: domain.LaunchConfirmed, CreatedAt: 6},
	}
	if rows := BuildAgentRows(nil, nil, launches); len(rows) != 0 {
		t.Fatalf("settled launches with no live terminal must be hidden, got %+v", rows)
	}
}

func TestBuildAgentRows_SettledLaunchWithDeadTerminalHidden(t *testing.T) {
	// A settled launch whose terminal preview exists but has empty AgentState
	// (the terminal is dead/gone, MCP returned no status) must also be hidden.
	// This covers the case where FetchPreviews creates a placeholder preview for
	// every target, but the terminal.getStatus call returns nothing for dead terminals.
	term := "term_dead_preview"
	launches := []domain.AgentLaunchRecord{
		{ID: "agt_failed", Title: "old failed spawn", TerminalID: &term, Stage: domain.LaunchFailed, CreatedAt: 5},
	}
	// Preview exists but AgentState is empty — terminal is not actually live.
	previews := []daemon.TerminalPreview{{TerminalID: "term_dead_preview", AgentState: "", UpdatedAt: 100}}
	if rows := BuildAgentRows(nil, previews, launches); len(rows) != 0 {
		t.Fatalf("settled launch with dead terminal (empty AgentState) must be hidden, got %+v", rows)
	}
}

func TestBuildAgentRows_InFlightLaunchAlwaysShown(t *testing.T) {
	// An in-flight saga is guaranteed this-session (prior-session non-terminal sagas are
	// cleared on DB open), so it shows as STARTING even before its terminal/preview exists.
	launches := []domain.AgentLaunchRecord{{
		ID: "agt_new", Title: "spinning up", Stage: domain.TerminalBound, CreatedAt: 5,
	}}
	rows := BuildAgentRows(nil, nil, launches)
	if len(rows) != 1 || rows[0].Badge != "STARTING" {
		t.Fatalf("in-flight launch must show as STARTING, got %+v", rows)
	}
}

func TestBuildAgentRows_CancelledWatcherDoesNotMaskLaunchRoster(t *testing.T) {
	// The watcher row is STILL in the DB after cancellation (ListWatchers("") returns
	// every status, incl. prior-session watchers force-cancelled on DB open). It must
	// not be treated as live and mask its launch — the agent should show its saga state.
	w := watcherRec("wch_done", string(domain.ClassCompletedSuccess), nil)
	w.Status = "cancelled"
	term := "term_3"
	wid := "wch_done"
	launches := []domain.AgentLaunchRecord{{
		ID: "agt_1", Title: "shipped agent", WatcherID: &wid, TerminalID: &term,
		Stage: domain.LaunchConfirmed, CreatedAt: 7,
	}}
	// The terminal is still live this session, so the saga-derived row survives.
	previews := []daemon.TerminalPreview{{TerminalID: "term_3", AgentState: "waiting"}}
	rows := BuildAgentRows([]domain.WatcherRecord{w}, previews, launches)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (cancelled watcher defers to its launch row)", len(rows))
	}
	if rows[0].ID != "term_3" || rows[0].Badge != "WORKING" {
		t.Errorf("row = %+v, want the saga-derived term_3/WORKING row, not the stale watcher", rows[0])
	}
}

func TestBuildAgentRows_LaunchWithNilWatcherIDNoTerminalDuplicate(t *testing.T) {
	// A live watcher owns term_1; a launch on the same terminal never recorded its
	// WatcherID. Terminal-id dedupe must collapse them to one row (the live watcher's).
	w := watcherRec("wch_1", string(domain.ClassStillWorking), nil)
	w.TargetsJson = `["term_1"]`
	term := "term_1"
	launches := []domain.AgentLaunchRecord{{
		ID: "agt_1", Title: "same terminal", TerminalID: &term, Stage: domain.LaunchConfirmed, CreatedAt: 5,
	}}
	rows := BuildAgentRows([]domain.WatcherRecord{w}, nil, launches)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (no terminal-based duplicate)", len(rows))
	}
	if rows[0].ID != "wch_1" {
		t.Errorf("ID = %q, want wch_1 (the live watcher owns the shared terminal)", rows[0].ID)
	}
}

func TestPreviewWatchers_ExcludesNonActiveWatchers(t *testing.T) {
	// A cancelled terminal watcher must not consume a preview slot (its terminal is
	// likely dead) — only the live watcher's terminal is a target.
	active := watcherRec("wch_live", string(domain.ClassStillWorking), nil)
	active.TargetsJson = `["term_live"]`
	cancelled := watcherRec("wch_dead", string(domain.ClassCompletedSuccess), nil)
	cancelled.Status = "cancelled"
	cancelled.TargetsJson = `["term_dead"]`
	pws := previewWatchers([]domain.WatcherRecord{active, cancelled}, nil)
	gotTerms := map[string]bool{}
	for _, pw := range pws {
		for _, id := range pw.TerminalIDs {
			gotTerms[id] = true
		}
	}
	if !gotTerms["term_live"] || gotTerms["term_dead"] {
		t.Errorf("preview terminals = %v, want term_live only (cancelled watcher excluded)", gotTerms)
	}
}

func TestLaunchBadge_StageMapping(t *testing.T) {
	cases := []struct {
		stage    domain.AgentLaunchStage
		badge    string
		priority int
	}{
		{domain.LaunchFailed, "FAILED", 1},
		{domain.LaunchAmbiguous, "REVIEW", 2},
		{domain.LaunchConfirmed, "WORKING", 3},
		{domain.LaunchRequested, "STARTING", 3},
		{domain.AgentStarted, "STARTING", 3},
		{domain.TerminalBound, "STARTING", 3},
		{domain.WatcherAttached, "STARTING", 3},
	}
	for _, c := range cases {
		b, p := launchBadge(c.stage)
		if b != c.badge || p != c.priority {
			t.Errorf("launchBadge(%q) = (%q,%d), want (%q,%d)", c.stage, b, p, c.badge, c.priority)
		}
	}
}

func TestPreviewWatchers_TerminalWatchersAndUncoveredLaunches(t *testing.T) {
	term := watcherRec("wch_t", string(domain.ClassStillWorking), nil)
	term.TargetsJson = `["term_1"]`
	pr := watcherRec("wch_pr", "", nil)
	pr.Kind = "pr_state"
	pr.TargetsJson = `["pr_42"]`
	// One launch on an already-covered terminal (deduped) and one on a fresh terminal.
	covered := "term_1"
	fresh := "term_2"
	launches := []domain.AgentLaunchRecord{
		{ID: "agt_dup", TerminalID: &covered},
		{ID: "agt_new", Title: "new", TerminalID: &fresh},
	}
	pws := previewWatchers([]domain.WatcherRecord{term, pr}, launches)
	if len(pws) != 2 {
		t.Fatalf("previewWatchers = %d entries, want 2 (%+v)", len(pws), pws)
	}
	gotTerms := map[string]bool{}
	for _, pw := range pws {
		for _, id := range pw.TerminalIDs {
			gotTerms[id] = true
		}
	}
	if !gotTerms["term_1"] || !gotTerms["term_2"] || gotTerms["pr_42"] {
		t.Errorf("preview terminals = %v, want term_1+term_2 only (pr_state excluded, dup dropped)", gotTerms)
	}
}

func TestFilterPreviews_DropsStaleCards(t *testing.T) {
	cached := []daemon.TerminalPreview{
		{TerminalID: "term_1", AgentState: "a"},
		{TerminalID: "term_closed", AgentState: "b"},
	}
	targets := []daemon.PreviewTarget{{TerminalID: "term_1"}}
	out := filterPreviews(cached, targets)
	if len(out) != 1 || out[0].TerminalID != "term_1" {
		t.Errorf("filterPreviews = %+v, want only the still-targeted term_1", out)
	}
}

func TestTerminalIDs_MalformedDegrades(t *testing.T) {
	if ids := terminalIDs(`{not an array}`); ids != nil {
		t.Errorf("malformed targetsJson → %v, want nil (degrade, no panic)", ids)
	}
	if got := firstTerminalID(`["", "term_2"]`); got != "term_2" {
		t.Errorf("firstTerminalID skipped empty? got %q want term_2", got)
	}
}
