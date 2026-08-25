package commands

import (
	"context"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
)

// CommandResult is the REPL slash handler return.
type CommandResult struct {
	Handled bool
	Quit    bool
	// ConversationCleared reports that /clear ACTUALLY cleared. It is the only
	// trustworthy signal: the clear is refused while a turn is in flight, so a caller
	// that instead matches the command TEXT wipes its surface while the engine keeps
	// the conversation and goes on working in it.
	ConversationCleared bool
}

// HandleSlashCommand handles a slash line in the line REPL, printing the result
// via the renderer. It shares the same data accessors as the UI
// handler so both surfaces stay in lockstep (the registry test asserts every
// command is handled by both). clearHostTerminal is called on /clear by the caller
// (the REPL owns stdout), signalled via CommandResult.ConversationCleared.
func HandleSlashCommand(ctx context.Context, line string, a *app.App, r *render.Renderer) CommandResult {
	return HandleSlashCommandWithProgress(ctx, line, a, r, nil)
}

// HandleSlashCommandWithProgress is HandleSlashCommand plus an EXTRA progress sink,
// called alongside the renderer's own stage lines rather than instead of them.
//
// It exists for the embedded host, which needs the identical command surface — same
// handler, same output, byte for byte — while also being able to show progress
// somewhere that is not a terminal. Routing that host through the UI handler instead
// would have been the smaller diff and the wrong one: `/help` there appends the
// cockpit's KEY CHEAT-SHEET, which describes keys an embedded panel does not have, and
// `/doctor` formats through a different function. Both would have changed under a host
// that only asked for a progress channel.
//
// nil is the ordinary case and costs nothing.
func HandleSlashCommandWithProgress(
	ctx context.Context,
	line string,
	a *app.App,
	r *render.Renderer,
	progress func(stage string),
) CommandResult {
	cmd, _, rest := parseCommand(line)
	if cmd == "" {
		return CommandResult{Handled: false}
	}
	name := canonical(cmd)
	if usage := noArgUsage(name, rest); usage != "" {
		r.Line(usage)
		return CommandResult{Handled: true}
	}

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
		// of behavior) while honoring the REPL "print to stdout" contract. Stage
		// progress from the slow model-backed commands (/compact) prints as it
		// happens so the REPL is never silent for the whole run.
		res := HandleUICommandWithProgress(ctx, line, a, func(stage string) {
			// EITHER the sink or the renderer, never both. A caller that supplied a sink
			// has its own place to show stages, and echoing them into the rendered text
			// as well would make the finished card open by repeating progress the caller
			// had already displayed — "· Opening your browser…" shown once live and then
			// again, past tense, at the top of the result.
			if progress != nil {
				progress(stage)
				return
			}
			r.Line("· " + stage)
		})
		if !res.Handled {
			return CommandResult{Handled: false}
		}
		if res.Text != "" {
			r.Line(res.Text)
		}
		return CommandResult{Handled: true, Quit: res.Quit, ConversationCleared: res.ClearTranscript}
	}
}

// printDoctor prints the checklist with ✓/✗ marks.
func printDoctor(r *render.Renderer, checks []DoctorCheck) {
	for _, c := range checks {
		line := padRight(c.Label, 16) + ": " + c.Detail
		if !c.OK && c.Fix != "" {
			line += "  → " + c.Fix
		}
		if c.OK {
			r.Success(line)
		} else {
			r.Error(line)
		}
	}
}
