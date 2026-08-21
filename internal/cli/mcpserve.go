package cli

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/cli/render"
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
		if p.AutoApprove != nil {
			sessionOpts.AutoApprove = p.AutoApprove
		}
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
		// Never prompt: there is no terminal here, and a blocked prompt would wedge the
		// whole server rather than fail this one session.
		if err := ensureSignedIn(bootstrap, overrides, false); err != nil {
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

		logPath := debuglog.StartDebugLog(
			debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
			map[string]any{"sessionId": a.SessionID, "project": a.Config.ProjectPath})

		// Non-interactive, exactly like a one-shot: with auto-approve off every
		// confirmation is DECLINED rather than parked, because there is no human on this
		// pipe to answer one and a blocked dispatch would hang the session until the
		// caller gave up. The declined call is reported in the run's timeline.
		//
		// The event sink is NOT set here. It is per-TURN — each turn records into its own
		// Run — and appRuntime.Send installs it. Wiring one here would be wrong twice
		// over: it would mix turns together, and it would look like the recording is
		// handled when it is not.
		confirm := func(_ context.Context, req tools.ConfirmRequest) (bool, error) {
			return false, nil
		}
		a.SetHooks(app.AppHooks{Confirm: confirm})

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
			MCPConnected: st.Connected,
			MCPTransport: st.Transport,
		}
		return mcpserver.NewAppRuntime(a, facts, own.Release), nil
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

// applyIfSet overwrites dst only when the session supplied a value, so an unset session
// argument falls back to the process-level default rather than blanking it.
func applyIfSet(dst *string, v string) {
	if v != "" {
		*dst = v
	}
}
