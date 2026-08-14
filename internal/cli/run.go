package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/cli/jsonout"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/credentials"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/projectinstructions"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/tools"
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

// ensureSignedIn gates startup on a usable sign-in. The backend authenticates in
// every environment — there is no unauthenticated mode to degrade into — so a launch
// without a key would otherwise boot a full cockpit that 401s on the first message.
//
// allowPrompt is false wherever prompting would corrupt the output contract or hang:
// one-shot runs (stdout is the answer, stdin may be a pipe), --json, the stdio host,
// and the supervisor daemon. Those get the actionable error instead.
//
// It re-resolves the config rather than taking one from the caller because the gate
// runs BEFORE app.Create — which is the point: the key has to exist before the
// backend client is built. The resolve is cheap and app.Create redoes it against the
// file RunLogin just wrote.
func ensureSignedIn(ctx context.Context, overrides config.ConfigOverrides, allowPrompt bool) error {
	cfg, err := config.LoadConfig(overrides)
	if err != nil {
		return err
	}
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil
	}
	if !allowPrompt {
		// Names what DAINTREE_API_KEY actually IS. A user who takes the env-var route
		// never sees the login flow's disclosure, and "set DAINTREE_API_KEY" on its own
		// reads like a service token rather than a credential OpenRouter bills.
		return fmt.Errorf("not signed in — run `daintree-assistant login` to connect to %s (or set DAINTREE_API_KEY to your own OpenRouter key; %s)",
			cfg.BackendURL, backend.KeyPurposeNotice)
	}
	_, err = RunLogin(ctx, cfg, StdLoginIO())
	return err
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
	// Never prompt here: stdout is the answer channel (or the JSONL stream) and stdin
	// is routinely a pipe. Fail with the instruction instead.
	if err := ensureSignedIn(ctx, overrides, false); err != nil {
		reportError(err)
		if sink != nil {
			return sink.Finish()
		}
		return domain.OneShotExitCode.Error
	}
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

	// AUTO_APPROVE reaches HERE too, and that is easy to miss. One-shot is
	// non-interactive, so it installs an auto-DECLINE confirm hook below — but dispatch
	// skips the hook entirely when AutoApprove is set, because a one-shot run is still
	// the `main` actor. The net effect is that an inherited
	// DAINTREE_ASSISTANT_AUTO_APPROVE=1 makes a scripted run perform tier-allowed
	// mutations with nothing on screen to say so. The cockpit has a persistent badge for
	// exactly this; a scripted run has no footer, so it gets a loud line instead —
	// on stderr, and as a structured event in JSON mode, so neither output contract
	// breaks.
	if a.Config.AutoApprove {
		const warn = "AUTO-APPROVE is ON: mutating actions will run WITHOUT confirmation (tier '%s'). " +
			"Unset DAINTREE_ASSISTANT_AUTO_APPROVE unless this is an automated harness."
		if sink != nil {
			sink.Error(fmt.Sprintf(warn, a.Tier()))
		} else {
			stderrR.Warn(fmt.Sprintf(warn, a.Tier()))
		}
	}

	confirm := func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
		// One-shot is non-interactive → auto-decline. Reached ONLY when AutoApprove is
		// off; see the warning above for why that distinction matters.
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
	// Sign-in gate BEFORE the ownership lease: a login that ends in Ctrl-C should
	// leave no lease behind, and it must not race the daemon handover.
	if err := ensureSignedIn(ctx, overrides, ttyOK); err != nil {
		r.Error(err.Error())
		return domain.OneShotExitCode.Error
	}
	debuglog.BootTrace("boot.signin.ok")
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

