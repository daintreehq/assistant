package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
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
