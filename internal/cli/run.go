package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
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
//	--json no prompt → usage error (exit 2, stderr; normally caught by main)
//	else          → RunInteractive
func Run(ctx context.Context, opts Options) int {
	if opts.HasPrompt {
		return RunOneShot(ctx, opts)
	}
	if opts.JSON {
		fmt.Fprint(os.Stderr, "--json requires a prompt argument (one-shot mode only).\n")
		return 2
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
	debuglog.BootTrace("oneshot.overrides.loaded")
	// One-shot takes the owner lease briefly (never spawning a daemon — a script
	// probe must not litter the machine with supervisors). A held lease means a
	// live assistant owns the project: fail loudly instead of double-opening.
	own, err := acquireOwnership(ctx, overrides, false, 10*time.Second,
		func(m string) { fmt.Fprintln(os.Stderr, m) })
	if err != nil {
		reportError(err)
		if sink != nil {
			return sink.Finish()
		}
		return domain.OneShotExitCode.Error
	}
	defer own.Release()
	debuglog.BootTrace("oneshot.ownership.acquired")
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		reportError(err)
		if sink != nil {
			return sink.Finish()
		}
		return domain.OneShotExitCode.Error
	}

	// A debug-log path is diagnostic metadata, never answer content. Keep it on
	// stderr for every one-shot mode so stdout remains empty on a failed human run.
	if path := debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath}); path != "" {
		stderrR.Line(stderrR.Gray("logging to " + path))
	}

	// No model-key preflight: the CLI no longer holds model credentials (the backend
	// owns them). If the backend is unreachable the turn fails with a clear
	// "could not reach assistant backend" error from the backend client.

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

	cs := newOneShotConsoleSink(render.Stdout(), stderrR)
	var events agent.EventSink = cs
	if sink != nil {
		events = sink
	}
	a.SetHooks(app.AppHooks{AgentEvents: events, Confirm: confirm, Log: logHook})

	debuglog.BootTrace("oneshot.app.created")
	runErr := func() error {
		a.ConnectMcp(ctx)
		debuglog.BootTrace("oneshot.mcp.connect.done")
		_, err := a.Session.Send(ctx, opts.Prompt, agent.SendOptions{})
		debuglog.BootTrace("oneshot.send.done")
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

	debuglog.BootTrace("oneshot.shutdown.done")
	if sink != nil {
		return sink.Finish()
	}
	// Session.Send returns turn FAILURES as sentinel replies (not an error — the error
	// return is reserved for the single-flight guard), so a backend-down / model-error
	// turn surfaces as an Error event, not runErr. Gate the exit code on both so a failed
	// one-shot exits non-zero for scripts/CI (the JSON sink does the same via Finish()).
	if runErr != nil || cs.Failed() {
		return domain.OneShotExitCode.Error
	}
	if cs.Cancelled() {
		return domain.OneShotExitCode.Cancelled
	}
	return domain.OneShotExitCode.Success
}

// RunInteractive routes to the cockpit (TTY + !classic) or the classic REPL.
func RunInteractive(ctx context.Context, opts Options) int {
	return runInteractive(ctx, opts, stdinIsTTY() && stdoutIsTTY())
}

