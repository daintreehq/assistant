package cli

import (
	"bytes"
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/commands"
)

// The embedded host serves the SAME command surface as the line REPL, byte for byte.
//
// Sharing one handler is the stated contract of this adapter, and the registry parity
// test asserts every command is served by both — but neither catches the two arms
// producing DIFFERENT OUTPUT for the same command, which is what happened. Adding a
// progress channel for `/login` meant reaching for the handler that accepted a progress
// sink, and that was the UI one. Most commands are served identically by both, so almost
// everything looked right; `/help` is not one of them. The UI arm appends the cockpit's
// key cheat-sheet, so an embedded panel's `/help` began advertising terminal keybindings
// the panel does not have and cannot honour.
//
// Compared as whole outputs rather than by hunting for the cheat-sheet: pinning that one
// symptom would pass the next time the two arms diverge somewhere else.
func TestHostRunCommandMatchesTheReplSurface(t *testing.T) {
	t.Setenv("DAINTREE_ASSISTANT_STATE_DIR", t.TempDir())
	// A loopback port nothing listens on. `Offline` governs the MODEL backend and does
	// not reach the account layer, so without this /account resolves to the deployed
	// endpoint and this test does real network I/O against production.
	overrides, err := overridesFromOptions(Options{
		Offline: boolPtr(true), Project: t.TempDir(), BackendURL: "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })

	adapter := &hostAppAdapter{app: a}

	// The SAME app for both, because /status prints the session id and the state dir.
	for _, line := range []string{"/help", "/status", "/backend", "/account"} {
		var buf bytes.Buffer
		want := commands.HandleSlashCommand(context.Background(), line, a, render.New(&buf))
		viaHost := adapter.RunCommand(context.Background(), line)

		if viaHost.Text != buf.String() {
			t.Errorf("%s differs between the host and the REPL\n--- repl ---\n%s\n--- host ---\n%s",
				line, buf.String(), viaHost.Text)
		}
		if viaHost.Quit != want.Quit || viaHost.ConversationCleared != want.ConversationCleared {
			t.Errorf("%s: host verdict %+v disagrees with the REPL's %+v", line, viaHost, want)
		}
		if viaHost.Unknown {
			t.Errorf("%s came back unknown from the host adapter", line)
		}
	}
}

// A slow command is reported as slow THROUGH the adapter, which is the only path the
// host can ask down. A registry entry the adapter does not forward would leave `/login`
// running inline on the command loop — the freeze the Slow flag exists to prevent.
func TestHostAdapterReportsSlowCommands(t *testing.T) {
	adapter := &hostAppAdapter{}
	if !adapter.IsSlowCommand("/login") {
		t.Error("the adapter does not report /login as slow, so the host would run it on its command loop")
	}
	if adapter.IsSlowCommand("/status") {
		t.Error("the adapter reports /status as slow; it returns promptly and belongs on the loop")
	}
}
