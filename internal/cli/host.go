package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
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
