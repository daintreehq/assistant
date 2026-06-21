package commands

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
)

// CommandResult is the REPL slash handler return (commandData.ts CommandResult).
type CommandResult struct {
	Handled bool
	Quit    bool
}

// HandleSlashCommand handles a slash line in the classic REPL, printing the result
// via the renderer (commands.ts). It shares the same data accessors as the UI
// handler so both surfaces stay in lockstep (the registry test asserts every
// command is handled by both). clearHostTerminal is called on /clear by the caller
// (the REPL owns stdout), signalled via the returned result + this handler's wipe
// of the conversation.
func HandleSlashCommand(ctx context.Context, line string, a *app.App, r *render.Renderer) CommandResult {
	cmd, _, _ := parseCommand(line)
	if cmd == "" {
		return CommandResult{Handled: false}
	}
	name := canonical(cmd)

	switch name {
	case "quit":
		return CommandResult{Handled: true, Quit: true}
	case "help":
		r.Line(HelpTextREPL())
		return CommandResult{Handled: true}
	case "doctor":
		printDoctor(r, RunDoctor(ctx, a))
		return CommandResult{Handled: true}
	default:
		// Every other command reuses the UI handler's data accessors and prints
		// the resulting card text. This keeps the two surfaces in sync (one source
		// of behavior) while honoring the REPL "print to stdout" contract.
		res := HandleUICommand(ctx, line, a)
		if !res.Handled {
			return CommandResult{Handled: false}
		}
		if res.Text != "" {
			r.Line(res.Text)
		}
		return CommandResult{Handled: true, Quit: res.Quit}
	}
}

// printDoctor prints the checklist with ✓/✗ marks (commandRegistry §6.4 doctor).
func printDoctor(r *render.Renderer, checks []DoctorCheck) {
	for _, c := range checks {
		line := padRight(c.Label, 16) + ": " + c.Detail
		if c.Fix != "" {
			line += "  → " + c.Fix
		}
		if c.OK {
			r.Success(line)
		} else {
			r.Error(line)
		}
	}
}