// RunDoctor is the `doctor` subcommand: the environment gate.
//
// It builds a structured DoctorReport and renders it as either the human banner or
// `--json`. Every condition is a typed check with an id, a status, and a next action —
// see doctorreport.go for why that replaced prose.
//
// The exit code is the contract: non-zero iff something FAILED. Warnings and unknowns
// never gate, because a gate that fires on "could not check" is a gate people learn to
// ignore.
func RunDoctor(ctx context.Context, opts Options) int {
	report, err := buildDoctorReport(ctx, opts)
	if err != nil {
		if opts.JSON {
			// Even a fatal setup error answers in JSON when JSON was asked for: a caller
			// parsing stdout must never receive prose on the one path it cannot handle.
			fatal := &DoctorReport{Version: buildVersion, Platform: runtime.GOOS + "/" + runtime.GOARCH}
			fatal.Add(DoctorCheck{ID: "doctor.setup", Label: "doctor", Status: StatusFail, Detail: err.Error()})
			fatal.Finalize()
			_ = fatal.WriteJSON(os.Stdout)
		} else {
			render.Stdout().Error(err.Error())
		}
		return domain.OneShotExitCode.Error
	}

	if opts.JSON {
		if werr := report.WriteJSON(os.Stdout); werr != nil {
			render.Stdout().Error(werr.Error())
			return domain.OneShotExitCode.Error
		}
	} else {
		renderDoctorHuman(os.Stdout, report)
	}
	if !report.Summary.Healthy {
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// buildDoctorReport runs every check. It returns an error only when the diagnosis itself
// cannot start (config or App construction) — every other condition is a check.
func buildDoctorReport(ctx context.Context, opts Options) (*DoctorReport, error) {
	// STDERR in JSON mode. buildOverrides prints a warning when DAINTREE.md cannot be
	// read, and `doctor --json` promises stdout is a single JSON document — one
	// unreadable project file would otherwise emit prose ahead of it and break every
	// parser downstream.
	r := render.Stdout()
	if opts.JSON {
		r = render.New(os.Stderr)
	}
	overrides := buildOverrides(opts, r)

	report := &DoctorReport{
		Version:  buildVersion,
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Environment checks first, and BEFORE the lease: they need no App, they are the ones
	// that explain a broken install, and they must still report when the DB cannot open.
	report.Add(CheckPlatform())
	report.Add(CheckBinaryOnPath(buildVersion))

	// Doctor opens the DB, so it needs the lease too (briefly, never spawning). A failure
	// here is itself a finding — "something else owns this project" is exactly what a
	// stuck user needs told — so it becomes a check rather than an abort.
	own, oerr := acquireOwnership(ctx, overrides, false, 10*time.Second, func(string) {})
	if oerr != nil {
		// Deliberately does NOT assert "another process owns it": acquiring the lease can
		// also fail on a read-only mount, a bad state path, or a permissions problem, and
		// naming the wrong cause sends the user to fix something that is not broken.
		report.Add(DoctorCheck{
			ID: "state.owner", Label: "state ownership", Status: StatusFail,
			Detail: "could not take the project's owner lease: " + oerr.Error(),
			Hint:   "Usually another assistant is open — close it, or run `daintree-assistant daemon stop`. If not, check that the state dir is writable.",
		})
		report.Finalize()
		return report, nil
	}
	defer own.Release()
	report.Add(DoctorCheck{
		ID: "state.owner", Label: "state ownership", Status: StatusOK,
		Detail: "acquired (no other assistant is using this project)",
	})

	a, err := app.Create(app.CreateOptions{Overrides: overrides})
	if err != nil {
		// state.schema, not a second id: a stale schema is the overwhelmingly common
		// reason App.Create fails, and a caller keying off the documented id must find it
		// here rather than having to know about a sibling.
		report.Add(DoctorCheck{
			ID: "state.schema", Label: "state database", Status: StatusFail,
			Detail: err.Error(),
			Hint:   "If the schema is stale, run `daintree-assistant reset project-state` (it keeps your sign-in).",
		})
		report.Finalize()
		return report, nil
	}
	defer a.Shutdown()

	report.Add(CheckStateDir(a.Config.StateDir))
	report.Add(CheckCredentialsFile(a.Config.CredentialsPath))
	report.Add(DoctorCheck{
		ID: "state.schema", Label: "state schema", Status: StatusOK,
		Detail: fmt.Sprintf("version %d at %s", storage.SchemaVersion(), a.Config.DBPath),
		Data:   map[string]any{"version": storage.SchemaVersion(), "path": a.Config.DBPath},
	})

	a.ConnectMcp(ctx)

	// One-shot probes: a diagnostic reports the hop's CURRENT state. The patient turn-time
	// retry budget would make a plainly-dead backend take seconds per row to report
	// exactly the same thing.
	ctx = backend.WithoutRetry(ctx)
	for _, c := range backendDoctorChecks(ctx, a) {
		report.Add(c)
	}
	for _, c := range daintreeDoctorChecks(a) {
		report.Add(c)
	}
	report.Add(CheckAutoApprove(a.Config.AutoApprove, string(a.Tier())))
	report.Add(DoctorCheck{
		ID: "tools.registered", Label: "tools", Status: StatusOK,
		Detail: fmt.Sprintf("%d registered, tier '%s'", len(a.Registry.List()), a.Tier()),
		Data:   map[string]any{"count": len(a.Registry.List()), "tier": string(a.Tier())},
	})

	report.Extra = map[string]any{
		"project":     a.Config.ProjectPath,
		"stateDir":    a.Config.StateDir,
		"sessionId":   a.SessionID,
		"debugLog":    a.Config.DebugLog,
		"workflowInt": a.Config.WorkflowIntelligence,
	}
	report.Finalize()
	return report, nil
}

// backendDoctorChecks diagnoses the sign-in and the backend hop.
func backendDoctorChecks(ctx context.Context, a *app.App) []DoctorCheck {
	var out []DoctorCheck

	// Sign-in first: signed out, every authenticated check below fails for one reason,
	// and a wall of 401s reads as a broken backend instead of a missing login.
	if a.Config.APIKey == "" {
		out = append(out, DoctorCheck{
			ID: "auth.signedIn", Label: "signed in", Status: StatusFail,
			Detail: "no API key stored",
			Hint:   "Run `daintree-assistant login`.",
		})
	} else {
		out = append(out, DoctorCheck{
			ID: "auth.signedIn", Label: "signed in", Status: StatusOK,
			Detail: credentials.Redact(a.Config.APIKey),
		})
	}

	hctx, hcancel := context.WithTimeout(ctx, 3*time.Second)
	herr := a.Backend.Health(hctx)
	hcancel()
	// Sanitized for the same reason as the MCP URL: a custom backend URL arrives from the
	// trusted env or the stored sign-in, neither of which passes through
	// credentials.NormalizeBaseURL, so it can carry userinfo.
	base := mcp.SanitizeURL(a.Backend.BaseURL())
	if herr != nil {
		out = append(out, DoctorCheck{
			ID: "backend.reachable", Label: "backend", Status: StatusFail,
			Detail: base + " — UNREACHABLE: " + herr.Error(),
			Hint:   "Check your network. For a local backend, start it: cd ../assistant-backend && python -m daintree_assistant_server",
			Data:   map[string]any{"url": base},
		})
		// Everything below needs the backend; reporting each as its own failure would
		// bury the one cause under four symptoms.
		return out
	}
	out = append(out, DoctorCheck{
		ID: "backend.reachable", Label: "backend", Status: StatusOK,
		Detail: base, Data: map[string]any{"url": base},
	})

	if a.Config.APIKey != "" {
		out = append(out, verifyKeyDoctorCheck(ctx, a, base))
	}
	out = append(out, taskManifestDoctorCheck(ctx, a))
	return out
}

// verifyKeyDoctorCheck asks whether the key actually WORKS — which nothing else here can
// tell you, since our own auth is structural and every other row stays green with a
// bogus key.
func verifyKeyDoctorCheck(ctx context.Context, a *app.App, base string) DoctorCheck {
	c := DoctorCheck{ID: "auth.keyValid", Label: "key valid"}
	vctx, vcancel := context.WithTimeout(ctx, 3*time.Second)
	ver, verr := a.Backend.VerifyKey(vctx)
	vcancel()

	switch {
	case errors.Is(verr, backend.ErrVerifyUnsupported):
		// Must agree with backend.CheckSignIn, or doctor would bless an endpoint `login`
		// refuses: a gating failure for any remote host, a benign gap on loopback.
		if !backend.AllowsUnverifiedSignIn(base) {
			c.Status = StatusFail
			c.Detail = "this backend does not serve /v1/daintree/auth/verify"
			c.Hint = "Your key is fine — the endpoint is out of date or a proxy is intercepting it. Retry off any proxy, or use a Local backend."
			return c
		}
		c.Status = StatusUnknown
		c.Detail = "this local backend can't check"
		return c
	case verr != nil:
		c.Status = StatusUnknown
		c.Detail = "could not check — " + verr.Error()
		return c
	case !ver.Valid:
		c.Status = StatusFail
		c.Detail = "the provider rejected this key: " + ver.Detail
		c.Hint = "Run `daintree-assistant login` with an active, funded key."
		return c
	case ver.LimitRemaining != nil && *ver.LimitRemaining <= 0:
		// Recognised but empty fails every turn just as surely as a wrong key — but the
		// fix is topping up, not re-pasting, so it is its own state.
		c.Status = StatusFail
		c.Detail = "the key is valid but has NO CREDIT remaining"
		c.Hint = "Top up the account — every turn will fail until you do."
		c.Data = map[string]any{"limitRemaining": *ver.LimitRemaining}
		return c
	}
	c.Status = StatusOK
	c.Detail = "yes" + keyLabelSuffix(ver, a.APIKey())
	if ver.LimitRemaining != nil {
		c.Data = map[string]any{"limitRemaining": *ver.LimitRemaining}
	}
	return c
}

// taskManifestDoctorCheck compares the task ids this CLI sends against what the backend
// advertises. Drift is GATING: every id is one the CLI will actually send, so a missing
// one is a guaranteed 404 mid-turn (the 2026-07-07 de-versioning incident, which a
// count-only check could not see).
func taskManifestDoctorCheck(ctx context.Context, a *app.App) DoctorCheck {
	c := DoctorCheck{ID: "backend.tasks", Label: "backend tasks"}
	cctx, ccancel := context.WithTimeout(ctx, 3*time.Second)
	caps, cerr := a.Backend.Capabilities(cctx)
	ccancel()

	if cerr != nil {
		// A capabilities FETCH error is not drift: /v1/daintree/capabilities sits behind
		// require_ready, so a warming backend legitimately yields nothing.
		c.Status = StatusUnknown
		c.Detail = "cannot verify — " + cerr.Error()
		return c
	}
	av := backend.CheckTasks(caps, a.Config.WorkflowIntelligence)
	switch {
	case !av.Reported:
		c.Status = StatusFail
		c.Detail = "the backend advertises NO tasks — every utility task call will fail"
		c.Hint = "The backend is misconfigured or mid-deploy. Check its /v1/daintree/capabilities."
		return c
	case av.OK():
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("all %d required tasks present", av.Required)
		c.Data = map[string]any{"required": av.Required}
		return c
	}
	c.Status = StatusFail
	c.Detail = fmt.Sprintf("DRIFT — %d of %d missing: %s", len(av.Missing), av.Required, strings.Join(av.Missing, ", "))
	c.Hint = "This CLI and the backend disagree about task ids. Update whichever is older."
	c.Data = map[string]any{"missing": av.Missing, "required": av.Required}
	return c
}

// daintreeDoctorChecks diagnoses the Daintree MCP connection and the project binding.
func daintreeDoctorChecks(a *app.App) []DoctorCheck {
	var out []DoctorCheck

	st := a.MCP.Status()
	// SANITIZED, always. Daintree's per-session MCP URL carries its bearer as
	// ?session=<token> (see mcp.SanitizeURL), and this value goes into `doctor --json`
	// and straight into a support bundle — i.e. into a file a tester is being encouraged
	// to send to someone else. The generic redactor cannot save us here: an opaque query
	// token matches no shape, and the field name "url" is not credential-marked, so it
	// would sail through both passes. Endpoints get stripped at the source, never trusted
	// to a downstream scrubber.
	mcpURL := mcp.SanitizeURL(a.Config.McpURL)
	c := DoctorCheck{ID: "mcp.daintree", Label: "Daintree MCP", Data: map[string]any{"url": mcpURL, "connected": st.Connected}}
	switch {
	case a.Config.Offline:
		// Explicitly asked for. Reporting a deliberate choice as a failure would make
		// every offline test run look broken.
		c.Status = StatusSkip
		c.Detail = "offline mode — Daintree MCP not attempted"
	case a.Config.McpURL == "":
		// Not a failure — degraded local mode is a supported way to run — but the
		// assistant cannot do its actual job, so it must not read as healthy either.
		c.Status = StatusWarn
		c.Detail = "not configured — DEGRADED LOCAL MODE"
		c.Hint = "Launch from inside Daintree, or pass --mcp-url/--mcp-token. Without it there are no terminals, agents or worktrees."
	case st.Connected:
		count := 0
		if st.ToolCount != nil {
			count = *st.ToolCount
		}
		c.Status = StatusOK
		c.Detail = fmt.Sprintf("connected (%s, %d tools)", st.Transport, count)
		c.Data["transport"], c.Data["toolCount"] = st.Transport, count
	default:
		c.Status = StatusFail
		c.Detail = "configured but NOT connected: " + st.Error
		c.Hint = "Daintree may have closed or revoked this session. Reopen the assistant from Daintree, or use /reconnect."
	}
	out = append(out, c)

	p := DoctorCheck{ID: "project.instructions", Label: "project", Data: map[string]any{"path": a.Config.ProjectPath}}
	p.Status = StatusOK
	if a.Config.ProjectInstructions != "" {
		p.Detail = fmt.Sprintf("%s (DAINTREE.md, %d bytes)", a.Config.ProjectPath, len(a.Config.ProjectInstructions))
	} else {
		p.Detail = a.Config.ProjectPath + " (no DAINTREE.md)"
	}
	out = append(out, p)
	return out
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
