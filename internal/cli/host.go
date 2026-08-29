package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/commands"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/host"
	"github.com/daintreehq/assistant/internal/supervisor"
	"github.com/daintreehq/assistant/internal/tools"
)

// hostSchemaAutoReset authorises the stale-schema rebuild for the embedded host.
//
// Reported on STDERR rather than the wire: this runs inside app.Create, before the
// session exists and therefore before any frame can be sequenced onto the protocol
// stream — the same channel, and the same reason, as the pinned-runbook and
// auto-approve notices below.
func hostSchemaAutoReset(have, want int) (bool, error) {
	fmt.Fprintf(os.Stderr,
		"daintree-assistant host: local assistant database was from an older version "+
			"(schema %d → %d) — resetting local state; your code and Daintree are untouched\n",
		have, want)
	return true, nil
}

// hostSchemaResetNotice names the backup once the stale database has been moved aside,
// so the reset never reads as silent data loss.
func hostSchemaResetNotice(backupPath string) {
	if backupPath == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "daintree-assistant host: previous state backed up to %s\n", backupPath)
}

// hostOverrides merges one boot descriptor onto the process-level overrides.
//
// The descriptor's cwd is the authoritative project path, and its DAINTREE.md content
// (host.boot already read the file) rides as an override — but only when nothing
// explicit is there, because that content is DISCOVERED exactly like the file
// buildOverrides loads. Without the guard, an operator who launched the host with
// --project-instructions-file would have it silently replaced on every boot.
//
// It returns a COPY: the factory runs once per session, so a merge that wrote through to
// the shared base would leak one session's project into the next.
func hostOverrides(base config.ConfigOverrides, params host.AppParams) config.ConfigOverrides {
	o := base
	if params.ProjectPath != "" {
		p := params.ProjectPath
		o.ProjectPath = &p
	}
	applyAutoProjectInstructions(&o, params.ProjectInstructions)
	return o
}

// hostOwnershipLog narrates lease acquisition on the host's diagnostic stream.
//
// This argument used to be nil, and the silence was the whole failure. Taking the
// project lease blocks for up to 60s when another Daintree already owns the project,
// and the embedded host is the ONE caller that does it with a person watching a
// spinner rather than a terminal they can Ctrl-C. Every other path — the interactive
// CLI, the one-shot runs — already passed a logger; the surface that needed it most
// was the only one that did not, so a contended launch was reported to the human as
// nothing whatsoever until the deadline finally turned it into an error.
//
// stderr, not the transport: this runs inside the App factory, before a session and
// its transport exist, and the host contract already reserves stderr for exactly this
// (`daintree-assistant host: …` — see the pin and auto-approve notices below). Daintree
// reads the stream line by line and puts it in the main log.
func hostOwnershipLog(line string) {
	fmt.Fprintf(os.Stderr, "daintree-assistant host: %s\n", line)
}

