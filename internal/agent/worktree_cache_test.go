package agent

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
	"github.com/daintreehq/daintree-assistant/internal/waitbudget"
)

// The current-worktree snapshot is served from a cross-turn cache and refreshed on a
// DETACHED goroutine (the open-terminal roster pattern), so a model round never blocks
// on the worktree.getCurrent MCP read that used to sit synchronously on every round's
// first-byte path. These tests pin that contract: non-blocking, cached, TTL-refreshed,
// and a bounded cold-start grace.

// waitForWorktreeIdle blocks until no worktree refresh is in flight AND at least one
// has completed — the point where the cache is stable for deterministic assertions.
func waitForWorktreeIdle(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.worktreeMu.Lock()
		settled := !s.worktreeRefreshing && !s.worktreeFetchedAt.IsZero()
		s.worktreeMu.Unlock()
		if settled {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("worktree refresh never settled")
}

// expireWorktreeCache resets the cache to COLD (never fetched), forcing the next
// consult to re-fetch — the test-time equivalent of worktreeSnapshotTTL elapsing.
func expireWorktreeCache(s *Session) {
	s.worktreeMu.Lock()
	s.worktreeSnap = nil
	s.worktreeFetchedAt = time.Time{}
	s.worktreeMu.Unlock()
}

// The marquee guarantee: with a fetcher that blocks indefinitely, Send must still
// finish the turn — the refresh runs detached, and the round proceeds WITHOUT worktree
// context (the cold cache waits only the short grace, then moves on), exactly what the
// old inline path produced when its read budget expired.
func TestWorktree_FetchDoesNotBlockModelRound(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(ctx context.Context) *prompts.WorktreeContext {
		entered <- struct{}{}
		<-release // simulate a hung/slow MCP
		return &prompts.WorktreeContext{Present: true, Branch: "slow/branch"}
	}
	s := NewSession(deps)

	done := make(chan string, 1)
	go func() {
		reply, _ := s.Send(context.Background(), "hi", SendOptions{})
		done <- reply
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worktree refresh was never kicked off")
	}

	// The turn must finish while the fetcher is still blocked on release.
	select {
	case reply := <-done:
		if reply != "ok" {
			t.Fatalf("reply = %q, want ok", reply)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked on the worktree fetch — the read must be detached")
	}

	// The cold round carried no worktree (the refresh is still in flight).
	if got := be.runtimeAt(0).Worktree; got != nil {
		t.Errorf("cold round should carry no worktree while the fetch hangs, got %+v", got)
	}

	close(release)
	s.DrainBackgroundWork()
}

// Cross-turn cache: turn 1's detached refresh populates the snapshot; turn 2 serves it
// with NO fetch of its own (the cache is younger than the TTL).
func TestWorktree_FreshCacheServedWithoutRefetch(t *testing.T) {
	var calls atomic.Int32
	r := &fakeRouter{results: []models.ChatResult{{Content: "a"}, {Content: "b"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		calls.Add(1)
		return &prompts.WorktreeContext{Present: true, Branch: "feature/cached"}
	}
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	waitForWorktreeIdle(t, s)
	if _, err := s.Send(context.Background(), "two", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork()

	if got := calls.Load(); got != 1 {
		t.Fatalf("fetcher entered %d times, want exactly 1 (turn 2 must serve the fresh cache)", got)
	}
	rc := be.runtimeAt(1)
	if rc.Worktree == nil || rc.Worktree.Current == nil || rc.Worktree.Current.Branch != "feature/cached" {
		t.Fatalf("turn 2 should serve the warmed snapshot, got %+v", rc.Worktree)
	}
}

// TTL refresh: a snapshot older than worktreeSnapshotTTL at consult time triggers a
// detached re-fetch — while the round itself still proceeds on the OLD cached value
// (the refresh is for the NEXT consumer, never a blocking read for this one).
func TestWorktree_TTLExpiryKicksDetachedRefresh(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 1)

	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(ctx context.Context) *prompts.WorktreeContext {
		entered <- struct{}{}
		<-release
		return &prompts.WorktreeContext{Present: true, Branch: "feature/new"}
	}
	s := NewSession(deps)

	// Seed an AGED cache: a valid snapshot whose fetch timestamp has outlived the TTL.
	s.worktreeMu.Lock()
	s.worktreeSnap = &prompts.WorktreeContext{Present: true, Branch: "feature/old"}
	s.worktreeFetchedAt = time.Now().Add(-worktreeSnapshotTTL - time.Second)
	s.worktreeMu.Unlock()

	if _, err := s.Send(context.Background(), "work", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	// The stale consult must have kicked the refresh…
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("a stale cache at consult time must kick a detached refresh")
	}
	// …while the round itself was served the OLD snapshot (fetch still blocked).
	rc := be.runtimeAt(0)
	if rc.Worktree == nil || rc.Worktree.Current == nil || rc.Worktree.Current.Branch != "feature/old" {
		t.Fatalf("the round must proceed on the stale cached value, got %+v", rc.Worktree)
	}

	close(release)
	s.DrainBackgroundWork()
	// The landed refresh replaces the cache for the next consumer.
	s.worktreeMu.Lock()
	got := s.worktreeSnap
	s.worktreeMu.Unlock()
	if got == nil || got.Branch != "feature/new" {
		t.Fatalf("the landed refresh should replace the cache, got %+v", got)
	}
}

// A cache YOUNGER than the TTL kicks nothing: the turn-start warm and the per-round
// consult are both TTL-gated, so back-to-back turns pay zero MCP reads.
func TestWorktree_YoungCacheKicksNoRefresh(t *testing.T) {
	var calls atomic.Int32
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	deps, be := recordingDeps(r, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		calls.Add(1)
		return &prompts.WorktreeContext{Present: true, Branch: "feature/should-not-fetch"}
	}
	s := NewSession(deps)

	s.worktreeMu.Lock()
	s.worktreeSnap = &prompts.WorktreeContext{Present: true, Branch: "feature/young"}
	s.worktreeFetchedAt = time.Now()
	s.worktreeMu.Unlock()

	if _, err := s.Send(context.Background(), "work", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork()
	if got := calls.Load(); got != 0 {
		t.Fatalf("a young cache must not re-fetch, fetcher entered %d times", got)
	}
	rc := be.runtimeAt(0)
	if rc.Worktree == nil || rc.Worktree.Current == nil || rc.Worktree.Current.Branch != "feature/young" {
		t.Fatalf("the round should serve the young cache, got %+v", rc.Worktree)
	}
}

// The cold-cache grace is a ONE-SHOT courtesy: while the FIRST fetch is still in
// flight, only the first consult waits worktreeFirstFetchGrace — once that grace
// fully elapses, every later consult of the still-cold cache proceeds immediately
// (nil worktree is fine). A degraded 5s MCP read must not cost 250ms per round.
func TestWorktree_ColdGracePaidOnceWhileFirstFetchInFlight(t *testing.T) {
	release := make(chan struct{})
	deps, _ := recordingDeps(&fakeRouter{}, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		<-release // the first fetch stays in flight for the whole test
		return &prompts.WorktreeContext{Present: true, Branch: "slow/branch"}
	}
	s := NewSession(deps)
	defer func() {
		close(release)
		s.DrainBackgroundWork()
	}()

	// First consult: pays the grace in full (the fetch never lands), latches.
	start := time.Now()
	if got := s.currentWorktreeContext(context.Background()); got != nil {
		t.Fatalf("cold consult must degrade to nil while the fetch hangs, got %+v", got)
	}
	if elapsed := time.Since(start); elapsed < worktreeFirstFetchGrace {
		t.Fatalf("the FIRST cold consult should wait the grace, returned after %v", elapsed)
	}
	s.worktreeMu.Lock()
	latched := s.worktreeGraceElapsed
	s.worktreeMu.Unlock()
	if !latched {
		t.Fatal("a fully-elapsed grace must latch worktreeGraceElapsed")
	}

	// Later consults: same cold cache, same in-flight fetch — NO further grace.
	for i := 0; i < 3; i++ {
		start = time.Now()
		if got := s.currentWorktreeContext(context.Background()); got != nil {
			t.Fatalf("round %d: still-cold consult must serve nil, got %+v", i, got)
		}
		if elapsed := time.Since(start); elapsed >= worktreeFirstFetchGrace {
			t.Fatalf("round %d: consult after the latch paid the grace again (%v)", i, elapsed)
		}
	}
}

// A grace the fetch BEATS does not latch: nothing was wasted, the cache is warm,
// and (test-only) a re-cooled cache may use the grace again — so the latch only
// ever suppresses waits that already proved useless.
func TestWorktree_GraceBeatenByFastFetchDoesNotLatch(t *testing.T) {
	deps, _ := recordingDeps(&fakeRouter{}, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		return &prompts.WorktreeContext{Present: true, Branch: "fast/branch"}
	}
	s := NewSession(deps)

	got := s.currentWorktreeContext(context.Background())
	if got == nil || got.Branch != "fast/branch" {
		t.Fatalf("a fast first fetch should land within the grace, got %+v", got)
	}
	s.worktreeMu.Lock()
	latched := s.worktreeGraceElapsed
	s.worktreeMu.Unlock()
	if latched {
		t.Fatal("a grace the fetch beat must not latch")
	}
	s.DrainBackgroundWork()
}

// WarmOpenTerminals (the splash/reconnect warm) fills the worktree cache too, so a
// fast first submit finds it already fetched.
func TestWorktree_WarmOpenTerminalsWarmsWorktreeCache(t *testing.T) {
	var calls atomic.Int32
	deps, _ := recordingDeps(&fakeRouter{}, &fakeTools{})
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		calls.Add(1)
		return &prompts.WorktreeContext{Present: true, Branch: "feature/warm"}
	}
	s := NewSession(deps)
	s.WarmOpenTerminals()
	waitForWorktreeIdle(t, s)
	s.worktreeMu.Lock()
	got := s.worktreeSnap
	s.worktreeMu.Unlock()
	if got == nil || got.Branch != "feature/warm" {
		t.Fatalf("splash warm should fill the worktree cache, got %+v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("warm should fetch exactly once, got %d", calls.Load())
	}
	s.DrainBackgroundWork()
}

// A nil fetcher (tests, non-MCP paths) stays a clean no-op end to end.
func TestWorktree_NilFetcherOmitsContext(t *testing.T) {
	deps, be := recordingDeps(&injectRouter{}, &fakeTools{})
	deps.CurrentWorktreeFetcher = nil
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := be.runtimeAt(0).Worktree; got != nil {
		t.Fatalf("nil fetcher must omit the worktree, got %+v", got)
	}
	s.DrainBackgroundWork()
}

// The turn context carries the per-turn cumulative foreground-wait budget into every
// tool dispatch — full (120s) at the first dispatch of a fresh turn, and minted anew
// per user turn.
func TestTurnContextCarriesForegroundWaitBudget(t *testing.T) {
	var seen atomic.Pointer[waitbudget.Budget]
	tr := &ctxCapturingTools{onDispatch: func(ctx context.Context) {
		seen.Store(waitbudget.From(ctx))
	}}
	tr.result = domain.Ok("ok", nil)
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
	}}
	deps, _ := recordingDeps(r, tr)
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "work", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	b := seen.Load()
	if b == nil {
		t.Fatal("tool dispatch ctx must carry the turn's wait budget")
	}
	if got := b.Remaining(); got != waitbudget.TurnBudget {
		t.Fatalf("fresh turn budget = %v, want %v", got, waitbudget.TurnBudget)
	}
	s.DrainBackgroundWork()
}

// ctxCapturingTools is a ToolRunner fake that exposes the dispatch ctx to the test.
type ctxCapturingTools struct {
	fakeTools
	onDispatch func(ctx context.Context)
}

func (t *ctxCapturingTools) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	if t.onDispatch != nil {
		t.onDispatch(ctx)
	}
	return t.fakeTools.Dispatch(ctx, name, args, turn)
}
