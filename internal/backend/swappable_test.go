package backend

import (
	"context"
	"sync"
	"testing"
)

// stubBackend is a Backend that only reports which one it is.
type stubBackend struct{ name string }

func (s stubBackend) RespondStream(context.Context, RespondRequest, StreamCallbacks) (RespondResult, error) {
	return RespondResult{}, nil
}
func (s stubBackend) RunTask(context.Context, TaskRequest) (TaskResult, error) {
	return TaskResult{}, nil
}
func (s stubBackend) VerifyKey(context.Context) (KeyVerification, error) {
	return KeyVerification{Valid: true, Label: s.name}, nil
}

func (s stubBackend) Capabilities(context.Context) (Capabilities, error) {
	return Capabilities{ServerVersion: s.name}, nil
}
func (s stubBackend) Version(context.Context) (Version, error) { return Version{}, nil }
func (s stubBackend) Health(context.Context) error             { return nil }
func (s stubBackend) Ready(context.Context) error              { return nil }
func (s stubBackend) BaseURL() string                          { return s.name }

func TestSwappableDelegatesAndSwaps(t *testing.T) {
	s := NewSwappable(stubBackend{name: "first"})
	if got := s.BaseURL(); got != "first" {
		t.Fatalf("BaseURL before swap = %q, want first", got)
	}

	prev := s.Swap(stubBackend{name: "second"})
	if prev.BaseURL() != "first" {
		t.Fatalf("Swap returned %q, want the previous delegate", prev.BaseURL())
	}
	if got := s.BaseURL(); got != "second" {
		t.Fatalf("BaseURL after swap = %q, want second", got)
	}
	// Every method must follow the swap, not just the one the sheet happens to use.
	caps, err := s.Capabilities(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if caps.ServerVersion != "second" {
		t.Fatalf("Capabilities went to %q, want second", caps.ServerVersion)
	}
}

// The whole point of the wrapper: /login swaps the client while the scheduler tick, the
// async coordinator and possibly a wake turn are calling through it on other goroutines.
// Under -race this fails loudly if the delegate is not properly synchronized.
func TestSwappableIsRaceFreeUnderConcurrentSwap(t *testing.T) {
	s := NewSwappable(stubBackend{name: "initial"})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					// A reader must never observe a torn or nil delegate.
					if s.BaseURL() == "" {
						t.Error("observed an empty delegate mid-swap")
						return
					}
					_ = s.Health(context.Background())
				}
			}
		}()
	}

	for i := range 200 {
		s.Swap(stubBackend{name: string(rune('a' + i%26))})
	}
	close(stop)
	wg.Wait()
}

// A Swappable must be usable anywhere a Backend is, including wrapped in another one —
// app.Create wraps a test's BackendOverride the same way it wraps a real client.
func TestSwappableSatisfiesBackend(t *testing.T) {
	var b Backend = NewSwappable(NewSwappable(stubBackend{name: "nested"}))
	if b.BaseURL() != "nested" {
		t.Fatalf("nested delegate = %q, want nested", b.BaseURL())
	}
}