// RunHost is the `host --stdio` entry: it builds the embedded-host App factory and
// hands it to host.Run, which serves the stdio NDJSON protocol over os.Stdin/Stdout/
// Stderr until teardown exits the process. The factory builds a real app.App per
// booted session and adapts it onto the host.App seam.
//
// host.Run returns a nonzero code only when its stdin/factory precondition fails
// (terminal stdin, nil factory); the normal path never returns (teardown os.Exits).
func RunHost(ctx context.Context, opts Options) int {
	// Resolved once, before serving: --api-key-file is read here, and an unreadable one
	// must be fatal at the door rather than per boot request.
	baseOverrides, err := overridesFromOptions(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return domain.OneShotExitCode.Error
	}
	factory := func(fctx context.Context, params host.AppParams) (host.App, error) {
		overrides := hostOverrides(baseOverrides, params)
		// CROSS-CHECK THE DESCRIPTOR AGAINST THE ENVIRONMENT, before taking a lease or
		// opening a database. The descriptor is what Daintree believes it opened; the
		// environment is what this runtime actually binds to. Nothing compared them, so
		// two independent statements of the same identity could disagree while both
		// sides reported success — Daintree rendering a conversation as one project's
		// while the runtime spawned agents in another's.
		//
		// Resolved as a PROBE: this must not create a state directory for a session that
		// is about to be refused.
		//
		// A probe FAILURE is fatal, not a reason to skip the check. Swallowing it made
		// the cross-check fail open in exactly the situation where configuration is
		// already suspect — and app.Create can still succeed afterwards on the overrides
		// that were resolved earlier, so the session would run unchecked.
		cfg, cerr := loadProbeConfigFromOptions(withOverrides(opts, overrides))
		if cerr != nil {
			return nil, fmt.Errorf("cannot resolve this session's configuration to check it against the descriptor: %w", cerr)
		}
		if berr := checkDescriptorBinding(params.Declared, cfg); berr != nil {
			return nil, berr
		}
		// The embedded host owns the project like any interactive assistant: take
		// the lease (spawning/attaching to the supervisor daemon) before opening
		// the DB. The Ownership handle is deliberately held for the PROCESS
		// lifetime — host teardown os.Exits, and both the flock and the attach
		// connection release on process death, which is exactly the handover the
		// daemon listens for.
		own, err := acquireOwnership(fctx, overrides, true, 60*time.Second, hostOwnershipLog)
		if err != nil {
			return nil, err
		}
		a, err := app.Create(app.CreateOptions{
			Overrides:        overrides,
			SessionID:        params.SessionID, // appSessionId: resume id when resuming
			PinnedRunbookIDs: opts.PinnedRunbookIDs,
			// The embedded host takes the SAME automatic stale-schema recovery the
			// interactive terminal does (schemaAutoReset), and for the same reason: the
			// pre-release policy hard-resets rather than migrates, so a stale on-disk
			// baseline has exactly one sensible answer and it is always "yes".
			//
			// It was excluded before because the exclusion was written as "scripts /
			// non-TTY", and the host is neither a TTY nor a script — it is THE product
			// surface, driven by a GUI with a human in front of it. Leaving it on the
			// loud-refusal branch meant every existing install died at boot the first
			// time the schema moved, with `host:error` telling the user to run a
			// Makefile target that does not exist in a Daintree install and no way
			// forward from inside the app.
			//
			// Nothing is destroyed either way: app.Create MOVES the old database aside
			// into a timestamped backup directory (BackupDB) and rebuilds, so the
			// previous timers, watchers, memories and history stay on disk.
			OnSchemaStale: hostSchemaAutoReset,
			OnSchemaReset: hostSchemaResetNotice,
		})
		if err != nil {
			own.Release()
			return nil, err
		}
		// Negotiate --runbook before the host serves a frame, and before adopting: adoption
		// writes the project's durable current-session pointer and shutdown does not
		// restore the previous value, so a failed preflight would leave the supervisor
		// resuming a session that never ran a turn.
		//
		// The protocol has no warning frame, so both halves of the result go to stderr —
		// the same channel the auto-approve notice below uses, and for the same reason: a
		// condition that changes what every turn means must not be invisible just because
		// the wire cannot carry it.
		pinNotice, perr := a.PreparePinnedRunbooks(fctx)
		if perr != nil {
			_ = a.Shutdown()
			own.Release()
			return nil, perr
		}
		a.AdoptAsCurrentSession()
		if pinNotice != "" {
			fmt.Fprintf(os.Stderr, "daintree-assistant host: %s\n", pinNotice)
		}
		// Same trap as the one-shot path: the host runs as the `main` actor, so
		// AUTO_APPROVE bypasses its confirmation bridge entirely — Daintree would be
		// driving a runtime that performs tier-allowed mutations without ever asking.
		// stderr, not the NDJSON stream: the protocol has no warning frame, and inventing
		// one would break the transport contract for the sake of a message.
		if a.Config.AutoApprove {
			fmt.Fprintf(os.Stderr,
				"daintree-assistant host: AUTO-APPROVE is ON — mutating actions will run WITHOUT confirmation (tier %q).\n",
				a.Tier())
		}
		return &hostAppAdapter{app: a, ctx: fctx, own: own}, nil
	}
	// Report the engine build in host:ready so Daintree can version-gate without
	// shelling out to `--version` separately.
	host.BuildVersion = buildVersion
	return host.Run(ctx, factory)
}

