package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/mcpserver"
	"github.com/daintreehq/assistant/internal/tools"
)

// RunMCPServe is the `mcp --stdio` entry: it serves the assistant itself as an MCP
// server so another agent can drive it as a sub-agent.
//
// The factory below is the interesting part. Unlike every other entry point, it resolves
// configuration PER SESSION rather than once at startup: an MCP client launches this
// process a single time and holds the pipe for its whole session, with no way to restart
// it when the operator wants a different project or backend. Making the binding a
// session argument turns "restart the server" into "close and open a session".
func RunMCPServe(ctx context.Context, opts Options) int {
	// Resolve the process-level defaults ONCE. The auto-approve default in particular
	// cannot be read off opts: a nil Options.AutoApprove means "the environment
	// decides", so inspecting the pointer would silently report false for a process
	// launched with DAINTREE_ASSISTANT_AUTO_APPROVE=1 and then suppress it.
	var defaultAutoApprove bool
	if cfg, err := loadConfigFromOptions(opts); err == nil {
		defaultAutoApprove = cfg.AutoApprove
	}

	// One debug log for the PROCESS, not one per session. debuglog keeps a single
	// package-global active path, so a per-session start would silently redirect every
	// earlier session's writes into the newest session's file and leave the paths those
	// sessions reported pointing at a log that stopped growing. Sessions stay
	// distinguishable inside the one file by their sessionId fields.
	var logOnce sync.Once
	var processLogPath string
	startProcessLog := func(cfg config.AppConfig) string {
		logOnce.Do(func() {
			processLogPath = debuglog.StartDebugLog(
				debuglog.Config{DebugLog: cfg.DebugLog, LogDir: cfg.LogDir},
				map[string]any{"sessionId": "mcp-server", "project": cfg.ProjectPath})
		})
		// Prefer the live value: a log started by an earlier session is the one in use.
		if path := debuglog.CurrentDebugLogPath(); path != "" {
			return path
		}
		return processLogPath
	}

	// Serving on stdout means every diagnostic must go to stderr. A single stray byte on
	// stdout corrupts the JSON-RPC framing and the client drops the connection.
	factory := func(bootstrap, lifetime context.Context, p mcpserver.OpenParams) (mcpserver.Runtime, error) {
		// Start from the process-level options (the .mcp.json env and any flags this
		// process was launched with), then let the session's arguments win. That is the
		// defaults-not-bindings rule: the launch config seeds a session, it does not
		// constrain one.
		sessionOpts := opts
		sessionOpts.Prompt, sessionOpts.HasPrompt = "", false
		applyIfSet(&sessionOpts.Project, p.Project)
		applyIfSet(&sessionOpts.BackendURL, p.BackendURL)
		applyIfSet(&sessionOpts.APIKeyFile, p.APIKeyFile)
		applyIfSet(&sessionOpts.Tier, p.Tier)
		applyIfSet(&sessionOpts.McpURL, p.McpURL)
		applyIfSet(&sessionOpts.McpToken, p.McpToken)
		applyIfSet(&sessionOpts.StateDir, p.StateDir)
		applyIfSet(&sessionOpts.LogDir, p.LogDir)
		// The mode decides both the confirm hook's behaviour and, for "auto", the
		// runtime's own auto-approve (which makes dispatch skip the hook entirely —
		// they are two different layers and both have to agree).
		mode, auto := resolveApprovalMode(p.Approvals, defaultAutoApprove)
		sessionOpts.AutoApprove = &auto
		if p.DebugLog != nil {
			sessionOpts.DebugLog = p.DebugLog
		}
		if tier := sessionOpts.Tier; tier != "" && !domain.Tier(tier).IsValid() {
			return nil, fmt.Errorf("invalid tier %q (choose supervisor, operator, or system)", tier)
		}

		overrides, err := buildOverrides(sessionOpts, render.New(os.Stderr))
		if err != nil {
			return nil, err
		}
		// Take the project's owner lease for the session's whole life, spawning the
		// supervisor daemon like an interactive launch: an MCP-driven session is a
		// long-lived owner, not a probe, and the work it starts deserves to be adopted
		// when the session closes. A short wait so a busy project fails fast with a
		// clear message rather than hanging a tool call.
		own, err := acquireOwnership(bootstrap, overrides, true, 15*time.Second, nil)
		if err != nil {
			return nil, err
		}
		a, err := app.Create(app.CreateOptions{Overrides: overrides})
		if err != nil {
			own.Release()
			return nil, err
		}
		a.AdoptAsCurrentSession()

		logPath := startProcessLog(a.Config)

		// Confirmations go through the session's broker, which is what makes "ask" more
		// than a slogan: it parks the dispatch, surfaces the call with its risk,
		// consequence and redacted args, and fails closed on a timer so a forgotten
		// approval can never pin the turn forever.
		//
		// The event sink is NOT set here. It is per-TURN — each turn records into its own
		// Run — and appRuntime.Send installs it. Wiring one here would be wrong twice
		// over: it would mix turns together, and it would look like the recording is
		// handled when it is not.
		approvals := mcpserver.NewApprovals(mode, p.ApprovalTimeout)
		// runtime is captured by the hook so an approval can name the run it blocks.
		// Assigned below, after the facts are built; the hook only ever reads it on a
		// dispatch, which cannot happen before the runtime exists.
		var runtime interface{ CurrentRunID() string }
		a.SetHooks(app.AppHooks{
			Confirm: func(cctx context.Context, req tools.ConfirmRequest) (bool, error) {
				runID := ""
				if runtime != nil {
					runID = runtime.CurrentRunID()
				}
				return approvals.Confirm(cctx, mcpserver.ApprovalRequest{
					Tool:        req.ToolName,
					Risk:        req.Risk,
					Consequence: req.Consequence,
					Summary:     req.Summary,
					RawArgs:     string(req.Args),
					RunID:       runID,
				}), nil
			},
		})

		st := a.ConnectMcp(bootstrap)
		// LIFETIME, not bootstrap: the SDK cancels a request context the moment its
		// response is sent, so a scheduler started on the open call's context would die
		// before the session had run a single turn — and the async coordinator would keep
		// ACCEPTING work (its started flag survives its parent's cancellation) with no
		// loop left to settle it.
		//
		// The attention callback is deliberately NIL. A non-nil one enables the
		// scheduler's notifier, which marks every attention-or-higher event delivered
		// after invoking it — so a no-op callback would silently consume exactly the
		// async completions daintree.attention exists to hand back. With nil, the
		// notifier stands down and the rows stay unnotified until a caller reads them.
		a.StartScheduler(lifetime, nil)

		facts := mcpserver.RuntimeFacts{
			Project:      a.Config.ProjectPath,
			Tier:         string(a.Tier()),
			BackendURL:   mcp.SanitizeURL(a.Config.BackendURL),
			LogPath:      logPath,
			AutoApprove:  a.Config.AutoApprove,
			ApprovalMode: string(mode),
			MCPConnected: st.Connected,
			MCPTransport: st.Transport,
		}
		rt := mcpserver.NewAppRuntime(a, facts, approvals, own.Release)
		if withRun, ok := rt.(interface{ CurrentRunID() string }); ok {
			runtime = withRun
		}
		return rt, nil
	}

	err := mcpserver.Serve(ctx, mcpserver.Options{
		Version:     buildVersion,
		Factory:     factory,
		Diagnostics: os.Stderr,
	})
	if err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "mcp server:", err)
		return domain.OneShotExitCode.Error
	}
	return domain.OneShotExitCode.Success
}

