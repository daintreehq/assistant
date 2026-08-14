package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// The open-terminal inventory is served from a cross-turn cache and refreshed on a
// DETACHED goroutine, so a turn NEVER blocks on the terminal.list + getStatus MCP
// round-trip that used to gate its first model round (up to 5s, every MCP-connected
// turn). These tests pin that contract: non-blocking, cached-and-refreshed per turn,
// stable across the rounds of a turn, and a harmless nil-fetcher path.

// TestOpenTerminals_FetchDoesNotBlockTurn is the marquee guarantee: with a fetcher that
// blocks indefinitely, Send must still finish the turn — the refresh runs detached and
// the turn streams against whatever the cache already held (nil, on this cold first turn).
func TestOpenTerminals_FetchDoesNotBlockTurn(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		entered <- struct{}{}
		<-release // block until the test releases — simulates a hung/slow MCP
		return []backend.OpenTerminal{{ID: "terminal-1", Kind: "agent"}}
	}
	s := NewSession(deps)

	done := make(chan string, 1)
	go func() {
		reply, _ := s.Send(context.Background(), "hi", SendOptions{})
		done <- reply
	}()

	// The detached refresh must have started (proves the turn kicked it, not skipped it).
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("roster refresh was never kicked off")
	}

	// The turn must finish WITHOUT the fetcher returning — it is still blocked on release.
	select {
	case reply := <-done:
		if reply != "ok" {
			t.Fatalf("reply = %q, want ok", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on the roster fetch — the inventory read must be detached")
	}

	// Cold first round carried no roster (the refresh is still in flight).
	if rc := be.runtimeAt(0); len(rc.OpenTerminals) != 0 {
		t.Errorf("cold first round should carry no roster, got %d entries", len(rc.OpenTerminals))
	}

	close(release)
	s.DrainBackgroundWork() // join the detached refresh so the test leaks no goroutine
}

// TestOpenTerminals_CacheWarmsNextTurn verifies the cross-turn cache: turn 1's detached
// refresh populates the roster, and turn 2 serves it into round 0's runtime block with
// no blocking read of its own.
func TestOpenTerminals_CacheWarmsNextTurn(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "a"}, {Content: "b"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		return []backend.OpenTerminal{{ID: "terminal-42", Kind: "agent", Title: "worker"}}
	}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Force turn 1's detached refresh to land in the cache before turn 2 reads it.
	// DrainBackgroundWork is terminal (turn 2 won't refresh), but it still SERVES the
	// warmed cache — exactly what we assert.
	s.DrainBackgroundWork()

	if _, err := s.Send(context.Background(), "two", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// req[1] is turn 2, round 0. Its runtime block must carry the roster turn 1 warmed.
	rc := be.runtimeAt(1)
	if len(rc.OpenTerminals) != 1 || rc.OpenTerminals[0].ID != "terminal-42" {
		t.Fatalf("turn 2 round 0 should serve the warmed roster, got %+v", rc.OpenTerminals)
	}
}

// TestOpenTerminals_CachedSnapshotRidesEveryRound pins two properties within one turn:
// the SAME cached snapshot rides EVERY round (it is not re-fetched per round, which would
// multiply the MCP read budget across a multi-round turn), and the refresh runs at most
// once per turn. It warms the cache first (turn 1 + drain), which also freezes it (drain
// is terminal), so a multi-round turn 2 reads one stable snapshot deterministically.
func TestOpenTerminals_CachedSnapshotRidesEveryRound(t *testing.T) {
	calls := 0
	snap := []backend.OpenTerminal{{ID: "terminal-1", Kind: "agent", AgentState: "running"}}
	r := &injectRouter{results: []models.ChatResult{
		{Content: "warm"}, // turn 1, round 0 → ends (warms the cache)
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // turn 2, round 0 → loop
		{Content: "final"}, // turn 2, round 1 → ends
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.OpenTerminalsFetcher = func(context.Context) []backend.OpenTerminal { calls++; return snap }
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "warm", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork() // land turn 1's refresh (calls==1) and freeze the cache

	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 3 {
		t.Fatalf("want turn 1 (1 round) + turn 2 (2 rounds) = 3 requests, got %d", len(be.requests()))
	}
	// Exactly one refresh for the whole run — the multi-round turn 2 kicked no fetch of
	// its own (frozen by drain), proving the read is never per-round. (Read after the
	// drain join, so no race on calls.)
	if calls != 1 {
		t.Fatalf("fetcher should run once, not once per round, got %d", calls)
	}
	// Both rounds of turn 2 (req[1], req[2]) carry the same warmed snapshot.
	for i := 1; i <= 2; i++ {
		got := be.runtimeAt(i).OpenTerminals
		if len(got) != 1 || got[0].ID != "terminal-1" || got[0].AgentState != "running" {
			t.Errorf("round %d runtime should carry the cached snapshot, got %+v", i, got)
		}
	}
}

// TestOpenTerminals_RefreshDedupedAcrossTurns proves the dedupe in a LIVE session (no
// DrainBackgroundWork freeze): while turn 1's refresh is still in flight, turn 2's kick
// must reuse it rather than stack a second concurrent fetch. A fetcher that blocks holds
// the refresh open across both turns; the fetcher must be ENTERED exactly once.
func TestOpenTerminals_RefreshDedupedAcrossTurns(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var calls atomic.Int32

	r := &fakeRouter{results: []models.ChatResult{{Content: "one"}, {Content: "two"}}}
	deps, _ := recordingDeps(r, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		calls.Add(1)
		entered <- struct{}{}
		<-release // hold the refresh open across BOTH turns
		return []backend.OpenTerminal{{ID: "terminal-1"}}
	}
	s := NewSession(deps)

	// Turn 1 kicks the refresh (sets rosterRefreshing=true synchronously, spawns it).
	if _, err := s.Send(context.Background(), "one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Ensure turn 1's refresh goroutine actually reached the (blocked) fetcher.
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("turn 1 refresh never entered the fetcher")
	}

	// Turn 2 runs while the refresh is still in flight — its kick must dedupe (bail),
	// spawning no second fetch. (Send must not block: refreshRosterAsync returns at once.)
	if _, err := s.Send(context.Background(), "two", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// No second entry: exactly one fetch served both turns. Give any (erroneously
	// spawned) second goroutine a moment to reach the fetcher before asserting.
	select {
	case <-entered:
		t.Fatal("a second refresh entered the fetcher — turn 2 must reuse the in-flight one")
	case <-time.After(150 * time.Millisecond):
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("fetcher entered %d times, want exactly 1 (deduped across turns)", got)
	}

	close(release)
	s.DrainBackgroundWork()
}

// TestOpenTerminals_CloseSettlePrunesRosterForNextRound pins the close-consistency
// contract end-to-end through the turn loop: when a terminal.close result settles, the
// closed ids are pruned from the cached roster SYNCHRONOUSLY, so the very next round's
// runtime block already reflects the close — it never waits on (or races) a detached
// MCP refresh. This is the 2026-07-11 regression: a close in turn N left the cache
// showing the closed terminals as open, and the model in turn N+1 announced the close
// "didn't stick".
func TestOpenTerminals_CloseSettlePrunesRosterForNextRound(t *testing.T) {
	snap := []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}, {ID: "terminal-3"}}
	r := &injectRouter{results: []models.ChatResult{
		{Content: "warm"}, // turn 1, round 0 → ends (warms the cache)
		{ToolCalls: []models.ToolCallRequest{
			toolCall("c1", "terminal__close", `{"terminalIds":["terminal-2","terminal-3"]}`),
		}}, // turn 2, round 0 → close
		{Content: "done"}, // turn 2, round 1 → ends
	}}
	closeRes := domain.Ok("Closed 2 terminal(s): terminal-2, terminal-3.",
		map[string]any{"closed": []string{"terminal-2", "terminal-3"}})
	deps, be := recordingDeps(r, &fakeTools{result: closeRes})
	deps.OpenTerminalsFetcher = func(context.Context) []backend.OpenTerminal { return snap }
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "warm", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// Land turn 1's refresh in the cache. Drain is terminal — no later fetch can run —
	// so any change the next round sees can ONLY be the synchronous settle-time prune.
	s.DrainBackgroundWork()

	if _, err := s.Send(context.Background(), "close 2 and 3", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// Turn 2 round 0 (req[1], built BEFORE the close settled) carries the full roster.
	if got := be.runtimeAt(1).OpenTerminals; len(got) != 3 {
		t.Fatalf("round 0 should carry the pre-close roster, got %+v", got)
	}
	// Turn 2 round 1 (req[2], built right AFTER the close settled) must already be pruned.
	got := be.runtimeAt(2).OpenTerminals
	if len(got) != 1 || got[0].ID != "terminal-1" {
		t.Fatalf("round after the close should carry only terminal-1, got %+v", got)
	}
}

// TestOpenTerminals_StaleFetchCannotResurrectClosedTerminals pins the rosterGen race
// guard, including the NEVER-stale property (not just eventual consistency): a
// detached fetch that STARTED before a close settles carries pre-close truth, so
// committing it after the prune would resurrect the closed terminal in the cache. The
// refresher must discard that stale snapshot — the cache mid-refetch must already show
// the pruned roster — and the refetch (started after the close) commits post-close truth.
func TestOpenTerminals_StaleFetchCannotResurrectClosedTerminals(t *testing.T) {
	release1 := make(chan struct{})
	release2 := make(chan struct{})
	entered1 := make(chan struct{}, 1)
	entered2 := make(chan struct{}, 1)
	var once1, once2 sync.Once
	// A t.Fatal on a timeout below must not strand the fetcher goroutine on its gate.
	t.Cleanup(func() { once1.Do(func() { close(release1) }); once2.Do(func() { close(release2) }) })
	var calls atomic.Int32

	deps, _ := recordingDeps(&fakeRouter{results: []models.ChatResult{{Content: "ok"}}}, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		if calls.Add(1) == 1 {
			entered1 <- struct{}{}
			<-release1                                                            // hold the PRE-close fetch in flight across the prune
			return []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}} // pre-close truth
		}
		entered2 <- struct{}{}
		<-release2                                        // hold the refetch open so the discard window is observable
		return []backend.OpenTerminal{{ID: "terminal-1"}} // post-close truth
	}
	s := NewSession(deps)

	// Seed the warmed cache directly (whitebox): the pre-close roster an earlier
	// completed refresh would have left. Fetch #1 below is held open, so nothing else
	// could warm it deterministically.
	s.rosterMu.Lock()
	s.roster = []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}}
	s.rosterMu.Unlock()

	s.WarmOpenTerminals()
	select {
	case <-entered1:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never entered the fetcher")
	}

	// terminal-2 closes while the pre-close fetch is still in flight.
	s.observeRosterMutation("terminal.close", "{}",
		domain.Ok("Closed 1 terminal(s): terminal-2.", map[string]any{"closed": []string{"terminal-2"}}))

	once1.Do(func() { close(release1) })
	select {
	case <-entered2:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never refetched after the discarded stale attempt")
	}
	// The refetch is in flight, so fetch #1 has been fully decided: had it committed,
	// terminal-2 would be back. The cache must still show exactly the pruned roster.
	if got := s.currentRoster(); len(got) != 1 || got[0].ID != "terminal-1" {
		t.Fatalf("stale pre-close fetch must never commit; want [terminal-1] mid-refetch, got %+v", got)
	}

	once2.Do(func() { close(release2) })
	s.DrainBackgroundWork() // join the refresher: the refetch commits post-close truth

	got := s.currentRoster()
	if len(got) != 1 || got[0].ID != "terminal-1" {
		t.Fatalf("want [terminal-1] after the refetch commits, got %+v", got)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("refresher should refetch exactly once after the discard, got %d fetches", n)
	}
}