// hostAppAdapter adapts the concrete *app.App onto the host.App interface. The two
// surfaces differ slightly (return types, a ctx on StartScheduler/Shutdown), so the
// adapter bridges them. ctx is the host run context, threaded into the scheduler.
type hostAppAdapter struct {
	app *app.App
	ctx context.Context
	own *supervisor.Ownership
}

func (h *hostAppAdapter) SetHooks(hooks host.AppHooks) {
	h.app.SetHooks(app.AppHooks{
		AgentEvents: hooks.AgentEvents,
		// Adapt the host's bool-only confirm onto the app's (bool, error) hook: the
		// host hook never errors (a rejection is just false), so wrap it.
		Confirm: func(cctx context.Context, req tools.ConfirmRequest) (bool, error) {
			if hooks.Confirm == nil {
				return false, nil
			}
			return hooks.Confirm(cctx, host.ConfirmRequest{
				ToolName:          req.ToolName,
				ToolKey:           req.ToolKey,
				Summary:           req.Summary,
				RiskClass:         req.Risk,
				Consequence:       req.Consequence,
				RawArgs:           string(req.Args),
				NeedsTypedConfirm: req.NeedsTypedConfirm,
			}), nil
		},
		// The host CAN answer a question now (question:requested / question:answer), so
		// this is no longer nil on the product path — which is what stops
		// user.askMultipleChoice reporting QUESTION_UNAVAILABLE to the one surface a
		// user actually runs.
		AskChoice: func(cctx context.Context, req tools.AskChoiceRequest) (tools.AskChoiceAnswer, error) {
			if hooks.AskChoice == nil {
				return tools.AskChoiceAnswer{}, tools.ErrNoAskChoiceHook
			}
			opts := make([]host.AskChoiceOption, 0, len(req.Options))
			for _, o := range req.Options {
				opts = append(opts, host.AskChoiceOption{Label: o.Label, Text: o.Text})
			}
			ans, err := hooks.AskChoice(cctx, host.AskChoiceRequest{
				ToolCallID: req.ToolCallID,
				Local:      req.Local,
				Question:   req.Question,
				Options:    opts,
				Default:    req.Default,
			})
			if err != nil {
				// Translated at the seam, not passed through. The two packages keep
				// their own sentinels on purpose (internal/host compiles in isolation
				// against agent/config/domain), and without this mapping the tool layer
				// sees an unrecognised error and reports a working question surface as
				// QUESTION_UNAVAILABLE — telling the model to try another route when
				// the user simply declined.
				if errors.Is(err, host.ErrQuestionDismissed) {
					return tools.AskChoiceAnswer{}, tools.ErrQuestionDismissed
				}
				return tools.AskChoiceAnswer{}, err
			}
			return tools.AskChoiceAnswer{Label: ans.Label, Index: ans.Index, Text: ans.Text}, nil
		},
	})
}

// RunCommand routes a slash line through the SAME handler the line REPL uses, with
// the renderer pointed at a buffer instead of stdout. Sharing the handler is the point:
// the registry test asserts every command is served by both surfaces, so an embedded
// host cannot quietly support a different set from the one the CLI documents.
func (h *hostAppAdapter) RunCommand(ctx context.Context, line string) host.CommandOutcome {
	return h.RunCommandWithProgress(ctx, line, nil)
}

// RunCommandWithProgress is RunCommand plus the stage reporter the slow commands use.
// See host.CommandProgressRunner for why this is a second method rather than a wider
// RunCommand signature.
func (h *hostAppAdapter) RunCommandWithProgress(
	ctx context.Context,
	line string,
	progress func(stage string),
) host.CommandOutcome {
	if !commands.IsKnownCommand(line) {
		return host.CommandOutcome{Unknown: true}
	}
	var buf bytes.Buffer
	// STILL the REPL handler, with a progress sink threaded through it — not the UI one.
	// Sharing this handler is the point (the registry test asserts every command is
	// served by both surfaces), and the two are not interchangeable: the UI arm of
	// `/help` appends a key cheat-sheet describing keys an embedded panel does not have,
	// and `/doctor` formats through a different function. A host that asked only for a
	// progress channel must not silently get different output for either.
	//
	// The renderer's own stage lines land in `buf`, which nobody reads until the command
	// has finished; `progress` is what reaches the panel while it is still running.
	res := commands.HandleSlashCommandWithProgress(ctx, line, h.app, render.New(&buf), progress)
	if !res.Handled {
		return host.CommandOutcome{Unknown: true}
	}
	return host.CommandOutcome{Text: buf.String(), Quit: res.Quit, ConversationCleared: res.ConversationCleared}
}

