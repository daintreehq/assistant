package mcp

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func interactiveCtx() context.Context {
	return WithPriority(context.Background(), PriorityInteractive)
}

func backgroundCtx() context.Context {
	return WithPriority(context.Background(), PriorityBackground)
}

// TestPriorityFromContextDefault proves untagged callers land on the Refresh
// middle ground, and tagged ones round-trip.
func TestPriorityFromContextDefault(t *testing.T) {
	if got := priorityFromContext(context.Background()); got != PriorityRefresh {
		t.Fatalf("untagged ctx class = %v; want PriorityRefresh", got)
	}
	if got := priorityFromContext(interactiveCtx()); got != PriorityInteractive {
		t.Fatalf("tagged ctx class = %v; want PriorityInteractive", got)
	}
	if got := priorityFromContext(WithPriority(context.Background(), Priority(99))); got != PriorityRefresh {
		t.Fatalf("out-of-range class = %v; want PriorityRefresh", got)
	}
}

// TestGovernorConcurrencyCap proves at most maxConcurrent acquires are held at
// once: 6 workers race through a cap-2 governor while tracking the high-water
// mark of concurrently-held slots. Interactive class so the full cap (reserved
// slot included) is exercised.
func TestGovernorConcurrencyCap(t *testing.T) {
	g := newGovernor(2, 0)
	var inFlight, maxSeen int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.acquire(interactiveCtx())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := atomic.AddInt64(&inFlight, 1)
			for {
				m := atomic.LoadInt64(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt64(&maxSeen, m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			rel()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&maxSeen); got > 2 {
		t.Fatalf("observed %d concurrent holders; cap is 2", got)
	}
}

// TestGovernorSharedCapExcludesReserved proves non-Interactive traffic can
// never occupy the reserved slot: 8 default-class (Refresh) workers through a
// cap-4 governor top out at 3 concurrent holders.
func TestGovernorSharedCapExcludesReserved(t *testing.T) {
	g := newGovernor(4, 0)
	var inFlight, maxSeen int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := g.acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			n := atomic.AddInt64(&inFlight, 1)
			for {
				m := atomic.LoadInt64(&maxSeen)
				if n <= m || atomic.CompareAndSwapInt64(&maxSeen, m, n) {
					break
				}
			}
			time.Sleep(20 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
			rel()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&maxSeen); got > 3 {
		t.Fatalf("observed %d concurrent non-Interactive holders; shared cap is 3", got)
	}
}

// TestGovernorPacingSpacesStarts proves consecutive acquire grants are spaced by
// at least the pacing interval even when slots are plentiful (cap ≥ callers):
// four concurrent acquires against a 30ms interval must start ≥ ~30ms apart.
func TestGovernorPacingSpacesStarts(t *testing.T) {
	const interval = 30 * time.Millisecond
	g := newGovernor(4, interval)
	times := make([]time.Time, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rel, err := g.acquire(context.Background())
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			times[i] = time.Now()
			rel()
		}(i)
	}
	wg.Wait()
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	for i := 1; i < len(times); i++ {
		// Allow generous scheduling slop below the nominal interval, but a gap
		// under half the interval means two starts collapsed onto one pacing tick.
		if gap := times[i].Sub(times[i-1]); gap < interval/2 {
			t.Fatalf("starts %d and %d only %v apart; want ≥ %v", i-1, i, gap, interval)
		}
	}
}

// TestGovernorInteractiveSkipsPacing proves Interactive acquires bypass the
// min-interval pacer: with a primed pacer that would delay a Refresh start by
// ~300ms, an Interactive acquire returns near-instantly.
func TestGovernorInteractiveSkipsPacing(t *testing.T) {
	const interval = 300 * time.Millisecond
	g := newGovernor(4, interval)
	// Prime the pacer: the first (Refresh) acquire starts immediately but books
	// the next paced start ~300ms out.
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("prime acquire: %v", err)
	}
	rel()

	start := time.Now()
	reli, err := g.acquire(interactiveCtx())
	if err != nil {
		t.Fatalf("interactive acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed > interval/2 {
		t.Fatalf("interactive acquire took %v; want well under the %v pacing interval", elapsed, interval)
	}
	reli()

	// Contrast: a Refresh acquire right now still owes the pacer its tick — the
	// Interactive call must not have consumed (or reset) it.
	start = time.Now()
	relr, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("refresh acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed < interval/3 {
		t.Fatalf("refresh acquire returned in %v; want a paced wait of roughly %v", elapsed, interval)
	}
	relr()
}

// TestGovernorReservedSlotForInteractive proves the reserved slot: with the
// shared capacity (3 of 4) fully held by Background, one more Background caller
// queues, but an Interactive acquire succeeds promptly.
func TestGovernorReservedSlotForInteractive(t *testing.T) {
	g := newGovernor(4, 0)
	rels := make([]func(), 0, 3)
	for i := 0; i < 3; i++ {
		rel, err := g.acquire(backgroundCtx())
		if err != nil {
			t.Fatalf("background acquire %d: %v", i, err)
		}
		rels = append(rels, rel)
	}

	// A 4th Background caller must queue: the remaining slot is Interactive-only.
	bgGranted := make(chan func(), 1)
	go func() {
		rel, err := g.acquire(backgroundCtx())
		if err != nil {
			return
		}
		bgGranted <- rel
	}()
	waitFor(t, func() bool { return g.queuedCount(PriorityBackground) == 1 })
	select {
	case rel := <-bgGranted:
		rel()
		t.Fatal("4th Background acquire took the reserved slot")
	case <-time.After(50 * time.Millisecond):
	}

	// Interactive walks straight into the reserved slot.
	done := make(chan func(), 1)
	go func() {
		rel, err := g.acquire(interactiveCtx())
		if err != nil {
			t.Errorf("interactive acquire: %v", err)
			return
		}
		done <- rel
	}()
	select {
	case rel := <-done:
		rel()
	case <-time.After(2 * time.Second):
		t.Fatal("Interactive acquire did not get the reserved slot promptly")
	}

	for _, rel := range rels {
		rel()
	}
	// Drain the queued Background waiter (granted once shared capacity freed).
	select {
	case rel := <-bgGranted:
		rel()
	case <-time.After(2 * time.Second):
		t.Fatal("queued Background waiter never granted after release")
	}
}

// TestGovernorInteractiveQueueJumpsSharedQueue proves grant ordering on a shared
// slot: with Background and Refresh already queued, a later-arriving Interactive
// waiter is granted first (then Refresh, then Background).
func TestGovernorInteractiveQueueJumpsSharedQueue(t *testing.T) {
	g := newGovernor(1, 0) // cap 1 ⇒ no reserved slot; ordering alone decides.
	hold, err := g.acquire(backgroundCtx())
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	order := make(chan string, 3)
	spawn := func(name string, ctx context.Context, class Priority, queued int) {
		go func() {
			rel, err := g.acquire(ctx)
			if err != nil {
				t.Errorf("%s acquire: %v", name, err)
				return
			}
			order <- name
			rel()
		}()
		waitFor(t, func() bool { return g.queuedCount(class) == queued })
	}
	spawn("background", backgroundCtx(), PriorityBackground, 1)
	spawn("refresh", context.Background(), PriorityRefresh, 1)
	spawn("interactive", interactiveCtx(), PriorityInteractive, 1)

	hold() // release the held slot: the cascade should run interactive → refresh → background
	want := []string{"interactive", "refresh", "background"}
	for i, w := range want {
		select {
		case got := <-order:
			if got != w {
				t.Fatalf("grant %d = %q; want %q", i, got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("grant %d (%q) never arrived", i, w)
		}
	}
}

// TestGovernorBackgroundAntiStarvation proves the aging rule: a Background
// waiter queued past governorStarvationAge takes the next shared grant even
// though an Interactive waiter is also queued.
func TestGovernorBackgroundAntiStarvation(t *testing.T) {
	oldAge := governorStarvationAge
	governorStarvationAge = 30 * time.Millisecond
	defer func() { governorStarvationAge = oldAge }()

	g := newGovernor(2, 0) // capTotal 2, capShared 1
	relA, err := g.acquire(interactiveCtx())
	if err != nil {
		t.Fatalf("interactive holder A: %v", err)
	}
	relB, err := g.acquire(interactiveCtx())
	if err != nil {
		t.Fatalf("interactive holder B: %v", err)
	}

	order := make(chan string, 2)
	go func() {
		rel, err := g.acquire(backgroundCtx())
		if err != nil {
			t.Errorf("background acquire: %v", err)
			return
		}
		order <- "background"
		rel()
	}()
	waitFor(t, func() bool { return g.queuedCount(PriorityBackground) == 1 })

	go func() {
		rel, err := g.acquire(interactiveCtx())
		if err != nil {
			t.Errorf("interactive waiter: %v", err)
			return
		}
		order <- "interactive"
		rel()
	}()
	waitFor(t, func() bool { return g.queuedCount(PriorityInteractive) == 1 })

	// Age the Background waiter past the starvation threshold, then free one
	// slot. The aged Background waiter must beat the queued Interactive one.
	time.Sleep(3 * governorStarvationAge)
	relA()

	want := []string{"background", "interactive"}
	for i, w := range want {
		select {
		case got := <-order:
			if got != w {
				t.Fatalf("grant %d = %q; want %q (anti-starvation)", i, got, w)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("grant %d (%q) never arrived", i, w)
		}
	}
	relB()
}

// TestGovernorAgedBacklogCannotStarveInteractive proves the alternation guard on
// the anti-starvation override: with the reserved slot pinned by a long
// Interactive call and a SUSTAINED aged-Background backlog, a queued Interactive
// waiter must be granted within two shared grants — the aged backlog may win one
// override, but never two consecutive shared grants while Interactive waits.
func TestGovernorAgedBacklogCannotStarveInteractive(t *testing.T) {
	oldAge := governorStarvationAge
	governorStarvationAge = 30 * time.Millisecond
	defer func() { governorStarvationAge = oldAge }()

	g := newGovernor(2, 0) // capTotal 2, capShared 1
	// A long Interactive call pins the reserved slot for the whole test.
	relLong, err := g.acquire(interactiveCtx())
	if err != nil {
		t.Fatalf("long interactive holder: %v", err)
	}
	// A Background call holds the single shared slot.
	relShared, err := g.acquire(backgroundCtx())
	if err != nil {
		t.Fatalf("background holder: %v", err)
	}

	// Sustained backlog: several Background waiters queue up (they will all age
	// past the starvation threshold), plus one Interactive waiter.
	order := make(chan string, 8)
	const backlog = 4
	for i := 0; i < backlog; i++ {
		go func() {
			rel, err := g.acquire(backgroundCtx())
			if err != nil {
				t.Errorf("backlog acquire: %v", err)
				return
			}
			order <- "background"
			rel()
		}()
	}
	waitFor(t, func() bool { return g.queuedCount(PriorityBackground) == backlog })

	go func() {
		rel, err := g.acquire(interactiveCtx())
		if err != nil {
			t.Errorf("interactive waiter: %v", err)
			return
		}
		order <- "interactive"
		rel()
	}()
	waitFor(t, func() bool { return g.queuedCount(PriorityInteractive) == 1 })

	// Age the whole Background backlog past the threshold, then free the shared
	// slot. Every released Background grant re-queues nothing, but each release
	// hands the shared slot onward — the backlog stays aged throughout, so
	// WITHOUT the alternation guard the overrides would win every shared grant
	// and the Interactive waiter would only run after all four.
	time.Sleep(3 * governorStarvationAge)
	relShared()

	first := <-order
	second := <-order
	if first != "background" {
		t.Fatalf("first grant = %q; want the aged background override", first)
	}
	if second != "interactive" {
		t.Fatalf("second grant = %q; want interactive within 2 grants (alternation guard)", second)
	}
	// Drain the rest.
	for i := 0; i < backlog-1; i++ {
		select {
		case <-order:
		case <-time.After(2 * time.Second):
			t.Fatal("backlog waiter never granted")
		}
	}
	relLong()
}

// TestGovernorCancelledPacingReclaimsReservation proves a paced waiter that
// aborts before its start tick hands the reservation back: N sequential
// cancelled paced acquires must not delay the next real acquire by more than one
// pacing interval.
func TestGovernorCancelledPacingReclaimsReservation(t *testing.T) {
	const interval = 100 * time.Millisecond
	g := newGovernor(4, interval)
	// Prime the pacer so every subsequent acquire owes a paced wait.
	rel, err := g.acquire(backgroundCtx())
	if err != nil {
		t.Fatalf("prime acquire: %v", err)
	}
	rel()
	baseline := func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.nextStartAt
	}()

	// N cancelled paced acquires, sequentially (each reserves a tick, sleeps,
	// aborts). Each must reclaim its reservation.
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(backgroundCtx(), 10*time.Millisecond)
		if _, err := g.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			t.Fatalf("cancelled paced acquire %d err = %v; want context.DeadlineExceeded", i, err)
		}
		cancel()
	}

	after := func() time.Time {
		g.mu.Lock()
		defer g.mu.Unlock()
		return g.nextStartAt
	}()
	// The pacer must not have advanced beyond one interval past where the primed
	// baseline left it — the cancelled reservations were reclaimed.
	if drift := after.Sub(baseline); drift > interval {
		t.Fatalf("cancelled paced acquires advanced nextStartAt by %v; want ≤ one %v interval", drift, interval)
	}

	// And the next REAL acquire waits at most ~one interval.
	start := time.Now()
	rel2, err := g.acquire(backgroundCtx())
	if err != nil {
		t.Fatalf("post-cancel acquire: %v", err)
	}
	if elapsed := time.Since(start); elapsed > interval+interval/2 {
		t.Fatalf("post-cancel acquire waited %v; want ≤ ~one %v interval", elapsed, interval)
	}
	rel2()
}

// TestGovernorAcquireAbortsWhileQueuedForSlot proves a caller cancelled while
// waiting for a slot returns ctx.Err() and does not leak capacity.
func TestGovernorAcquireAbortsWhileQueuedForSlot(t *testing.T) {
	g := newGovernor(1, 0)
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued acquire err = %v; want context.Canceled", err)
	}
	rel()
	// The slot released above must be immediately acquirable — the aborted
	// waiter took nothing.
	rel2, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("post-abort acquire: %v", err)
	}
	rel2()
}

