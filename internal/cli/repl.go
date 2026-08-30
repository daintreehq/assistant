package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/commands"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/redact"
	"github.com/daintreehq/assistant/internal/tools"
)

// hostTerminalClear erases the viewport, the scrollback, and homes the cursor —
// the three escapes `clear` emits on xterm-class terminals, IN ORDER. Never
// touches the alternate buffer.
const hostTerminalClear = "\x1b[2J\x1b[3J\x1b[H"

// clearHostTerminal writes the clear sequence when stdout is a TTY (errors
// ignored — a failed escape must never break /clear).
func clearHostTerminal() {
	if !stdoutIsTTY() {
		return
	}
	defer func() { _ = recover() }()
	fmt.Fprint(os.Stdout, hostTerminalClear)
}

// startRepl runs the classic line REPL. Returns the
// process exit code.
func startRepl(ctx context.Context, a *app.App) int {
	r := render.Stdout()
	// Shared with the --json --multi-turn loop (internal/cli/lineinput.go): the same
	// stdin contract drives both, so it is stated once. Unbounded (limit 0) keeps the
	// REPL exactly as it was — an interactive line has a human typing the end of it.
	lines := streamLines(ctx, os.Stdin, 0)

	// Keep the read error alongside the line so EOF is distinguishable from an
	// intentionally empty submission. Reading on a goroutine also lets SIGTERM or
	// parent cancellation release an otherwise-blocked idle REPL cleanly.
	readLine := func(prompt string) (string, bool) {
		r.Out(prompt)
		var result lineResult
		var ok bool
		select {
		case <-ctx.Done():
			return "", false
		case result, ok = <-lines:
			if !ok {
				return "", false
			}
		}
		trimmed := strings.TrimSpace(result.line)
		if result.err != nil && trimmed == "" {
			return "", false
		}
		return trimmed, true
	}
	ask := func(prompt string) string {
		line, _ := readLine(prompt)
		return line
	}
	// askLine is ask that DISTINGUISHES an EOF / closed-stdin read (ok=false) from an
	// empty line (ok=true, ""), so the multiple-choice prompt can cancel on EOF instead
	// of silently taking the default answer.
	askLine := readLine

	confirm := buildConfirmFunc(r, ask)
	askChoice := buildAskChoiceFunc(r, askLine)
	logHook := func(m string) { r.Line(r.Gray("  · " + m)) }

	a.SetHooks(app.AppHooks{
		AgentEvents: NewConsoleSink(r),
		Confirm:     confirm,
		AskChoice:   askChoice,
		Log:         logHook,
	})

	// Own SIGINT for the REPL: a Ctrl-C DURING a turn cancels that one generation and
	// keeps the REPL alive (the high-value fix — previously the process was killed); at
	// idle it is a no-op (the cooked-mode line read can't be interrupted portably — exit
	// with Ctrl-D or /exit). main deliberately excludes SIGINT from the interactive
	// launch context, so Ctrl-C cannot poison later work. SIGTERM and explicit parent
	// cancellation still flow through base and shut the REPL down.
	base := ctx
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	a.ConnectMcp(base)
	st := a.MCP.Status()
	// Declared before the scheduler starts and shared with the prompt loop below: it is
	// what keeps a due message and a typed line from becoming two turns at once.
	var wakeMu sync.Mutex

	// A scheduled MESSAGE runs here too, rather than being printed and forgotten.
	//
	// This surface had no wake reactor at all: the scheduler marked the event delivered,
	// the REPL printed it, and the instruction the user had scheduled was never carried
	// out by anybody — the exact silent-loss failure the whole feature exists to end.
	// Everything else still just prints; only an instruction earns a turn.
	a.StartScheduler(base, func(events []domain.QueueEvent) {
		messages, notices := splitTimerMessages(events)
		if len(notices) > 0 {
			printAttention(r, notices)
		}
		for _, e := range messages {
			// Serialized, and never concurrently with a user turn: the REPL is one line
			// at a time, and two turns sharing this session would interleave their
			// output into the same terminal.
			wakeMu.Lock()
			runReplWake(base, a, r, e)
			wakeMu.Unlock()
		}
	}, nil)

	printBanner(r, a, st.Connected, st.Transport)
	// One-time "while you were away" notice (consumed on read): the supervisor
	// daemon's detached activity + what this attach adopted.
	for _, line := range a.AttachSummaryLines() {
		r.Line(r.Gray("· " + line))
	}
	if !st.Connected {
		// Same split the attached session makes (internal/ui/view.go mcpDegradedView): /reconnect
		// retries a dropped link, a session Daintree closed or replaced needs a fresh token
		// that only reopening the panel supplies, and with no endpoint configured at all
		// there is nothing for /reconnect to retry.
		if a.Config.Offline || strings.TrimSpace(a.Config.McpURL) == "" {
			r.Warn("Degraded local mode — no Daintree link, so no terminals, agents or worktrees. Launch the assistant from Daintree to enable them.")
		} else {
			r.Warn("Daintree MCP not connected. Run /reconnect. If Daintree closed or replaced this session, reopen the Assistant panel.")
		}
	}
	// The CLI no longer holds model credentials (the backend owns them), so there is no
	// key to preflight. If the backend is unreachable, a turn surfaces a clear backend
	// error; read-only slash commands work regardless.

	for {
		line, ok := readLine(r.Cyan("\ndaintree ❯ "))
		if !ok {
			break
		}
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			res := commands.HandleSlashCommand(base, line, a, r)
			if res.Quit {
				break
			}
			// /clear additionally wipes the host terminal in the REPL — but only when
			// the engine actually cleared. Matching the command text instead would
			// wipe the scrollback on a clear the engine REFUSED, leaving the user
			// staring at a blank screen with the conversation still live behind it.
			if res.ConversationCleared {
				clearHostTerminal()
			}
			continue
		}
		// Drain any Ctrl-C buffered while idle so a stray idle press can't instantly
		// cancel this fresh turn.
		select {
		case <-sigCh:
		default:
		}
		// The SAME lock the wake reactor takes. Holding it on only one side was not
		// serialization: whichever started first won, and the loser was rejected with
		// ErrTurnInProgress — for a scheduled message that meant the scheduler had
		// already marked it delivered and nothing in this owner would try again, and
		// for the user it meant their typed line bouncing off a turn they could not see.
		wakeMu.Lock()
		err := runReplTurn(base, a, sigCh, line)
		wakeMu.Unlock()
		if base.Err() != nil {
			break
		}
		if err != nil {
			r.Error(err.Error())
		}
	}

	_ = a.Shutdown()
	if ctx.Err() != nil {
		return domain.OneShotExitCode.Cancelled
	}
	r.Line(r.Gray("Goodbye."))
	return 0
}