// IsSlowCommand answers host.CommandProgressRunner from the registry — the same table
// that decides everything else about a command.
func (h *hostAppAdapter) IsSlowCommand(line string) bool {
	return commands.IsSlowCommand(line)
}

// The adapter must satisfy the runner interface in full. Asserted at COMPILE TIME
// because the host only ever reaches these methods through a type assertion
// (`h.app.(CommandProgressRunner)`) — an adapter that fell one method short would not
// fail to build, it would silently stop being a runner, and every slow command would
// quietly run inline again. Which, for a command that asks, is a deadlock.
var _ host.CommandProgressRunner = (*hostAppAdapter)(nil)

// IsExclusiveCommand answers host.CommandProgressRunner from the same table.
func (h *hostAppAdapter) IsExclusiveCommand(line string) bool {
	return commands.IsExclusiveCommand(line)
}

// CommandCatalog mirrors the CLI's own registry, so the two surfaces cannot offer
// different command sets.
func (h *hostAppAdapter) CommandCatalog() []host.CommandMeta {
	rows := commands.PaletteRows()
	out := make([]host.CommandMeta, 0, len(rows))
	for _, r := range rows {
		out = append(out, host.CommandMeta{Name: r.Name, Syntax: r.Syntax, Palette: r.Desc})
	}
	return out
}

// Operations builds the deck from the SAME stores the cockpit read, so the two surfaces
// cannot disagree about what is running. Every read is best-effort: a deck that fails to
// open because one table errored is worse than a deck missing one section.
func (h *hostAppAdapter) Operations(ctx context.Context) host.OperationsSnapshot {
	snap := host.OperationsSnapshot{}
	if h.app == nil {
		return snap
	}
	if st := h.app.Store; st != nil {
		if timers, err := st.ListTimers("scheduled"); err == nil {
			for _, t := range timers {
				snap.Timers = append(snap.Timers, host.TimerRow{ID: t.ID, Label: t.Title, DueAt: t.FireAt})
			}
		}
		if ws, err := st.ListLiveWatchers(); err == nil {
			for _, w := range ws {
				state := ""
				if w.LastClassification != nil {
					state = *w.LastClassification
				}
				snap.Agents = append(snap.Agents, host.AgentRow{
					ID: w.ID, Title: w.Title, Goal: w.Goal, AgentState: state,
					StartedAt: w.CreatedAt,
					// The cockpit merged a watcher with its terminal's preview; the
					// preview needs a live MCP read, so it is left to the host, which
					// already has the terminal on screen.
					NeedsAttention: w.Status == "condition_met",
				})
			}
		}
		if async, err := st.ListLiveAsyncInvocations(); err == nil {
			for _, a := range async {
				snap.Async = append(snap.Async, host.AsyncRow{
					ID: a.ID, Title: a.Title, Tool: a.ToolName, StartedAt: a.CreatedAt,
				})
			}
		}
		if audit, err := st.ListAudit(8); err == nil {
			for _, a := range audit {
				snap.Audit = append(snap.Audit, host.AuditRow{
					Tool: a.ToolName, Outcome: a.Outcome, DurationMs: a.DurationMs, At: a.Ts,
				})
			}
		}
	}
	if h.app.Queue != nil {
		atLeast := domain.SeverityAttention
		if inbox, err := h.app.Queue.Digest(ctx, domain.QueueDigestOptions{
			SeverityAtLeast: &atLeast,
		}); err == nil {
			for _, e := range inbox {
				snap.Inbox = append(snap.Inbox, host.InboxRow{
					ID: e.ID, Severity: string(e.Severity), Source: string(e.Source),
					Summary: e.Summary, At: e.CreatedAt,
				})
			}
		}
	}
	return snap
}

