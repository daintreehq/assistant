package commands

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
)

// repl runs a line through the REPL handler against a GIVEN app and returns what it
// printed.
//
// The app is a parameter rather than built here on purpose: /status prints the session
// id, the project path and the state dir, so two invocations against two freshly built
// apps differ in three places that have nothing to do with what is being compared.
func repl(t *testing.T, a *app.App, line string, progress func(string)) (string, CommandResult) {
	t.Helper()
	var buf bytes.Buffer
	res := HandleSlashCommandWithProgress(context.Background(), line, a, render.New(&buf), progress)
	return buf.String(), res
}

// An embedded host asked only for a progress channel; it must not get a different
// command surface as a side effect.
//
// This is the regression it guards, and it was live for a while: the host adapter was
// switched from the REPL handler to the UI one purely because that was the arm taking a
// progress sink. Most commands are served identically by both, so nearly everything
// looked fine — but `/help` diverges. The UI arm appends the cockpit's KEY CHEAT-SHEET,
// which describes terminal keybindings an embedded panel does not have and cannot honour,
// so a panel's `/help` began advertising keys that did nothing.
//
// Compared as WHOLE outputs rather than by looking for the cheat-sheet: pinning the
// symptom would pass the next time the two arms diverge somewhere else.
func TestProgressSinkDoesNotChangeTheCommandSurface(t *testing.T) {
	for _, line := range []string{"/help", "/status", "/backend", "/account"} {
		// ONE app for both invocations — see repl.
		a := newOfflineApp(t)
		withoutSink, resA := repl(t, a, line, nil)
		withSink, resB := repl(t, a, line, func(string) {})

		if withoutSink != withSink {
			t.Errorf("%s renders differently once a progress sink is supplied\n--- without ---\n%s\n--- with ---\n%s",
				line, withoutSink, withSink)
		}
		if resA.Quit != resB.Quit || resA.ConversationCleared != resB.ConversationCleared {
			t.Errorf("%s returned a different verdict with a progress sink: %+v vs %+v", line, resA, resB)
		}
	}
}

// A supplied sink TAKES the stage lines rather than duplicating them.
//
// Both channels reach the same person on an embedded host — the sink live, the rendered
// text when the command settles — so echoing to both made a finished card open by
// repeating progress the panel had already shown.
func TestProgressStagesGoToTheSinkInsteadOfTheRenderedText(t *testing.T) {
	// A command with no stages proves nothing, so this needs one that reports. `/login`
	// reports "Opening your browser…" before it fails, which is exactly the shape being
	// tested. It is safe here only because `newOfflineApp` pins the endpoint at a dead
	// loopback port — see `deadBackend`. Against a reachable deployment this line would
	// open a browser and wait five minutes.
	a := newOfflineApp(t)
	var stages []string
	rendered, _ := repl(t, a, "/login", func(s string) { stages = append(stages, s) })

	if len(stages) == 0 {
		t.Fatal("the sink received no stages, so this proves nothing about where they went")
	}
	for _, stage := range stages {
		if strings.Contains(rendered, stage) {
			t.Errorf("stage %q reached the sink AND the rendered text — the card repeats progress already shown\n%s",
				stage, rendered)
		}
	}

	// And with no sink the renderer still gets them: the line REPL has nowhere else to
	// show progress, so suppressing them there would make a slow command silent.
	renderedNoSink, _ := repl(t, a, "/login", nil)
	if !strings.Contains(renderedNoSink, stages[0]) {
		t.Errorf("stage %q vanished from the REPL's own output, which has no other channel\n%s",
			stages[0], renderedNoSink)
	}
}