// replConfirmPhrase is the word the human must type to approve a typed-confirm
// action in the line REPL. It matches internal/ui's confirmPhrase so git/
// system actions are equally hard to approve on either surface — keep in sync.
const replConfirmPhrase = "confirm"

// buildConfirmFunc builds the classic-REPL approval handler. It mirrors the
// attached session's two-tier friction off the safety gate's verdict (req.NeedsTypedConfirm):
// the riskiest git/system actions demand the human type "confirm" (not a bare y),
// while everything else keeps the single-key [y/N] prompt. Extracted from the
// inline closure so the typed-confirm branch is unit-testable without a real stdin.
//
// Known limitation: a Ctrl-C WHILE this prompt is waiting for input does NOT cancel
// the turn — the line REPL runs in cooked mode, so the blocking line read can't
// be interrupted by a signal (no raw-mode key handling here, unlike the attached session).
// The decision is reached by typing n / Enter (declines, the safe default); Ctrl-C
// takes effect once control returns to the main prompt loop.
func buildConfirmFunc(r *render.Renderer, ask func(string) string) func(context.Context, tools.ConfirmRequest) (bool, error) {
	return func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		// Same confirm-boundary invariant the host applies: mask credentials BEFORE
		// truncating. A confirm request is built from the dispatch arguments and never
		// passes through the EventSink's source-side sanitization, so without this a
		// secret in the args is printed verbatim to the terminal (and its scrollback).
		r.Warn(r.Bold(req.ToolName) + " (" + string(req.Risk) + ") wants to run:\n     " +
			req.Summary + "\n     args: " + render.Truncate(redact.String(string(req.Args)), 200))
		if req.NeedsTypedConfirm {
			// Irreversible (git/system): require the typed phrase, never a single key.
			r.Warn(`This action is irreversible.`)
			answer := ask(r.Yellow(`   type "` + replConfirmPhrase + `" to approve: `))
			return strings.EqualFold(strings.TrimSpace(answer), replConfirmPhrase), nil
		}
		answer := ask(r.Yellow("   approve? [y/N] "))
		a := strings.ToLower(strings.TrimSpace(answer))
		return a == "y" || a == "yes", nil
	}
}

