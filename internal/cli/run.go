package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/cli/jsonout"
	"github.com/daintreehq/daintree-assistant/internal/cli/render"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/projectinstructions"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/mattn/go-isatty"
)

// CockpitRunner is the SEAM the (future) Bubble Tea cockpit registers. main.go
// installs the real runner; until then DefaultCockpitRunner errors so the
// interactive path falls back to the classic REPL. Signature: build the cockpit
// over an already-constructed App and block until it exits.
type CockpitRunner func(ctx context.Context, a *app.App) error

// DefaultCockpitRunner is the stub: the cockpit wave is not built yet.
func DefaultCockpitRunner(context.Context, *app.App) error {
	return errors.New("cockpit not built")
}

// Options are the parsed CLI flags + the one-shot prompt.
type Options struct {
	McpURL    string
	McpToken  string
	Project   string
	Tier      string
	Offline   bool
	Classic   bool
	JSON      bool
	Inline    bool // DEPRECATED NO-OP — accepted and ignored
	Prompt    string
	HasPrompt bool

	// Cockpit is the runner seam (defaults to DefaultCockpitRunner when nil).
	Cockpit CockpitRunner
}

// overridesFromOptions maps routing-irrelevant flags to config overrides. classic/
// inline/json are routing-only and NOT carried into config.
func overridesFromOptions(opts Options) config.ConfigOverrides {
	var o config.ConfigOverrides
	if opts.McpURL != "" {
		v := opts.McpURL
		o.McpURL = &v
	}
	if opts.McpToken != "" {
		v := opts.McpToken
		o.McpToken = &v
	}
	if opts.Project != "" {
		v := opts.Project
		o.ProjectPath = &v
	}
	if opts.Tier != "" {
		v := opts.Tier
		o.Tier = &v
	}
	if opts.Offline {
		v := true
		o.Offline = &v
	}
	return o
}

// buildOverrides resolves the overrides and loads the project DAINTREE.md (best
// effort; a warning is non-fatal). Async-in-TS, here it just reads the file.
func buildOverrides(opts Options, r *render.Renderer) config.ConfigOverrides {
	o := overridesFromOptions(opts)
	projectPath := opts.Project
	if projectPath == "" {
		if cwd, err := os.Getwd(); err == nil {
			projectPath = cwd
		}
	}
	res := projectinstructions.Load(projectPath)
	if res.Warning != "" {
		r.Warn(res.Warning)
	}
	if res.Content != "" {
		c := res.Content
		o.ProjectInstructions = &c
	}
	return o
}

// Run routes per the top-level dispatch:
//
//	prompt        → RunOneShot
//	--json no prompt → usage error (exit 1, stderr)
//	else          → RunInteractive
func Run(ctx context.Context, opts Options) int {
	if opts.HasPrompt {
		return RunOneShot(ctx, opts)
	}
	if opts.JSON {
		fmt.Fprint(os.Stderr, "--json requires a prompt argument (one-shot mode only).\n")
		return 1
	}
	return RunInteractive(ctx, opts)
}

