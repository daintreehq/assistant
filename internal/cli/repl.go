package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/commands"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
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
	reader := bufio.NewReader(os.Stdin)
	type lineResult struct {
		line string
		err  error
	}
	lines := make(chan lineResult)
	go func() {
		defer close(lines)
		for {
			line, err := reader.ReadString('\n')
			select {
			case lines <- lineResult{line: line, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

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
	a.StartScheduler(base, func(events []domain.QueueEvent) { printAttention(r, events) })

	printBanner(r, a, st.Connected, st.Transport)
	// One-time "while you were away" notice (consumed on read): the supervisor
	// daemon's detached activity + what this attach adopted.
	for _, line := range a.AttachSummaryLines() {
		r.Line(r.Gray("· " + line))
	}
	if !st.Connected {
		// Same split the cockpit makes (internal/ui/view.go mcpDegradedView): /reconnect
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
			// /clear additionally wipes the host terminal in the REPL.
			cmd, _, args := splitCmd(line)
			if cmd == "clear" && len(args) == 0 {
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
		err := runReplTurn(base, a, sigCh, line)
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
// action in the classic REPL. It matches internal/ui's confirmPhrase so git/
// system actions are equally hard to approve on either surface — keep in sync.
const replConfirmPhrase = "confirm"

// buildConfirmFunc builds the classic-REPL approval handler. It mirrors the
// cockpit's two-tier friction off the safety gate's verdict (req.NeedsTypedConfirm):
// the riskiest git/system actions demand the human type "confirm" (not a bare y),
// while everything else keeps the single-key [y/N] prompt. Extracted from the
// inline closure so the typed-confirm branch is unit-testable without a real stdin.
//
// Known limitation: a Ctrl-C WHILE this prompt is waiting for input does NOT cancel
// the turn — the classic REPL runs in cooked mode, so the blocking line read can't
// be interrupted by a signal (no raw-mode key handling here, unlike the cockpit).
// The decision is reached by typing n / Enter (declines, the safe default); Ctrl-C
// takes effect once control returns to the main prompt loop.
func buildConfirmFunc(r *render.Renderer, ask func(string) string) func(context.Context, tools.ConfirmRequest) (bool, error) {
	return func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		r.Warn(r.Bold(req.ToolName) + " (" + string(req.Risk) + ") wants to run:\n     " +
			req.Summary + "\n     args: " + render.Truncate(string(req.Args), 200))
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

// splitCmd extracts the canonical command word from a slash line (for the /clear
// host-wipe branch).
func splitCmd(line string) (string, string, []string) {
	trimmed := strings.TrimSpace(strings.TrimPrefix(line, "/"))
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", "", nil
	}
	return fields[0], strings.Join(fields[1:], " "), fields[1:]
}

// printBanner prints the REPL banner.
func printBanner(r *render.Renderer, a *app.App, connected bool, transport string) {
	mcpLine := "degraded local mode"
	if connected {
		mcpLine = "connected (" + transport + ")"
	}
	tierLine := "tier      " + string(a.Tier())
	// The cockpit raises a persistent AUTO-APPROVE badge; the classic REPL has no live
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
		r.Line(r.Magenta("◆ inbox") + " " + r.Bold(e.Title) + " " + r.Gray("("+string(e.Severity)+")"))
		r.Line("  " + e.Summary)
		if len(e.Evidence) > 0 {
			r.Line(r.Gray("  evidence: " + strings.Join(e.Evidence, " | ")))
		}
	}
	r.Out(r.Cyan("\ndaintree ❯ "))
}