// TestGovernorAbortWhileQueuedBehindHolder proves a waiter that entered the
// queue (slot busy) and is then cancelled unblocks with ctx.Err(), leaves the
// queue, and the eventually-freed slot goes to the next real waiter.
func TestGovernorAbortWhileQueuedBehindHolder(t *testing.T) {
	g := newGovernor(1, 0)
	hold, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := g.acquire(ctx)
		errCh <- err
	}()
	waitFor(t, func() bool { return g.queuedCount(PriorityRefresh) == 1 })
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued acquire err = %v; want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled waiter never unblocked")
	}
	if got := g.queuedCount(PriorityRefresh); got != 0 {
		t.Fatalf("cancelled waiter still queued (%d)", got)
	}
	hold()
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("post-abort acquire: %v", err)
	}
	rel()
}

// TestGovernorAbortDuringPacingReleasesSlot proves a caller cancelled during the
// pacing sleep gives its slot back (held count drains to zero).
func TestGovernorAbortDuringPacingReleasesSlot(t *testing.T) {
	g := newGovernor(1, time.Second)
	// Prime the pacer so the next acquire must sleep ~1s.
	rel, err := g.acquire(context.Background())
	if err != nil {
		t.Fatalf("prime acquire: %v", err)
	}
	rel()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := g.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pacing-sleep acquire err = %v; want context.DeadlineExceeded", err)
	}
	if got := g.heldCount(); got != 0 {
		t.Fatalf("slot leaked: %d held after aborted acquire", got)
	}
}