// RunOneShot is the scriptable path. In JSON mode stdout carries
// ONLY the JSONL stream; every human line goes to stderr.
func RunOneShot(ctx context.Context, opts Options) int {
	stderrR := render.New(os.Stderr)
	var sink *jsonout.Sink
	if opts.JSON {
		sink = jsonout.New(os.Stdout, domain.NowMS)
	}

	reportError := func(err error) {
		msg := err.Error()
		if sink != nil {
			sink.Error(msg)
		} else {
			stderrR.Error(msg)
		}
	}

	overrides := buildOverrides(opts, stderrR)
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		reportError(err)
		if sink != nil {
			return sink.Finish()
		}
		return domain.OneShotExitCode.Error
	}

	// Debug log: JSON → stderr notice; else gray stdout notice.
	if path := debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath}); path != "" {
		if sink != nil {
			fmt.Fprintf(os.Stderr, "logging to %s\n", path)
		} else {
			render.Stdout().Line(render.Stdout().Gray("logging to " + path))
		}
	}

	// Preflight: without a model key the turn can only fail on the first model call,
	// so surface that up front (matching the cockpit's boot note) and exit before a
	// dead round-trip, pointing at `doctor` for a fuller setup check.
	if a.Config.DeepSeekAPIKey == "" {
		reportError(errors.New("DEEPSEEK_API_KEY is not set — I can't reach the model. Run `daintree-assistant doctor` to check your setup."))
		_ = a.Shutdown()
		if sink != nil {
			return sink.Finish()
		}
		return domain.OneShotExitCode.Error
	}

	confirm := func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		// One-shot is non-interactive → auto-decline.
		msg := fmt.Sprintf("Skipping %s (%s) — confirmation needed; run interactively to approve.", req.ToolName, req.Risk)
		if sink != nil {
			fmt.Fprint(os.Stderr, "  "+msg+"\n")
		} else {
			stderrR.Warn(msg)
		}
		return false, nil
	}
	logHook := func(m string) {
		if sink != nil {
			fmt.Fprintf(os.Stderr, "  · %s\n", m)
		} else {
			stderrR.Line(stderrR.Gray("  · " + m))
		}
	}

	var events = NewConsoleSink(render.Stdout())
	if sink != nil {
		events = sink
	}
	a.SetHooks(app.AppHooks{AgentEvents: events, Confirm: confirm, Log: logHook})

	runErr := func() error {
		a.ConnectMcp(ctx)
		_, err := a.Session.Send(ctx, opts.Prompt, agent.SendOptions{})
		return err
	}()
	if runErr != nil {
		reportError(runErr)
	}

	// Shutdown BEFORE the terminal result line; route any shutdown error off stdout.
	if serr := a.Shutdown(); serr != nil {
		if sink != nil {
			fmt.Fprintf(os.Stderr, "shutdown error: %v\n", serr)
		} else {
			stderrR.Error("shutdown error: " + serr.Error())
		}
	}

	if sink != nil {
		return sink.Finish()
	}
	if runErr != nil {
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// RunInteractive routes to the cockpit (TTY + !classic) or the classic REPL.
func RunInteractive(ctx context.Context, opts Options) int {
	ttyOK := stdinIsTTY() && stdoutIsTTY()
	wantsCockpit := !opts.Classic && ttyOK

	overrides := buildOverrides(opts, render.Stdout())
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		render.Stdout().Error(err.Error())
		return domain.OneShotExitCode.Error
	}

	if wantsCockpit {
		// Cockpit: open the debug log (header badge shows it, print nothing).
		debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
			map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})
		runner := opts.Cockpit
		if runner == nil {
			runner = DefaultCockpitRunner
		}
		if cerr := runner(ctx, a); cerr != nil {
			// Cockpit unavailable → fall back to the classic REPL.
			render.Stdout().Warn("cockpit unavailable (" + cerr.Error() + ") — falling back to the classic REPL")
			announceDebugLog(a)
			return startRepl(ctx, a)
		}
		_ = a.Shutdown()
		return 0
	}

	announceDebugLog(a)
	return startRepl(ctx, a)
}

// RunDoctor is the TERSE doctor subcommand banner — distinct from
// the rich /doctor slash checklist.
func RunDoctor(ctx context.Context, opts Options) int {
	r := render.Stdout()
	overrides := buildOverrides(opts, r)
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	a.ConnectMcp(ctx)
	st := a.MCP.Status()

	// Track whether any gating check failed so the exit code reflects health instead
	// of always reporting success — scripts and CI gate on `doctor`'s exit code. A
	// missing model key is the one hard failure here; a disconnected MCP is a valid
	// degraded local mode, not a doctor failure.
	anyFail := false

	r.Line("Daintree Assistant — doctor")
	fwk := "present"
	if a.Config.DeepSeekAPIKey == "" {
		fwk = "MISSING — set DEEPSEEK_API_KEY"
		anyFail = true
	}
	r.Line("  deepseek key  : " + fwk)
	mcpURL := a.Config.McpURL
	if mcpURL == "" {
		mcpURL = "(unset)"
	}
	r.Line("  mcp url        : " + mcpURL)
	conn := "not connected"
	if st.Connected {
		count := 0
		if st.ToolCount != nil {
			count = *st.ToolCount
		}
		conn = fmt.Sprintf("ok (%s, %d tools)", st.Transport, count)
	} else if st.Error != "" {
		conn = st.Error
	}
	r.Line("  mcp connection : " + conn)
	r.Line("  project        : " + a.Config.ProjectPath)
	instr := "(none)"
	if a.Config.ProjectInstructions != "" {
		instr = fmt.Sprintf("DAINTREE.md (%d bytes)", len(a.Config.ProjectInstructions))
	}
	r.Line("  instructions   : " + instr)
	r.Line(fmt.Sprintf("  tools loaded   : %d", len(a.Registry.List())))
	r.Line("  tier           : " + string(a.Tier()))

	_ = a.Shutdown()
	if anyFail {
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// announceDebugLog opens the log and prints a gray notice when active.
func announceDebugLog(a *app.App) {
	path := debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})
	if path != "" {
		r := render.Stdout()
		r.Line(r.Gray("logging to " + path))
	}
}

func stdinIsTTY() bool  { return isatty.IsTerminal(os.Stdin.Fd()) }
func stdoutIsTTY() bool { return isatty.IsTerminal(os.Stdout.Fd()) }
