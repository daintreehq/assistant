package cli

import (
	"context"
	"errors"
	"fmt"
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
// empty script, an unreadable stdin, or a Session.Send error, which signals a broken
// single-flight invariant rather than a failed answer). An ordinary failed turn is not
// one: it is reported on its own turn:end, latched into the run's outcome by the sink,
// and the next prompt still runs — matching the classic REPL, which prints the error
// and loops.
func runJSONTurns(ctx context.Context, a *app.App, sink *jsonout.Sink, in io.Reader) error {
	lines := streamLines(ctx, in, maxPromptFileBytes)
	prompts := 0

	// done is the ONE way out of this loop, and centralising it is the point: every exit
	// — cancelled, EOF, /quit, a read failure — has to answer "did the bound expire?"
	// before it answers "was the script empty?". Returning straight from any of those
	// paths let a deadline that truncated the script still report the last SUCCESSFUL
	// turn as the run's outcome, which is a silent lie about work that never happened.
	done := func(err error) error {
		if ctx.Err() != nil {
			sink.CancelRun()
		}
		if err != nil {
			return err
		}
		return finalPromptCheck(prompts, ctx)
	}

	for {
		var res lineResult
		var open bool
		select {
		case <-ctx.Done():
			// The bound expired or the parent cancelled while waiting for input. There is
			// no turn to cancel, so this marks the RUN cancelled rather than inventing an
			// assistant:cancelled event for a turn that never started.
			return done(nil)
		case res, open = <-lines:
			if !open {
				return done(nil)
			}
		}
		// Checked BEFORE the line is acted on: select picks randomly when input and
		// cancellation are both ready, so without this a queued line could start a whole
		// new turn on an already-dead context.
		if ctx.Err() != nil {
			return done(nil)
		}

		// Only io.EOF is an ordinary end of input. Any other read error is a REAL
		// failure — a truncated pipe, an I/O error — and treating it as a clean finish
		// would report success for a conversation that was cut off mid-script.
		readFailed := res.err != nil && !errors.Is(res.err, io.EOF)

		line := strings.TrimSpace(res.line)
		// A final line WITHOUT a trailing newline still arrives with text, accompanied by
		// io.EOF — and it is a real prompt. So the text is checked before the error is.
		if line == "" {
			if readFailed {
				return done(fmt.Errorf("--multi-turn: reading stdin: %w", res.err))
			}
			if res.err != nil {
				return done(nil)
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
				Command: line,
				// NOT cmd.Handled: that reports whether the handler consumed the line,
				// which is true even for a command the catalog has never heard of (the
				// cockpit still gets an "Unknown command" card to render). Only the
				// registry can answer whether the command EXISTS, and a script needs
				// that answer to catch its own typos.
				Handled:             commands.IsKnownCommand(line),
				Title:               cmd.Title,
				Content:             cmd.Text,
				Quit:                cmd.Quit,
				ConversationCleared: cmd.ClearTranscript,
			})
			// A command can be slow — /compact runs two backend calls — so the deadline
			// may well have expired inside it.
			if ctx.Err() != nil {
				return done(nil)
			}
			// A read FAILURE outranks /quit, and the order matters: a reader can hand
			// back a partial "/quit" together with an I/O error, and stopping on the
			// command would report that truncated stream as a clean, deliberate exit.
			if readFailed {
				return done(fmt.Errorf("--multi-turn: reading stdin: %w", res.err))
			}
			if cmd.Quit || res.err != nil {
				return done(nil)
			}
			continue
		}

		prompts++
		if err := runJSONTurn(ctx, a, sink, line); err != nil {
			return done(err)
		}
		// Stop on a dead context rather than draining the rest of the script into a
		// cancelled session, exactly as the REPL breaks on base.Err().
		if ctx.Err() != nil {
			return done(nil)
		}
		if readFailed {
			return done(fmt.Errorf("--multi-turn: reading stdin: %w", res.err))
		}
		if res.err != nil {
			return done(nil)
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
//
// A CANCELLED run is exempt: a bound that expired before the first prompt was reached
// says nothing about whether the script had one, and reporting "no prompts on stdin"
// there would send the reader to fix a file that is perfectly correct.
func finalPromptCheck(prompts int, ctx context.Context) error {
	if prompts == 0 && ctx.Err() == nil {
		return errNoPrompts
	}
	return nil
}