// TestOpenTerminals_LaggyServerFetchCannotResurrectClosedTerminals pins the tombstone
// guard — the 2026-07-11 (ses_a9e0a6ef) regression the rosterGen guard is blind to:
// Daintree acks terminal.close BEFORE terminal.list reflects the teardown, so the
// reconciliation fetch kicked by the close itself (started after the prune — gen
// matches at commit time) read a pre-close list and re-committed all the closed
// terminals, which the next turn's round 0 then served to the model. A fetch commit
// must drop confirmed-closed ids no matter when the fetch ran.
func TestOpenTerminals_LaggyServerFetchCannotResurrectClosedTerminals(t *testing.T) {
	var calls atomic.Int32
	deps, _ := recordingDeps(&fakeRouter{results: []models.ChatResult{{Content: "ok"}}}, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		// The laggy server: the close has been acked, but list still shows both.
		calls.Add(1)
		return []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}}
	}
	s := NewSession(deps)

	// The warmed pre-close cache an earlier completed refresh would have left.
	s.rosterMu.Lock()
	s.roster = []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}}
	s.rosterMu.Unlock()

	// terminal-2 closes. The settle path prunes + tombstones it, then kicks the
	// reconciliation refresh — which fetches the stale pre-close list above.
	s.observeRosterMutation("terminal.close", "{}",
		domain.Ok("Closed 1 terminal(s): terminal-2.", map[string]any{"closed": []string{"terminal-2"}}))
	// Join the kicked refresh WITHOUT draining (drain is terminal and would block the
	// second refresh below). The refresh registered on s.wg before observe returned,
	// and nothing else is on the wg in this test.
	s.wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("the close settle must kick exactly one reconciliation fetch, got %d", n)
	}
	got := s.currentRoster()
	if len(got) != 1 || got[0].ID != "terminal-1" {
		t.Fatalf("laggy post-close fetch must not resurrect terminal-2; want [terminal-1], got %+v", got)
	}

	// A LATER, unrelated refresh reading a still-stale list must be filtered too —
	// the tombstone outlives the immediately-kicked reconciliation fetch.
	s.WarmOpenTerminals()
	s.DrainBackgroundWork()
	if n := calls.Load(); n != 2 {
		t.Fatalf("want two completed fetches (reconciliation + warm), got %d", n)
	}
	got = s.currentRoster()
	if len(got) != 1 || got[0].ID != "terminal-1" {
		t.Fatalf("later stale fetch must not resurrect terminal-2 either; want [terminal-1], got %+v", got)
	}
}