// buildAskChoiceFunc builds the classic-REPL multiple-choice handler for
// user.askMultipleChoice. It prints the question + labelled options and reads a single
// option letter (A–Z, case-insensitive) or a 1-based number, re-prompting on an invalid
// entry. An empty line takes the default option; an EOF / closed stdin (askLine ok=false)
// CANCELS the question (context.Canceled → QUESTION_CANCELLED) rather than silently
// answering with the default. Mirrors the cooked-mode limitation documented on
// buildConfirmFunc: a Ctrl-C here only takes effect once control returns to the prompt loop.
func buildAskChoiceFunc(r *render.Renderer, askLine func(string) (string, bool)) func(context.Context, tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
	return func(_ context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
		def := req.Default
		if def < 0 || def >= len(req.Options) {
			def = 0
		}
		r.Line("")
		r.Line(r.Bold(req.Question))
		for i, opt := range req.Options {
			suffix := ""
			if i == def {
				suffix = r.Gray("  (default)")
			}
			r.Line("     " + r.Bold(opt.Label+".") + " " + opt.Text + suffix)
		}
		last := req.Options[len(req.Options)-1].Label
		for {
			answer, ok := askLine(r.Yellow("   choose [A-" + last + "] (Enter for default): "))
			if !ok {
				// EOF / closed stdin — cancel instead of silently taking the default.
				return tools.AskChoiceAnswer{}, context.Canceled
			}
			if answer == "" {
				opt := req.Options[def]
				return tools.AskChoiceAnswer{Label: opt.Label, Index: def, Text: opt.Text}, nil
			}
			if idx, ok := replChoiceIndex(answer, len(req.Options)); ok {
				opt := req.Options[idx]
				return tools.AskChoiceAnswer{Label: opt.Label, Index: idx, Text: opt.Text}, nil
			}
			r.Warn("   Please enter a letter between A and " + last + " (or its number).")
		}
	}
}

// replChoiceIndex maps a typed answer to a 0-based option index: a single letter (A–Z /
// a–z) or a 1-based number ("2" → index 1). ok=false when it isn't a valid choice.
func replChoiceIndex(s string, n int) (int, bool) {
	s = strings.TrimSpace(s)
	if len(s) == 1 {
		c := s[0]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c >= 'A' && c <= 'Z' {
			if idx := int(c - 'A'); idx < n {
				return idx, true
			}
			return 0, false
		}
	}
	if num, err := strconv.Atoi(s); err == nil {
		if idx := num - 1; idx >= 0 && idx < n {
			return idx, true
		}
	}
	return 0, false
}

// runReplTurn runs one user turn under a cancellable child context. A Ctrl-C (sigCh)
// arriving while the generation is in flight cancels it gracefully (the session
// unwinds to AssistantCancelled and prints "Turn cancelled") and returns nil so the
// REPL loops back to the prompt instead of surfacing the cancellation as an error.
func runReplTurn(base context.Context, a *app.App, sigCh <-chan os.Signal, line string) error {
	return runCancellable(base, sigCh, func(ctx context.Context) error {
		// App.Send (not Session.Send) so the first user turn consumes the one-time
		// session-ended-watchers NOTE from message[1].
		_, err := a.Send(ctx, line, agent.SendOptions{})
		return err
	})
}

// runCancellable runs fn under a cancellable child of base and aborts it on the first
// sigCh signal — cancelling fn's context and swallowing its result so a Ctrl-C cancels
// just this unit of work (the generation) without surfacing an error or tearing down
// the REPL. Returns fn's error on normal completion. Pure (no app/session), so the
// interrupt contract is unit-testable.
func runCancellable(base context.Context, sigCh <-chan os.Signal, fn func(context.Context) error) error {
	ctx, cancel := context.WithCancel(base)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn(ctx) }()
	select {
	case <-sigCh:
		cancel() // first Ctrl-C: abort this generation, keep the REPL alive
		<-done   // let the unit unwind before returning to the prompt
		return nil
	case err := <-done:
		return err
	}
}

// printBanner prints the REPL banner.
func printBanner(r *render.Renderer, a *app.App, connected bool, transport string) {
	mcpLine := "degraded local mode"
	if connected {
		mcpLine = "connected (" + transport + ")"
	}
	tierLine := "tier      " + string(a.Tier())
	// The attached session raises a persistent AUTO-APPROVE badge; the line REPL has no live
	// footer to put one in, so it goes on the tier line — which is where it belongs
	// anyway, since it is a statement ABOUT the tier: the gate is unchanged, but nothing
	// inside it will ask first. Without this, `--classic` was the one surface where a
	// session running unattended looked exactly like one that was not.
	if a.Config.AutoApprove {
		tierLine += "  " + r.Bold("AUTO-APPROVE: mutating actions will NOT ask first")
	}
	lines := []string{
		r.Bold("Daintree Assistant") + "  — local operations officer",
		"project   " + a.Config.ProjectPath,
		"backend   " + mcp.SanitizeURL(a.Backend.BaseURL()),
		"mcp       " + mcpLine,
		tierLine,
		r.Gray("Type /help for commands; Ctrl+D or /quit exits."),
		r.Gray("I never edit files directly — I spawn and supervise agents."),
	}
	r.Banner(lines)
}

