package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
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

	ask := func(prompt string) string {
		r.Out(prompt)
		line, err := reader.ReadString('\n')
		if err != nil {
			return ""
		}
		return strings.TrimSpace(line)
	}

	confirm := buildConfirmFunc(r, ask)
	logHook := func(m string) { r.Line(r.Gray("  · " + m)) }

	a.SetHooks(app.AppHooks{
		AgentEvents: NewConsoleSink(r),
		Confirm:     confirm,
		Log:         logHook,
	})

	// Own SIGINT for the REPL: a Ctrl-C DURING a turn cancels that one generation and
	// keeps the REPL alive (the high-value fix — previously the process was killed); at
	// idle it is a no-op (the cooked-mode line read can't be interrupted portably — exit
	// with Ctrl-D or /exit). The session + scheduler + slash commands run on a base
	// context DECOUPLED from main's signal context, which cancels exactly once on the
	// first Ctrl-C; reusing it would wedge every later turn after a single interrupt.
	base := context.WithoutCancel(ctx)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)

	a.ConnectMcp(base)
	st := a.MCP.Status()
	a.StartScheduler(base, func(events []domain.QueueEvent) { printAttention(r, events) })

	printBanner(r, a, st.Connected, st.Transport)
	if !st.Connected {
		r.Warn("Running in degraded local mode — Daintree MCP not connected. Use /reconnect once Daintree is up.")
	}
	// Mirror the cockpit's boot note: a missing model key is a genuine degraded state
	// worth surfacing up front rather than only failing on the first turn. Unlike
	// one-shot, the REPL does NOT exit — read-only slash commands still work.
	if a.Config.FireworksAPIKey == "" {
		r.Warn("FIREWORKS_API_KEY is not set — I can't reach the model. Run `daintree-assistant doctor` to check your setup.")
		r.Line(r.Gray("  Read-only slash commands still work: /tools, /skills, /doctor."))
	}

	for {
		line := ask(r.Cyan("\ndaintree ❯ "))
		if line == "" {
			// EOF (Ctrl-D) returns "" from a closed stream; treat a bare empty as
			// a continue, but a closed reader ends the loop.
			if _, err := reader.Peek(1); err != nil {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "/") {
			res := commands.HandleSlashCommand(base, line, a, r)
			if res.Quit {
				break
			}
			// /clear additionally wipes the host terminal in the REPL.
			cmd, _, _ := splitCmd(line)
			if cmd == "clear" {
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
		if err := runReplTurn(base, a, sigCh, line); err != nil {
			r.Error(err.Error())
		}
	}

	_ = a.Shutdown()
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
	d := a.Router.Describe()
	lines := []string{
		r.Bold("Daintree Assistant") + "  — local operations officer",
		"project   " + a.Config.ProjectPath,
		"mcp       " + mcpLine,
		"models    large=" + basename(d["large"]) + " · small=" + basename(d["small"]),
		"tier      " + string(a.Tier()),
		r.Gray("Type /help for commands. I never edit files directly — I spawn and supervise agents."),
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

// basename returns the model id's last path segment (split("/").pop()).
func basename(model string) string {
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		return model[i+1:]
	}
	return model
}