// TestOpenTerminals_TombstoneExpiryLetsRestoredTerminalReappear pins the OTHER side of
// the tombstone contract: terminal.close moves a terminal to Daintree's TRASH (only
// terminal.kill deletes permanently), so a human can restore it under the SAME id — a
// tombstone must therefore expire (rosterTombstoneTTL) rather than suppress the id for
// the whole session, and an expired entry is deleted at the fetch commit so the map
// stays bounded by recent closes.
func TestOpenTerminals_TombstoneExpiryLetsRestoredTerminalReappear(t *testing.T) {
	deps, _ := recordingDeps(&fakeRouter{results: []models.ChatResult{{Content: "ok"}}}, &fakeTools{})
	deps.OpenTerminalsFetcher = func(context.Context) []backend.OpenTerminal {
		return []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}}
	}
	s := NewSession(deps)

	// terminal-2 was closed long ago (whitebox: its tombstone already expired) and has
	// since been restored from the trash — the server legitimately lists it again.
	s.rosterMu.Lock()
	s.rosterTombstones = map[string]time.Time{"terminal-2": time.Now().Add(-time.Second)}
	s.rosterMu.Unlock()

	s.WarmOpenTerminals()
	s.DrainBackgroundWork()

	if got := s.currentRoster(); len(got) != 2 {
		t.Fatalf("an expired tombstone must not suppress a restored terminal; want both entries, got %+v", got)
	}
	s.rosterMu.Lock()
	_, still := s.rosterTombstones["terminal-2"]
	s.rosterMu.Unlock()
	if still {
		t.Fatal("the expired tombstone should be deleted at the fetch commit")
	}
}

