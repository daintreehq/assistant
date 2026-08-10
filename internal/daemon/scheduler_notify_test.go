package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
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

// Finding: an event materially updated by its publisher BETWEEN the notify
// pass's Digest read and its MarkNotified acknowledgement must not be stamped
// notified with the update undelivered. The ack is version-conditional, so the
// stale mark fails, the row stays unnotified, and the next notify request
// delivers the updated content.
func TestScheduler_PublishUpdateRacingNotifyIsRedelivered(t *testing.T) {
	queue := newFakeQueue()
	created := int64(100)
	queue.digest = []domain.QueueEvent{{
		ID: "evt_1", Severity: domain.SeverityAttention,
		Title: "watcher: waiting", Summary: "v1", Count: 1, CreatedAt: created,
	}}

	var mu sync.Mutex
	var delivered []string
	bumped := false
	s := NewScheduler(SchedulerDeps{Store: newFakeStore(), Queue: queue, Registry: &fakeRegistry{}})
	s.SetOnAttention(func(evs []domain.QueueEvent) {
		mu.Lock()
		for _, e := range evs {
			delivered = append(delivered, e.Summary)
		}
		mu.Unlock()
		// The callback runs BETWEEN Digest and MarkNotified — exactly where a
		// publisher's dedupe update can land. Bump once, on the first delivery.
		if !bumped {
			bumped = true
			queue.bump("evt_1", "watcher: tests failed", "v2", 200)
		}
	})

	s.NotifyNow() // delivers v1; the ack races the bump and must NOT stick
	s.NotifyNow() // the pending rerun: must deliver the updated v2
	s.NotifyNow() // and then nothing further (v2's ack matched)

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 || delivered[0] != "v1" || delivered[1] != "v2" {
		t.Fatalf("delivered = %v; want [v1 v2] (update re-delivered exactly once)", delivered)
	}
}

// Finding: the per-pass digest is capped (one page), and coalescing collapses a
// burst of N requests into at most two passes — so without draining, a burst
// bigger than two pages strands its tail until an unrelated tick. notify() must
// loop while pages come back full: 45 events (> 2×20) through ONE request must
// all be delivered.
func TestScheduler_NotifyDrainsBurstBeyondOnePage(t *testing.T) {
	queue := newFakeQueue()
	for i := 0; i < 45; i++ {
		queue.digest = append(queue.digest, domain.QueueEvent{
			ID: fmt.Sprintf("evt_%02d", i), Severity: domain.SeverityAttention,
			Title: "T", Summary: "s", Count: 1, CreatedAt: int64(i),
		})
	}

	var mu sync.Mutex
	seen := map[string]int{}
	s := NewScheduler(SchedulerDeps{Store: newFakeStore(), Queue: queue, Registry: &fakeRegistry{}})
	s.SetOnAttention(func(evs []domain.QueueEvent) {
		mu.Lock()
		for _, e := range evs {
			seen[e.ID]++
		}
		mu.Unlock()
	})

	s.NotifyNow() // ONE request must drain the whole burst

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 45 {
		t.Fatalf("delivered %d distinct events, want all 45 in one drained pass", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("event %s delivered %d times, want exactly once", id, n)
		}
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