// printAttention prints out-of-band inbox events, then restores the prompt.
func printAttention(r *render.Renderer, events []domain.QueueEvent) {
	// Out-of-band: an inbox event can land while the user is mid-type at the prompt.
	// Erase the current prompt line first (TTY only) so the event starts clean instead
	// of being appended onto the half-typed input, then reprint the prompt afterward.
	// The in-progress text survives in the terminal's cooked line buffer and still
	// submits on Enter.
	if stdoutIsTTY() {
		r.Out("\r\x1b[K")
	}
	for _, e := range events {
		r.Line("")
		// A scheduled MESSAGE is an instruction, not a notice, and this surface has no
		// wake reactor to carry it out — the panel and the background supervisor do.
		// Printing it under the same "inbox" heading as a reminder would let the user's
		// own instruction scroll past looking like something already handled, which is
		// the failure this whole feature exists to end. Say plainly that it is due and
		// that nothing here will run it, so the person sitting at this prompt can.
		// Defensive only: printAttention is now handed the NOTICES half of a burst, so
		// a message should never reach here. If one does, say what it is rather than
		// filing an instruction under the same heading as a reminder.
		if agent.IsTimerMessageEvent(e) {
			r.Line(r.Magenta("◆ scheduled message") + " " + r.Bold(e.Title))
			r.Line("  " + e.Summary)
			continue
		}
		r.Line(r.Magenta("◆ inbox") + " " + r.Bold(e.Title) + " " + r.Gray("("+string(e.Severity)+")"))
		r.Line("  " + e.Summary)
		if len(e.Evidence) > 0 {
			r.Line(r.Gray("  evidence: " + strings.Join(e.Evidence, " | ")))
		}
	}
	r.Out(r.Cyan("\ndaintree ❯ "))
}

// splitTimerMessages separates scheduled instructions from everything else in a burst.
//
// By SHAPE, matching every other surface: a stale message is still a message, and the
// eligibility gate below decides whether it runs.
func splitTimerMessages(events []domain.QueueEvent) (messages, notices []domain.QueueEvent) {
	for _, e := range events {
		if agent.IsTimerMessageEvent(e) {
			messages = append(messages, e)
			continue
		}
		notices = append(notices, e)
	}
	return messages, notices
}

// runReplWake carries out one due scheduled message as an ordinary turn.
//
// Announced before and after, because a turn nobody asked for appearing at a prompt is
// alarming unless it says why it is there.
func runReplWake(ctx context.Context, a *app.App, r *render.Renderer, e domain.QueueEvent) {
	if !agent.IsActionableWake(e) {
		// Past its freshness window. Say so — the user set this and is standing right
		// here; silently dropping it would be the same failure in a quieter voice.
		r.Line("")
		r.Line(r.Magenta("◆ scheduled message missed") + " " + r.Bold(e.Title))
		r.Line("  " + e.Summary)
		r.Line(r.Gray("  it came due too long ago to act on safely, so it was not carried out"))
		return
	}
	r.Line("")
	r.Line(r.Magenta("◆ scheduled message due") + " " + r.Bold(e.Title))
	prompt := agent.BuildWakePrompt([]domain.QueueEvent{e}, nil)
	reply, err := a.Send(ctx, prompt, agent.SendOptions{IsWake: true, FromTimerMessage: true})
	if err != nil {
		r.Line(r.Gray("  the scheduled message could not be carried out: " + err.Error()))
		return
	}
	// A nil error is NOT success. Send reports a model failure, a tool failure and a
	// cancellation as sentinel REPLIES rather than errors, so resolving on err==nil
	// alone closed the errand for a turn that had done nothing — and a closed errand is
	// invisible to the boot recovery that would otherwise have delivered it again. The
	// other two reactors test the sentinel; this one has to as well.
	if agent.IsWakeFailureReply(reply) {
		r.Line(r.Gray("  the scheduled message did not complete — it stays in the inbox"))
		return
	}
	// Close the errand, exactly as the other reactors do, so it stops counting as
	// something still waiting on the user.
	a.ResolveAttention(agent.TimerMessageEventIDs([]domain.QueueEvent{e}))
}
