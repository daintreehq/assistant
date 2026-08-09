package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/host"
	"github.com/daintreehq/daintree-assistant/internal/supervisor"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// RunHost is the `host --stdio` entry: it builds the embedded-host App factory and
// hands it to host.Run, which serves the stdio NDJSON protocol over os.Stdin/Stdout/
// Stderr until teardown exits the process. The factory builds a real app.App per
// booted session and adapts it onto the host.App seam.
//
// host.Run returns a nonzero code only when its stdin/factory precondition fails
// (terminal stdin, nil factory); the normal path never returns (teardown os.Exits).
func RunHost(ctx context.Context, opts Options) int {
	// Gate before serving. The host speaks NDJSON over stdio with no terminal to prompt
	// on, so a signed-out launch must fail loudly here rather than accept boot requests
	// whose every turn then 401s.
	if err := ensureSignedIn(ctx, overridesFromOptions(opts), false); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return domain.OneShotExitCode.Error
	}
	factory := func(fctx context.Context, params host.AppParams) (host.App, error) {
		overrides := overridesFromOptions(opts)
		// The descriptor's cwd is the authoritative project path; the loaded
		// DAINTREE.md content rides as an override (host.boot already read the file).
		if params.ProjectPath != "" {
			p := params.ProjectPath
			overrides.ProjectPath = &p
		}
		if params.ProjectInstructions != "" {
			pi := params.ProjectInstructions
			overrides.ProjectInstructions = &pi
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
			Overrides: overrides,
			SessionID: params.SessionID, // appSessionId: resume id when resuming
		})
		if err != nil {
			own.Release()
			return nil, err
		}
		a.AdoptAsCurrentSession()
		return &hostAppAdapter{app: a, ctx: fctx, own: own}, nil
	}
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
				ToolName:    req.ToolName,
				Summary:     req.Summary,
				RiskClass:   req.Risk,
				Consequence: req.Consequence,
				RawArgs:     string(req.Args),
			}), nil
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
