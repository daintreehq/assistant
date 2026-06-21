package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
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
// touches the alternate buffer. Port of terminalClear.ts HOST_TERMINAL_CLEAR.
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

// startRepl runs the classic line REPL (repl.ts; readline → bufio). Returns the
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

	confirm := func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		r.Warn(r.Bold(req.ToolName) + " (" + string(req.Risk) + ") wants to run:\n     " +
			req.Summary + "\n     args: " + render.Truncate(string(req.Args), 200))
		answer := ask(r.Yellow("   approve? [y/N] "))
		a := strings.ToLower(strings.TrimSpace(answer))
		return a == "y" || a == "yes", nil
	}
	logHook := func(m string) { r.Line(r.Gray("  · " + m)) }

	a.SetHooks(app.AppHooks{
		AgentEvents: NewConsoleSink(r),
		Confirm:     confirm,
		Log:         logHook,
	})

	a.ConnectMcp(ctx)
	st := a.MCP.Status()
	a.StartScheduler(ctx, func(events []domain.QueueEvent) { printAttention(r, events) })

	printBanner(r, a, st.Connected, st.Transport)
	if !st.Connected {
		r.Warn("Running in degraded local mode — Daintree MCP not connected. Use /reconnect once Daintree is up.")
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
			res := commands.HandleSlashCommand(ctx, line, a, r)
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
		if _, err := a.Session.Send(ctx, line, agent.SendOptions{}); err != nil {
			r.Error(err.Error())
		}
	}

	_ = a.Shutdown()
	r.Line(r.Gray("Goodbye."))
	return 0
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

// printBanner prints the REPL banner (repl.ts §5.1).
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
		"tier      " + string(a.Config.Tier),
		r.Gray("Type /help for commands. I never edit files directly — I spawn and supervise agents."),
	}
	r.Banner(lines)
}

// printAttention prints out-of-band inbox events, then restores the prompt
// (repl.ts §5.2).
func printAttention(r *render.Renderer, events []domain.QueueEvent) {
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
