package host

import (
	"context"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/prompts"
)

func TestPromptWorktreeWireStates(t *testing.T) {
	for _, tc := range []struct {
		input          string
		known, present bool
	}{
		{`{"type":"prompt","sessionId":"s","text":"hi"}`, false, false},
		{`{"type":"prompt","sessionId":"s","text":"hi","worktree":null}`, true, false},
		{`{"type":"prompt","sessionId":"s","text":"hi","worktree":{"id":"b","path":"/b","branch":"topic"}}`, true, true},
	} {
		cmd, err := ParseCommand([]byte(tc.input))
		if err != nil {
			t.Fatal(err)
		}
		if (cmd.Worktree != nil) != tc.known || (tc.known && cmd.Worktree.Present != tc.present) {
			t.Fatalf("decoded %+v", cmd)
		}
	}
	for _, value := range []string{`{}`, `"b"`, `[]`, `{"id":"","path":"/b","branch":""}`, `{"id":"b","path":null,"branch":""}`} {
		if _, err := ParseCommand([]byte(`{"type":"prompt","sessionId":"s","text":"hi","worktree":` + value + `}`)); err == nil {
			t.Fatalf("accepted malformed worktree %s", value)
		}
	}
}

type contextualSession struct {
	*wakeSession
	engine  *agent.Session
	options chan agent.SendOptions
}

func (s *contextualSession) Send(ctx context.Context, text string, opts agent.SendOptions) (string, error) {
	s.options <- opts
	return s.wakeSession.Send(ctx, text, opts)
}
func (s *contextualSession) InjectUserPrompt(p agent.UserPrompt) { s.engine.InjectUserPrompt(p) }
func (s *contextualSession) RetractUserPrompt() (agent.UserPrompt, bool) {
	return s.engine.RetractUserPrompt()
}
func (s *contextualSession) DiscardPendingInjections() { s.engine.DiscardPendingInjections() }

func TestHostPreservesWorktreeThroughBusyPromptAndStrandRecovery(t *testing.T) {
	s := &contextualSession{wakeSession: newWakeSession(), engine: agent.NewSession(agent.SessionDeps{}), options: make(chan agent.SendOptions, 3)}
	h, _, _ := newWakeHost(t, s.wakeSession)
	h.session = s
	a := &prompts.WorktreeContext{Present: true, ID: "a", Path: "/a"}
	b := &prompts.WorktreeContext{Present: true, ID: "b", Path: "/b"}
	h.handleCommand(HostCommand{Type: CmdPrompt, Text: "start contest", Worktree: a})
	select {
	case opts := <-s.options:
		if opts.Worktree.ID != "a" {
			t.Fatal(opts)
		}
	case <-time.After(time.Second):
		t.Fatal("first prompt not sent")
	}
	h.handleCommand(HostCommand{Type: CmdPrompt, Text: "vote", Worktree: b})
	close(s.release)
	select {
	case opts := <-s.options:
		if opts.Worktree.ID != "b" {
			t.Fatal("recovered prompt lost selection", opts)
		}
	case <-time.After(time.Second):
		t.Fatal("stranded prompt not recovered")
	}
	h.turnWG.Wait()
}