// runInteractive is the testable core of RunInteractive. ttyOK is measured at the
// process boundary by RunInteractive; keeping it explicit here lets the cockpit seam
// be exercised without requiring a pseudoterminal in unit tests.
func runInteractive(ctx context.Context, opts Options, ttyOK bool) int {
	wantsCockpit := !opts.Classic && ttyOK

	r := render.Stdout()
	overrides := buildOverrides(opts, r)
	debuglog.BootTrace("boot.overrides.loaded")
	// Interactive launch: ensure the project's supervisor daemon exists, attach
	// (it yields ownership + receives our fresh MCP credentials), and take the
	// owner lease. Closing this assistant later hands supervision straight back
	// — the daemon keeps watchers/async/timers running and integrates results
	// with autonomous wake turns until the next attach.
	own, err := acquireOwnership(ctx, overrides, true, 60*time.Second,
		func(m string) { r.Line(r.Gray(m)) })
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	defer own.Release()
	debuglog.BootTrace("boot.ownership.acquired")
	createOpts := app.CreateOptions{Overrides: overrides}
	// A stale on-disk schema has exactly one sensible recovery for this pre-release,
	// single-baseline DB: hard-reset it. On an interactive terminal (Daintree's xterm)
	// take that automatically instead of prompting — the answer is always "yes" here, so
	// the y/N only added friction to every fresh-folder launch. A piped/non-TTY launch
	// still keeps the loud, actionable error: we never silently wipe local state in an
	// automated context.
	if ttyOK {
		createOpts.OnSchemaStale = schemaAutoReset(r)
		createOpts.OnSchemaReset = schemaResetNotice(r)
	}
	a, err := app.Create(createOpts)
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	debuglog.BootTrace("boot.app.created")
	// This conversation is now the project's current session — the one the
	// daemon's detached wake turns continue after we exit.
	a.AdoptAsCurrentSession()

	if wantsCockpit {
		// Cockpit: open the debug log (header badge shows it, print nothing).
		debuglog.StartDebugLog(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
			map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})
		runner := opts.Cockpit
		if runner == nil {
			runner = DefaultCockpitRunner
		}
		if cerr := runner(ctx, a); cerr != nil {
			// A cancelled launch context is a process shutdown request (SIGTERM on the
			// cockpit path). Bubble Tea reports that cancellation as a runner error; it
			// must not be mistaken for an unavailable cockpit and resurrect the process
			// in a classic REPL detached from the cancelled context.
			if ctx.Err() != nil {
				_ = a.Shutdown()
				return domain.OneShotExitCode.Cancelled
			}
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
	// Doctor opens the DB, so it needs the lease too (briefly, never spawning).
	own, err := acquireOwnership(ctx, overrides, false, 10*time.Second,
		func(m string) { r.Line(r.Gray(m)) })
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	defer own.Release()
	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	a.ConnectMcp(ctx)
	st := a.MCP.Status()

	// Track whether any gating check failed so the exit code reflects health instead
	// of always reporting success — scripts and CI gate on `doctor`'s exit code. An
	// UNREACHABLE backend is the one hard failure here (it is the CLI's model gateway);
	// a disconnected MCP is a valid degraded local mode, not a doctor failure.
	anyFail := false

	r.Line("Daintree Assistant — doctor")
	// One-shot probes: a diagnostic reports the hop's CURRENT state. The patient
	// turn-time retry budget would make a plainly-dead backend take seconds per row
	// to report exactly the same thing.
	ctx = backend.WithoutRetry(ctx)
	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	herr := a.Backend.Health(hctx)
	hcancel()
	backendLine := "ok"
	if herr != nil {
		backendLine = "UNREACHABLE — " + herr.Error()
		anyFail = true
	}
	r.Line("  backend        : " + a.Backend.BaseURL() + " — " + backendLine)

	// Task-ID drift is a GATING failure: every id in the manifest is one this CLI
	// will actually send, so a missing one is a guaranteed runtime 404 mid-turn (the
	// 2026-07-07 de-versioning incident, which a count-only check could not see).
	// A capabilities FETCH error is not a failure — /v1/daintree/capabilities sits
	// behind require_ready, so a warming backend legitimately yields nothing and the
	// honest verdict is "cannot verify".
	cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
	caps, cerr := a.Backend.Capabilities(cctx)
	ccancel()
	switch {
	case cerr != nil:
		r.Line("  backend tasks  : cannot verify — " + cerr.Error())
	default:
		av := backend.CheckTasks(caps, a.Config.WorkflowIntelligence)
		switch {
		case !av.Reported:
			r.Line("  backend tasks  : NONE advertised — every task call will fail")
			anyFail = true
		case av.OK():
			r.Line(fmt.Sprintf("  backend tasks  : ok (%d/%d required present)", av.Required, av.Required))
		default:
			r.Line(fmt.Sprintf("  backend tasks  : DRIFT — %d of %d missing: %s",
				len(av.Missing), av.Required, strings.Join(av.Missing, ", ")))
			anyFail = true
		}
	}

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

// schemaAutoReset returns the app.Create OnSchemaStale handler for the interactive
// terminal. Given what the Daintree Assistant is — a local operations officer whose
// SQLite state is a single clean pre-release baseline that we hard-reset (never migrate)
// on a schema bump — a stale on-disk DB has exactly one sensible recovery, so we take it
// automatically rather than block every fresh-folder launch on a y/N whose answer is
// always "yes". It prints one concise notice (local state reset, previous state kept as
// a backup; code + Daintree untouched) and authorises the rebuild — app.Create then
// moves the old DB aside (never deletes it) and reports the backup path through the
// OnSchemaReset handler (schemaResetNotice). Wired only when stdin/stdout are TTYs
// (see RunInteractive), so a piped/non-TTY launch still keeps the loud, actionable
// stale-schema error rather than silently destroying local state in an automated context.
func schemaAutoReset(r *render.Renderer) func(have, want int) (bool, error) {
	return func(have, want int) (bool, error) {
		r.Line(r.Gray(fmt.Sprintf(
			"Local assistant database was from an older version (schema %d → %d) — resetting local state; your code and Daintree are untouched.",
			have, want)))
		return true, nil
	}
}

// schemaResetNotice returns the app.Create OnSchemaReset handler paired with
// schemaAutoReset: once the stale database has been safely moved aside it names
// the backup path, so the reset never reads as silent data loss — the previous
// state (timers, watchers, memories, history) is right there on disk.
func schemaResetNotice(r *render.Renderer) func(backupPath string) {
	return func(backupPath string) {
		if backupPath == "" {
			return
		}
		r.Line(r.Gray("Previous state backed up to " + backupPath))
	}
}