// waitFor polls cond until true or fails the test after 2s.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition never became true")
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingLow is a LowLevelClient whose CallTool parks until released, tracking
// the high-water mark of concurrent in-flight calls — the integration probe
// that CallTool actually routes through the governor.
type blockingLow struct {
	fakeLow
	inFlight int64
	maxSeen  int64
	gate     chan struct{}
}

func (b *blockingLow) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	n := atomic.AddInt64(&b.inFlight, 1)
	for {
		m := atomic.LoadInt64(&b.maxSeen)
		if n <= m || atomic.CompareAndSwapInt64(&b.maxSeen, m, n) {
			break
		}
	}
	<-b.gate
	atomic.AddInt64(&b.inFlight, -1)
	return rawResult{Text: "ok"}, nil
}

// TestCallToolGovernedConcurrency proves Client.CallTool enforces the in-flight
// cap across concurrent callers: 8 parallel Interactive calls against a cap-2
// governor never exceed 2 simultaneous low-level calls.
func TestCallToolGovernedConcurrency(t *testing.T) {
	low := &blockingLow{gate: make(chan struct{})}
	c := newInjected(low)
	c.gov = newGovernor(2, 0)

	const callers = 8
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.CallTool(interactiveCtx(), "terminal.getStatus", nil, CallOptions{})
		}()
	}
	// Let callers queue up, then check the cap held, then drain everyone.
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt64(&low.maxSeen); got > 2 {
		t.Fatalf("observed %d concurrent low-level calls; cap is 2", got)
	}
	close(low.gate)
	wg.Wait()
	if got := atomic.LoadInt64(&low.maxSeen); got > 2 {
		t.Fatalf("observed %d concurrent low-level calls after drain; cap is 2", got)
	}
}

