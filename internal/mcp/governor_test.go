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

// TestGovernorConcurrencyCap proves at most maxConcurrent acquires are held at
// once: 6 workers race through a cap-2 governor while tracking the high-water
// mark of concurrently-held slots.
func TestGovernorConcurrencyCap(t *testing.T) {
	g := newGovernor(2, 0)
	var inFlight, maxSeen int64
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.acquire(context.Background()); err != nil {
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
			g.release()
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&maxSeen); got > 2 {
		t.Fatalf("observed %d concurrent holders; cap is 2", got)
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
			if err := g.acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			times[i] = time.Now()
			g.release()
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

// TestGovernorAcquireAbortsWhileQueuedForSlot proves a caller cancelled while
// waiting for a slot returns ctx.Err() and does not leak capacity.
func TestGovernorAcquireAbortsWhileQueuedForSlot(t *testing.T) {
	g := newGovernor(1, 0)
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("queued acquire err = %v; want context.Canceled", err)
	}
	g.release()
	// The slot released above must be immediately acquirable — the aborted
	// waiter took nothing.
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("post-abort acquire: %v", err)
	}
	g.release()
}

// TestGovernorAbortDuringPacingReleasesSlot proves a caller cancelled during the
// pacing sleep gives its slot back (the semaphore drains to empty).
func TestGovernorAbortDuringPacingReleasesSlot(t *testing.T) {
	g := newGovernor(1, time.Second)
	// Prime the pacer so the next acquire must sleep ~1s.
	if err := g.acquire(context.Background()); err != nil {
		t.Fatalf("prime acquire: %v", err)
	}
	g.release()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := g.acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("pacing-sleep acquire err = %v; want context.DeadlineExceeded", err)
	}
	if len(g.slots) != 0 {
		t.Fatalf("slot leaked: %d held after aborted acquire", len(g.slots))
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
// cap across concurrent callers: 8 parallel calls against a cap-2 governor never
// exceed 2 simultaneous low-level calls.
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
			_, _ = c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{})
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
