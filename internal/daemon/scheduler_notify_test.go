package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Per-job notification delivery: a completed job's attention must be delivered as
// soon as THAT job settles, not parked behind an unrelated slow sibling until the
// whole tick group finishes (the old end-of-tick-only notify let one wedged
// watcher delay a completed timer's wake by minutes).

func TestScheduler_FastJobNotifyNotDelayedBySlowSibling(t *testing.T) {
	store := newFakeStore()
	store.timers = []domain.TimerRecord{
		{ID: "tmr_slow", Title: "Slow", FireAt: 0, Status: "scheduled",
			PayloadType: "call_safe_tool",
			PayloadJson: `{"type":"call_safe_tool","toolCall":{"toolName":"x","args":{}}}`},
		{ID: "tmr_fast", Title: "Fast", FireAt: 0, Status: "scheduled",
			PayloadType: "enqueue", PayloadJson: `{"type":"enqueue","message":"ping"}`},
	}
	queue := newFakeQueue()
	queue.autoDigest = true
	// Reuses scheduler_test.go's blockingRegistry: the slow timer's dispatch parks
	// until release is closed, modeling a wedged sibling inside its deadline.
	release := make(chan struct{})
	reg := &blockingRegistry{enter: func() { <-release }}

	notified := make(chan domain.QueueEvent, 16)
	s := NewScheduler(SchedulerDeps{
		Store: store, Queue: queue, Registry: reg,
		OnAttention: func(evs []domain.QueueEvent) {
			for _, e := range evs {
				notified <- e
			}
		},
	})

	tickDone := make(chan struct{})
	go func() {
		s.Tick(context.Background(), 100)
		close(tickDone)
	}()

	// The fast timer's attention must arrive WHILE the slow job is still parked.
	select {
	case e := <-notified:
		if e.Title != "Fast" {
			t.Fatalf("first delivery = %q, want the fast timer's event", e.Title)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fast job's attention was delayed behind the slow sibling")
	}
	select {
	case <-tickDone:
		t.Fatal("tick finished before the slow job was released — assertion raced")
	default:
	}

	close(release)
	select {
	case <-tickDone:
	case <-time.After(2 * time.Second):
		t.Fatal("tick did not finish after the slow job released")
	}

	// Exactly-once: the per-job delivery plus the end-of-tick backstop must not
	// double-deliver the fast event (notify marks-notified under notifyMu).
	fastCount := 1
	for {
		select {
		case e := <-notified:
			if e.Title == "Fast" {
				fastCount++
			}
			continue
		default:
		}
		break
	}
	if fastCount != 1 {
		t.Fatalf("fast event delivered %d times, want exactly once", fastCount)
	}
}

// requestNotify coalescing: N near-simultaneous requests collapse (one active
// runner + at most one pending re-run) while never losing a request — every event
// published before a request is delivered, exactly once.
func TestScheduler_RequestNotifyCoalescesAndLosesNothing(t *testing.T) {
	queue := newFakeQueue()
	var mu sync.Mutex
	var delivered []string
	s := NewScheduler(SchedulerDeps{
		Store: newFakeStore(), Queue: queue, Registry: &fakeRegistry{},
		OnAttention: func(evs []domain.QueueEvent) {
			mu.Lock()
			for _, e := range evs {
				delivered = append(delivered, e.ID)
			}
			mu.Unlock()
		},
	})

	queue.autoDigest = true
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = queue.Publish(domain.QueuePublishArgs{
				Severity: domain.SeverityAttention, Title: "T", Summary: "s",
			})
			s.requestNotify()
		}()
	}
	wg.Wait()
	// One final pass covers any publish that raced the last in-flight Digest.
	s.requestNotify()

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != n {
		t.Fatalf("delivered %d events, want %d (each exactly once)", len(delivered), n)
	}
	seen := map[string]bool{}
	for _, id := range delivered {
		if seen[id] {
			t.Fatalf("event %s delivered twice", id)
		}
		seen[id] = true
	}
}
