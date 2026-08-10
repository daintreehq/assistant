package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// slowStreamRouter blocks inside Stream (emitting tokens) long enough for other
// goroutines to hammer the session's state, then returns a final answer. It lets a
// `-race` run prove that the streaming turn and concurrent UI commands never touch
// the same fields without the lock.
type slowStreamRouter struct{ hold time.Duration }

func (r slowStreamRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	// Touch opts.Messages (the snapshot) the way the real client would, while other
	// goroutines mutate the live history — proving the snapshot is decoupled.
	_ = len(opts.Messages)
	deadline := time.Now().Add(r.hold)
	for time.Now().Before(deadline) {
		if onToken != nil {
			onToken("t")
		}
		time.Sleep(time.Millisecond)
	}
	return models.ChatResult{Content: "done"}, nil
}
func (slowStreamRouter) Chat(context.Context, domain.ModelTier, models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "s"}, nil
}
func (slowStreamRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }

// TestConcurrentTurnAndCommandsRace runs a turn while other goroutines call
// Messages/Artifacts/InjectNote (read/append) and Clear/Compact (mutating commands)
// concurrently. Under `go test -race` this must be clean: every access is under the
// session lock, the streaming turn reads an immutable snapshot, and the mutating
// commands return ErrTurnInProgress (they never mutate mid-turn) so the in-flight
// turn's history is never corrupted.
func TestConcurrentTurnAndCommandsRace(t *testing.T) {
	deps := baseDeps(slowStreamRouter{hold: 60 * time.Millisecond}, &fakeTools{})
	s := NewSession(deps)

	stop := make(chan struct{})
	var loopers sync.WaitGroup

	// Reader goroutines: Messages / Artifacts / InjectNote.
	for i := 0; i < 4; i++ {
		loopers.Add(1)
		go func() {
			defer loopers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = s.Messages()
				_ = s.Artifacts()
				s.InjectNote("note")
			}
		}()
	}

	// Mutating-command goroutines: while a turn is in flight these MUST return
	// ErrTurnInProgress and leave the history untouched (never corrupt the stream).
	sawInProgress := false
	var mu sync.Mutex
	for i := 0; i < 3; i++ {
		loopers.Add(1)
		go func() {
			defer loopers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if err := s.Clear(); err == ErrTurnInProgress {
					mu.Lock()
					sawInProgress = true
					mu.Unlock()
				}
				_ = s.Compact("x")
			}
		}()
	}

	// Run the turn to completion on THIS goroutine while the loopers hammer.
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatalf("Send error: %v", err)
	}
	close(stop)
	loopers.Wait()

	// The mutating commands raced the in-flight turn and were correctly rejected at
	// least once — proof the guard fired (not merely that nothing ran).
	mu.Lock()
	defer mu.Unlock()
	if !sawInProgress {
		t.Fatal("expected at least one ErrTurnInProgress during the in-flight turn")
	}
}