// panicLow panics on the first CallTool and succeeds afterwards — the probe
// that a panicking SDK call cannot leak a governor slot.
type panicLow struct {
	fakeLow
	panics int32
}

func (p *panicLow) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	if atomic.AddInt32(&p.panics, 1) == 1 {
		panic("simulated SDK panic")
	}
	return rawResult{Text: "ok"}, nil
}

// TestCallToolPanicDoesNotLeakGovernorSlot proves the slot is released even when
// the low-level call panics: with a cap-1 governor, a follow-up call after a
// panicking one must still get through (a leaked slot would block it forever).
func TestCallToolPanicDoesNotLeakGovernorSlot(t *testing.T) {
	low := &panicLow{}
	c := newInjected(low)
	c.gov = newGovernor(1, 0)

	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		_, _ = c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{})
	}()
	if !panicked {
		t.Fatal("the first call was expected to panic")
	}

	done := make(chan struct{})
	go func() {
		_, _ = c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("governor slot leaked: the call after the panic never ran")
	}
}

// TestCallToolGovernorAbortNoDegrade proves a caller cancelled while queued gets
// its ctx error back WITHOUT degrading the connection — the transport was never
// touched, so connectivity is not evidence-backed either way.
func TestCallToolGovernorAbortNoDegrade(t *testing.T) {
	low := &blockingLow{gate: make(chan struct{})}
	defer close(low.gate)
	c := newInjected(low)
	c.gov = newGovernor(1, 0)

	// Occupy the only slot.
	started := make(chan struct{})
	go func() {
		close(started)
		_, _ = c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{})
	}()
	<-started
	// Wait until the first call actually holds the slot (is in flight).
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt64(&low.inFlight) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first call never reached the low-level client")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.CallTool(ctx, "terminal.getStatus", nil, CallOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("queued CallTool err = %v; want context.Canceled", err)
	}
	if !c.IsConnected() {
		t.Fatal("queued-abort must not degrade the connection")
	}
}