func (h *hostAppAdapter) McpStatus() (bool, *int, string) {
	if h.app == nil {
		return false, nil, ""
	}
	if h.app.MCP == nil {
		return false, nil, ""
	}
	st := h.app.MCP.Status()
	return st.Connected, st.ToolCount, st.Error
}

func (h *hostAppAdapter) CostSnapshot() (float64, bool) {
	if h.app == nil || h.app.CostLedger == nil {
		return 0, false
	}
	snap := h.app.CostLedger.Snapshot()
	// LowerBound inverted: the ledger says "this is a floor", the wire says "this is
	// complete". A host renders "≥ $x" when it is not.
	return snap.Observed, !snap.LowerBound
}

func (h *hostAppAdapter) ConnectMCP(ctx context.Context) error {
	// app.ConnectMcp returns a status, not an error; a degraded MCP is non-fatal, so
	// surface its error text (if any) for the host's stderr diagnostic.
	st := h.app.ConnectMcp(ctx)
	if !st.Connected && st.Error != "" {
		return errMCP(st.Error)
	}
	return nil
}

func (h *hostAppAdapter) StartScheduler(onAttention func(events []domain.QueueEvent)) {
	h.app.StartScheduler(h.ctx, onAttention)
}

func (h *hostAppAdapter) RearmAttention(ids []string) error {
	return h.app.RearmAttention(ids)
}

func (h *hostAppAdapter) Session() *agent.Session { return h.app.Session }

func (h *hostAppAdapter) RiskOf(toolName string) (domain.RiskClass, bool) {
	t := h.app.Registry.Get(toolName)
	if t == nil {
		return "", false
	}
	return t.Risk, true
}

func (h *hostAppAdapter) Config() config.AppConfig { return h.app.Config }

func (h *hostAppAdapter) Shutdown(context.Context) error {
	err := h.app.Shutdown()
	// Store closed → hand the lease back so the daemon resumes before our
	// process even exits (teardown os.Exits shortly after anyway).
	h.own.Release()
	return err
}

// errMCP is a tiny error wrapper for an MCP-status error string (avoids importing
// errors solely for one New).
type errMCP string

func (e errMCP) Error() string { return string(e) }

// checkDescriptorBinding compares what the descriptor DECLARED against what this process
// actually resolved from the environment.
//
// Only fields the descriptor genuinely carries are checked, and a blank on either side is
// skipped: the descriptor validates these for type while the live values come from the
// environment, so an absent one means "not stated" rather than "stated as empty".
// Comparing an unstated field would refuse every launch that simply does not inject that
// variable — which, since Daintree injects them all, means this is strict exactly where
// there is something to be strict about.
func checkDescriptorBinding(declared host.Binding, cfg config.AppConfig) error {
	if d, a := strings.TrimSpace(declared.ProjectID), strings.TrimSpace(cfg.ProjectID); d != "" && a != "" && d != a {
		return &host.BindingMismatchError{Field: "projectId", Declared: d, Actual: a}
	}
	if d, a := strings.TrimSpace(declared.WindowID), strings.TrimSpace(cfg.WindowID); d != "" && a != "" && d != a {
		return &host.BindingMismatchError{Field: "windowId", Declared: d, Actual: a}
	}
	if d, a := strings.TrimSpace(declared.Tier), strings.TrimSpace(string(cfg.Tier)); d != "" && a != "" && d != a {
		return &host.BindingMismatchError{Field: "tier", Declared: d, Actual: a}
	}
	// cwd is deliberately NOT compared here, and the reason is worth stating: there is no
	// independent second source for it in this process. hostOverrides derives the
	// config's project path FROM the descriptor's cwd, so comparing the two would be
	// comparing a value against itself — a check that can never fire, which is worse than
	// no check because it makes the cross-check look more complete than it is.
	//
	// The other three fields are genuinely independent: the descriptor states them and
	// the environment states them separately, so they can actually disagree.
	return nil
}

// withOverrides returns opts carrying the already-merged descriptor overrides, so the
// binding probe resolves exactly the configuration the session is about to use.
func withOverrides(opts Options, o config.ConfigOverrides) Options {
	if o.ProjectPath != nil {
		opts.Project = *o.ProjectPath
	}
	return opts
}
