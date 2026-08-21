package cli

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/jsonout"
	"github.com/daintreehq/assistant/internal/commands"
	"github.com/daintreehq/assistant/internal/domain"
)

// errNoPrompts is the "you asked for a conversation and gave me none" failure.
var errNoPrompts = errors.New("--multi-turn: no prompts on stdin (one prompt per line)")

// runJSONTurns is the --multi-turn loop: one process, one ongoing agent.Session, one
// JSONL transcript. It reads one logical input per stdin line, exactly as the classic
// REPL does, and runs each non-command line as its own turn.
//
// It deliberately does NOT own any setup or teardown. Everything that is
// process-scoped — the ownership lease, app.Create, the MCP connect, the skill pin
// negotiation, the scheduler, the async barrier, Shutdown and the sink's terminal
// line — already happens once in RunOneShot and is correct as-is for a run of any
// number of turns. This function replaces exactly one statement there: the single Send.
//
// The returned error is reserved for a failure that makes CONTINUING meaningless (an
// empty script, or a Session.Send error, which signals a broken single-flight invariant
// rather than a failed answer). An ordinary failed turn is not one: it is reported on
// its own turn:end, latched into the run's outcome by the sink, and the next prompt
// still runs — matching the classic REPL, which prints the error and loops.
func runJSONTurns(ctx context.Context, a *app.App, sink *jsonout.Sink, in io.Reader) error {
	lines := streamLines(ctx, in)
	prompts := 0

	for {
		var res lineResult
		var open bool
		select {
		case <-ctx.Done():
			// The bound expired or the parent cancelled while waiting for input. There is
			// no turn to cancel, so mark the RUN cancelled rather than inventing an
			// assistant:cancelled event for a turn that never started. CancelRun never
			// downgrades a run that already failed.
			sink.CancelRun()
			return nil
		case res, open = <-lines:
			if !open {
				return finalPromptCheck(prompts)
			}
		}

		line := strings.TrimSpace(res.line)
		// EOF with nothing on it. A final line WITHOUT a trailing newline still arrives
		// with text and io.EOF, and it is a real prompt — so the text is checked before
		// the error is.
		if line == "" {
			if res.err != nil {
				return finalPromptCheck(prompts)
			}
			continue
		}

		if strings.HasPrefix(line, "/") {
			// The UI handler, not the REPL one: it returns STRUCTURED data and prints
			// nothing, which is the only shape compatible with stdout carrying JSONL and
			// nothing else. Notably it also never clears the host terminal — a --json run
			// must not emit an ANSI escape even for /clear.
			cmd := commands.HandleUICommand(ctx, line, a)
			sink.CommandResult(domain.JsonCommandResultPayload{
				Command:             line,
				Handled:             cmd.Handled,
				Title:               cmd.Title,
				Content:             cmd.Text,
				Quit:                cmd.Quit,
				ConversationCleared: cmd.ClearTranscript,
			})
			if cmd.Quit {
				return finalPromptCheck(prompts)
			}
			if res.err != nil {
				return finalPromptCheck(prompts)
			}
			continue
		}

		prompts++
		if err := runJSONTurn(ctx, a, sink, line); err != nil {
			return err
		}
		// Stop on a dead context rather than draining the rest of the script into a
		// cancelled session, exactly as the REPL breaks on base.Err(). The turn just
		// cancelled has already reported itself, so the run's outcome is set.
		if ctx.Err() != nil {
			return nil
		}
		if res.err != nil {
			return finalPromptCheck(prompts)
		}
	}
}

// runJSONTurn runs ONE prompt inside a turn bracket. SettleTurn is deferred so the
// bracket closes on every path — a clean answer, a failed turn, a cancelled one, or a
// panic unwinding through the loop — because a turn:prompt without its turn:end would
// leave a consumer waiting for a boundary that never comes.
func runJSONTurn(ctx context.Context, a *app.App, sink *jsonout.Sink, prompt string) error {
	sink.BeginTurn(prompt)
	defer sink.SettleTurn()
	// Session.Send, matching the single-turn path in RunOneShot (App.Send is a plain
	// pass-through to it). Turn FAILURES arrive as events, not as this error: the error
	// return is reserved for the single-flight guard, so a non-nil one means the loop
	// itself is broken and must not keep sending.
	_, err := a.Session.Send(ctx, prompt, agent.SendOptions{})
	return err
}

// finalPromptCheck turns "the script contained no prompt at all" into a run failure.
// An empty or command-only stdin is a harness mistake — an unset variable, a file that
// did not exist — and a run that reported success for it would hide the mistake behind
// a transcript with nothing in it.
func finalPromptCheck(prompts int) error {
	if prompts == 0 {
		return errNoPrompts
	}
	return nil
}
