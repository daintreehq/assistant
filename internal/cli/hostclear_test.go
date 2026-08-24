package cli

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// blockingBackend holds a turn in flight until release is closed, which is what makes
// Session.Clear() return ErrTurnInProgress.
type blockingBackend struct{ release <-chan struct{} }

func (b blockingBackend) RespondStream(ctx context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	select {
	case <-b.release:
	case <-ctx.Done():
		return backend.RespondResult{}, ctx.Err()
	}
	if cb.OnMeta != nil {
		cb.OnMeta(backend.StreamMeta{State: "s1"})
	}
	return backend.RespondResult{Message: backend.RespondMessage{Role: "assistant", Content: "done"}}, nil
}

func (blockingBackend) RunTask(context.Context, backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{}, nil
}

type clearNoopRunner struct{}

func (clearNoopRunner) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (clearNoopRunner) ResolveWireName(string) string                   { return "" }
func (clearNoopRunner) Dispatch(context.Context, string, string, agent.TurnContext) domain.ToolResult {
	return domain.Ok("ok", nil)
}

// A refused /clear must report ConversationCleared FALSE all the way out to the host.
//
// The engine refuses a mid-turn clear because wiping history would corrupt the streaming
// snapshot. The outcome was then dropped at this adapter, so an embedding host saw a
// command:result for "/clear" with nothing to contradict the word — and Daintree's panel
// wiped its transcript, tool rows and live state while the engine kept the conversation
// and went on working in it. The user ends up talking to a model whose context they can
// no longer see, which is strictly worse than the refusal they never saw.
func TestHostRunCommand_RefusedClearReportsNotCleared(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	overrides, err := overridesFromOptions(Options{Offline: boolPtr(true), Project: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	release := make(chan struct{})
	a.Session = agent.NewSession(agent.SessionDeps{
		Backend:   blockingBackend{release: release},
		Tools:     clearNoopRunner{},
		SessionID: "sess_test",
	})
	a.Session.InjectNote("important history")
	historyBefore := len(a.Session.Messages())

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = a.Session.Send(context.Background(), "go", agent.SendOptions{})
	}()

	// Wait until the turn is genuinely in flight — Clear() failing IS the signal.
	deadline := time.Now().Add(2 * time.Second)
	for a.Session.Clear() == nil {
		if time.Now().After(deadline) {
			t.Fatal("turn never became in-flight")
		}
		a.Session.InjectNote("important history")
		time.Sleep(time.Millisecond)
	}

	adapter := &hostAppAdapter{app: a}
	out := adapter.RunCommand(context.Background(), "/clear")
	if out.ConversationCleared {
		t.Fatal("a REFUSED /clear reported ConversationCleared — the host will wipe a live transcript on this")
	}
	// The refusal has to be legible on its own: the host renders this text.
	if !strings.Contains(strings.ToLower(out.Text), "in progress") {
		t.Errorf("refusal text does not explain itself: %q", out.Text)
	}
	if len(a.Session.Messages()) < historyBefore {
		t.Error("a refused /clear must leave the conversation intact")
	}

	// And the same command, once the turn settles, reports TRUE — the flag tracks the
	// engine's actual behaviour rather than always failing safe.
	close(release)
	wg.Wait()
	if out := adapter.RunCommand(context.Background(), "/clear"); !out.ConversationCleared {
		t.Fatalf("a /clear that succeeded must report it: %+v", out)
	}
}