// TestOpenTerminals_SpawnSettleInvalidatesInFlightFetch: a successful spawn can't be
// patched into the cache locally (the roster entry needs live agent state), but it must
// still invalidate an in-flight PRE-spawn fetch so the refetch picks the new terminal up.
func TestOpenTerminals_SpawnSettleInvalidatesInFlightFetch(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	var once sync.Once
	t.Cleanup(func() { once.Do(func() { close(release) }) })
	var calls atomic.Int32

	deps, _ := recordingDeps(&fakeRouter{results: []models.ChatResult{{Content: "ok"}}}, &fakeTools{})
	deps.OpenTerminalsFetcher = func(ctx context.Context) []backend.OpenTerminal {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-release  // hold the PRE-spawn fetch in flight across the spawn settle
			return nil // pre-spawn truth: nothing open
		}
		return []backend.OpenTerminal{{ID: "terminal-new", AgentID: "claude"}} // post-spawn truth
	}
	s := NewSession(deps)

	s.WarmOpenTerminals()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("refresh never entered the fetcher")
	}

	s.observeRosterMutation("agentTask.spawnForEdits", "{}", domain.Ok("Spawned claude.", nil))

	once.Do(func() { close(release) })
	s.DrainBackgroundWork()

	got := s.currentRoster()
	if len(got) != 1 || got[0].ID != "terminal-new" {
		t.Fatalf("post-spawn refetch should carry the new terminal, got %+v", got)
	}
}

