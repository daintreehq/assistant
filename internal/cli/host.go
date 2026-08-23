package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
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
		// The embedded host owns the project like any interactive assistant: take
		// the lease (spawning/attaching to the supervisor daemon) before opening
		// the DB. The Ownership handle is deliberately held for the PROCESS
		// lifetime — host teardown os.Exits, and both the flock and the attach
		// connection release on process death, which is exactly the handover the
		// daemon listens for.
		own, err := acquireOwnership(fctx, overrides, true, 60*time.Second, nil)
		if err != nil {
			return nil, err
		}
		a, err := app.Create(app.CreateOptions{
			Overrides:      overrides,
			SessionID:      params.SessionID, // appSessionId: resume id when resuming
			PinnedSkillIDs: opts.PinnedSkillIDs,
		})
		if err != nil {
			own.Release()
			return nil, err
		}
		// Negotiate --skill before the host serves a frame, and before adopting: adoption
		// writes the project's durable current-session pointer and shutdown does not
		// restore the previous value, so a failed preflight would leave the supervisor
		// resuming a session that never ran a turn.
		//
		// The protocol has no warning frame, so both halves of the result go to stderr —
		// the same channel the auto-approve notice below uses, and for the same reason: a
		// condition that changes what every turn means must not be invisible just because
		// the wire cannot carry it.
		pinNotice, perr := a.PreparePinnedSkills(fctx)
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
		// The question hook the host mode previously left nil, which made
		// user.askMultipleChoice fail with QUESTION_UNAVAILABLE on every call even
		// though the tool was advertised to the model.
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
				Question:   req.Question,
				Options:    opts,
				Default:    req.Default,
			})
			if err != nil {
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
	if !commands.IsKnownCommand(line) {
		return host.CommandOutcome{Unknown: true}
	}
	var buf bytes.Buffer
	res := commands.HandleSlashCommand(ctx, line, h.app, render.New(&buf))
	if !res.Handled {
		return host.CommandOutcome{Unknown: true}
	}
	return host.CommandOutcome{Text: buf.String(), Quit: res.Quit, ConversationCleared: res.ConversationCleared}
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
