package app

import (
	"context"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// hooks_race_test.go locks in CODE-REVIEW #7: AppHooks are read by agent/tool
// goroutines (the buildContext confirm/log closures and the eventProxy sink) while
// SetHooks is called from the UI's Update loop. With the RWMutex guard, a `-race`
// run over concurrent SetHooks + hook reads must be clean.

// TestAppHooks_ConcurrentSetAndRead_NoRace hammers SetHooks from one goroutine while
// other goroutines exercise every hook reader (Confirm/Log closures + the event
// proxy sink) against the same App. Run with `go test -race`.
func TestAppHooks_ConcurrentSetAndRead_NoRace(t *testing.T) {
	a := newOfflineApp(t)
	t.Cleanup(func() { _ = a.Shutdown() })

	// A reader exercising the confirm + log closures (built off live hooks).
	tctx := a.buildContext(domain.ActorMain, "")
	// The event proxy the session holds, reading the live sink each call.
	proxy := &eventProxy{app: a}

	var readers, writer sync.WaitGroup
	stop := make(chan struct{})

	// Writer: continuously swap all three hooks until the readers finish.
	writer.Add(1)
	go func() {
		defer writer.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			a.SetHooks(AppHooks{
				Confirm:     func(context.Context, tools.ConfirmRequest) (bool, error) { return true, nil },
				Log:         func(string) {},
				AgentEvents: agent.NoopEventSink{},
			})
		}
	}()

	// Readers: hit the confirm/log closures and the proxy sink concurrently.
	for i := 0; i < 4; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for j := 0; j < 2000; j++ {
				_, _ = tctx.Confirm(context.Background(), tools.ConfirmRequest{})
				tctx.Log("x")
				proxy.AssistantToken("t")
				proxy.Phase(domain.PhaseReceived)
			}
		}()
	}

	readers.Wait() // readers finished their fixed iterations
	close(stop)    // now tell the writer to stop
	writer.Wait()
}