// TestOpenTerminals_PartialCloseFailurePrunesOnlyReportedIDs drives a PARTIAL close
// failure through the full settle path: the model asked to close terminal-2 AND
// terminal-3, but the result reports only terminal-2 closed (terminal-3 failed). The
// prune must be RESULT-driven — terminal-2 leaves the roster, terminal-3 stays — and a
// failed result must not be ignored (its details ids DID close).
func TestOpenTerminals_PartialCloseFailurePrunesOnlyReportedIDs(t *testing.T) {
	snap := []backend.OpenTerminal{{ID: "terminal-1"}, {ID: "terminal-2"}, {ID: "terminal-3"}}
	r := &injectRouter{results: []models.ChatResult{
		{Content: "warm"}, // turn 1, round 0 → ends (warms the cache)
		{ToolCalls: []models.ToolCallRequest{
			toolCall("c1", "terminal__close", `{"terminalIds":["terminal-2","terminal-3"]}`),
		}}, // turn 2, round 0 → close (partially fails)
		{Content: "done"}, // turn 2, round 1 → ends
	}}
	closeRes := domain.Fail("mcp_tool_error",
		"Closed 1 of 2 terminal(s); failed to close: terminal-3.",
		domain.WithDetails(map[string]any{"closed": []string{"terminal-2"}, "failed": []string{"terminal-3"}}))
	deps, be := recordingDeps(r, &fakeTools{result: closeRes})
	deps.OpenTerminalsFetcher = func(context.Context) []backend.OpenTerminal { return snap }
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "warm", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork() // land turn 1's refresh; drain is terminal — only the prune can change the cache

	if _, err := s.Send(context.Background(), "close 2 and 3", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	got := be.runtimeAt(2).OpenTerminals
	if len(got) != 2 || got[0].ID != "terminal-1" || got[1].ID != "terminal-3" {
		t.Fatalf("partial failure must prune only the reported id; want [terminal-1 terminal-3], got %+v", got)
	}
}

// TestObserveRosterMutation_InvalidatesOnlyRosterMutators pins the classification: which
// settled tools invalidate the roster cache (bump rosterGen) and which must not. Notably
// workflow.prepBranchForReview is a READ-ONLY readiness diagnostic (despite the name),
// spawn-family failures still invalidate (an ambiguous failure may have launched), and
// daintree.call invalidates only for successful inner terminal.* mutations.
func TestObserveRosterMutation_InvalidatesOnlyRosterMutators(t *testing.T) {
	cases := []struct {
		label string
		tool  string
		args  string
		res   domain.ToolResult
		want  bool
	}{
		{"close ok", "terminal.close", "{}", domain.Ok("closed", map[string]any{"closed": []string{"t"}}), true},
		{"close with unrecognized payload still reconciles", "terminal.close", "{}", domain.Ok("closed", nil), true},
		{"close with only empty reported ids still reconciles", "terminal.close", "{}", domain.Ok("closed", map[string]any{"closed": []string{""}}), true},
		{"spawn ok", "agentTask.spawnForEdits", "{}", domain.Ok("spawned", nil), true},
		{"spawn failure may have launched", "agentTask.spawnForEdits", "{}", domain.Fail("AGENT_LAUNCH_AMBIGUOUS", "ambiguous"), true},
		{"startWorkOnIssue ok", "workflow.startWorkOnIssue", "{}", domain.Ok("started", nil), true},
		{"recipe.run ok", "recipe.run", "{}", domain.Ok("ran", nil), true},
		{"worktree.createWithRecipe ok", "worktree.createWithRecipe", "{}", domain.Ok("created", nil), true},
		{"prepBranchForReview is read-only", "workflow.prepBranchForReview", "{}", domain.Ok("ready", nil), false},
		{"daintree.call inner terminal mutation", "daintree.call", `{"name":"terminal.kill","arguments":{}}`, domain.Ok("ok", nil), true},
		{"daintree.call inner read tool", "daintree.call", `{"name":"actions.getContext"}`, domain.Ok("ok", nil), false},
		{"daintree.call failed never ran", "daintree.call", `{"name":"terminal.new"}`, domain.Fail("mcp_tool_error", "nope"), false},
		// A read-only unwrapped terminal.* raw call (e.g. terminal.list) is a KNOWN,
		// accepted false positive: a spare refresh is harmless, a missed mutation is not.
		{"daintree.call read-only terminal tool (accepted false positive)", "daintree.call", `{"name":"terminal.list"}`, domain.Ok("ok", nil), true},
		{"daintree.call malformed args", "daintree.call", `{not json`, domain.Ok("ok", nil), false},
		{"daintree.call non-string name", "daintree.call", `{"name":7}`, domain.Ok("ok", nil), false},
		{"unrelated read tool", "fs.read", "{}", domain.Ok("ok", nil), false},
	}
	for _, tc := range cases {
		deps, _ := recordingDeps(&fakeRouter{}, &fakeTools{})
		deps.OpenTerminalsFetcher = nil // the kick no-ops; only the gen bump is observed
		s := NewSession(deps)

		s.rosterMu.Lock()
		before := s.rosterGen
		s.rosterMu.Unlock()
		s.observeRosterMutation(tc.tool, tc.args, tc.res)
		s.rosterMu.Lock()
		bumped := s.rosterGen != before
		s.rosterMu.Unlock()

		if bumped != tc.want {
			t.Errorf("%s: rosterGen bumped = %v, want %v", tc.label, bumped, tc.want)
		}
	}
}

// TestClosedTerminalIDs_Shapes pins the extraction across every result shape a
// terminal.close can produce: a clean success (Result), a partial failure (the ids in
// Error.Details DID close), a JSON-roundtripped []any payload, and the unrecognized
// shapes that must yield nil rather than guess.
func TestClosedTerminalIDs_Shapes(t *testing.T) {
	cases := []struct {
		name string
		res  domain.ToolResult
		want []string
	}{
		{"ok with []string", domain.Ok("closed", map[string]any{"closed": []string{"a", "b"}}), []string{"a", "b"}},
		{"native []string drops empty entries", domain.Ok("closed", map[string]any{"closed": []string{"a", "", "b"}}), []string{"a", "b"}},
		{"all-empty []string yields none", domain.Ok("closed", map[string]any{"closed": []string{""}}), nil},
		{"partial failure carries details", domain.Fail("mcp_tool_error", "partial",
			domain.WithDetails(map[string]any{"closed": []string{"a"}, "failed": []string{"b"}})), []string{"a"}},
		{"json-roundtripped []any", domain.Ok("closed", map[string]any{"closed": []any{"a", "", "b"}}), []string{"a", "b"}},
		{"ok with nil result", domain.Ok("closed", nil), nil},
		{"failure without details", domain.Fail("cancelled", "cancelled"), nil},
		{"unrecognized closed type", domain.Ok("closed", map[string]any{"closed": "a"}), nil},
	}
	for _, tc := range cases {
		got := closedTerminalIDs(tc.res)
		if len(got) != len(tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
				break
			}
		}
	}
}

// TestOpenTerminals_NilFetcherOmitsInventory: a nil fetcher (the default and the non-MCP
// path) simply omits the inventory — no panic, the runtime block's OpenTerminals stays
// empty — and DrainBackgroundWork is a no-op (nothing was spawned).
func TestOpenTerminals_NilFetcherOmitsInventory(t *testing.T) {
	r := &injectRouter{} // empty results ⇒ a single final round
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.OpenTerminalsFetcher = nil
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) == 0 {
		t.Fatal("want at least one recorded request")
	}
	if got := be.runtimeAt(0).OpenTerminals; len(got) != 0 {
		t.Fatalf("a nil fetcher must omit the inventory, got %+v", got)
	}
	s.DrainBackgroundWork()
}