// resolveApprovalMode decides the session's approval mode and the Config.AutoApprove
// that must accompany it.
//
// The two are separate layers and both have to agree: Config.AutoApprove makes
// tools.Dispatch skip the confirm hook ENTIRELY, while the mode governs what the hook
// does when it IS consulted. Auto-approve is therefore set if and only if the mode is
// "auto" — anything else would either ask nobody while claiming to ask, or park a call
// that dispatch already waved through.
//
// defaultAutoApprove comes from the RESOLVED process config, not from an Options
// pointer: a nil Options.AutoApprove means "the environment decides", so reading the
// pointer would report false for a process launched with
// DAINTREE_ASSISTANT_AUTO_APPROVE=1 and then write an explicit false that suppressed it.
func resolveApprovalMode(requested mcpserver.ApprovalMode, defaultAutoApprove bool) (mcpserver.ApprovalMode, bool) {
	mode := requested
	if mode == "" {
		mode = mcpserver.ApprovalDecline
		if defaultAutoApprove {
			mode = mcpserver.ApprovalAuto
		}
	}
	return mode, mode == mcpserver.ApprovalAuto
}

// applyIfSet overwrites dst only when the session supplied a value, so an unset session
// argument falls back to the process-level default rather than blanking it.
func applyIfSet(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
