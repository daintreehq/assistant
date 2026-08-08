package ui

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/terminal"
)

// run.go is the cockpit entrypoint matching the cli.CockpitRunner seam signature:
// Run(ctx, *app.App) error. It builds the event pump, the controller (which
// subscribes to the session via App hooks), the root model, and the tea.Program on
// the NORMAL screen buffer (no alt screen, no mouse capture, bracketed paste ON),
// runs it, and restores a clean window title on exit.
//
// main.go registers this as `cli.Options.Cockpit = ui.Run` so the TTY path launches
// it (see cmd/daintree-assistant/main.go).
func Run(ctx context.Context, a *app.App) error {
	pump := newEventPump()
	m := newModel(ctx, a, pump)
	th := m.theme

	// Seed the first cockpit frame with the real terminal size when available, then use
	// the splash duration to connect MCP, fetch the authoritative project name, and
	// warm the Assistant backend. The hand-off frame is built at the end of the logo
	// linger so it can include any project name that arrived in time; otherwise the
	// stable placeholder remains.
	if cols, rows, ok := terminalSize(os.Stdout); ok {
		m.columns = cols
		m.rows = rows
	}
	debuglog.BootTrace("boot.splash.start")
	bootPrefetch := startBootPrefetch(ctx, a)
	m.bootPrefetch = bootPrefetch
	defer bootPrefetch.stop()
	handoffFrame := func(cols, rows int) string {
		debuglog.BootTrace("boot.splash.handoff")
		if name := bootPrefetch.projectName(); name != "" {
			m.masthead.ProjectName = name
		}
		bootPrefetch.backendHandshakeComplete()
		// Seed Bubble Tea from the terminal as it is when the splash completes. The splash
		// re-measures every frame because embedded-pane layout can hydrate mid-animation.
		if cols > 0 && rows > 0 {
			m.columns = cols
			m.rows = rows
		}
		m.syncComposer()
		return m.bootHandoffFrame()
	}

	// Play the animation outside Bubble Tea, then replace its last frame atomically with
	// the complete cockpit. Bubble Tea adopts the already-painted footer as its live
	// region, so there is no blank or footer-only frame while the masthead commit barrier
	// runs. The hand-off keeps MCP unresolved (amber) and the composer immediately live.
	if painted, paintedCols, paintedRows := playBootSplash(ctx, os.Stdout, th, handoffFrame); painted {
		defer io.WriteString(os.Stdout, "\x1b[?25h") // defensive if BT exits before its first cursor restore
		// The hand-off already placed the masthead above the live footer. Treat it as
		// committed so the queue cannot print a duplicate.
		m.queue.headerDone = true
		// A real resize between this write and Bubble Tea's first size probe must repair
		// the pre-painted geometry instead of swallowing that first size message.
		m.handoffCols = paintedCols
		m.handoffRows = paintedRows
	} else {
		m.syncComposer()
	}

	// progRef lets the confirm-hook closure reach the program that is created below
	// (the one callback that can't ride the re-armed command, because the runtime
	// goroutine blocks on the resolve channel — we use Program.Send for it, the
	// documented fallback). The controller is a pointer field on
	// the model, so wiring it before NewProgram means the program's stored copy sees
	// it (a pointer copies by reference).
	var prog *tea.Program
	m.controller = newController(a, pump, func(msg tea.Msg) {
		if prog != nil {
			prog.Send(msg)
		}
	})

	prog = tea.NewProgram(
		m,
		tea.WithContext(ctx),
		tea.WithInput(os.Stdin),
		tea.WithOutput(os.Stdout),
	)

	debuglog.BootTrace("boot.program.start")
	finalModel, err := prog.Run()

	// Restore a clean window title on exit (Bubble Tea has released the terminal).
	terminal.SetTitle(os.Stdout, "Daintree")
	// /login quits the program with the flag set on the final model; surface it
	// as the sentinel the interactive launcher acts on (run the blocking login
	// prompt — impossible while Bubble Tea owned the terminal — then relaunch).
	if fm, ok := finalModel.(Model); ok && fm.loginRequested && err == nil {
		return domain.ErrLoginRequested
	}
	return err
}

type bootPrefetch struct {
	mcpDone     chan struct{}
	mcpResult   bootMCPResult
	backendDone <-chan error
	cancel      context.CancelFunc
}

type bootMCPResult struct {
	status      mcp.Status
	projectName string
}

func startBootPrefetch(ctx context.Context, a *app.App) *bootPrefetch {
	bootCtx, cancel := context.WithCancel(ctx)
	backendDone := make(chan error, 1)
	p := &bootPrefetch{
		mcpDone:     make(chan struct{}),
		backendDone: backendDone,
		cancel:      cancel,
	}
	go func() {
		p.mcpResult = bootConnectMCP(bootCtx, a)
		close(p.mcpDone)
	}()
	go func() {
		backendDone <- bootHandshakeBackend(ctx, a)
	}()
	return p
}

// bootConnectMCP owns the ONE automatic connect/discovery attempt for an interactive
// launch. It starts under the logo and is deliberately allowed to outlive the visual
// splash: Bubble Tea's bootstrap command awaits this same result instead of cancelling
// it at hand-off and starting the whole discovery path again. That keeps the cockpit
// interactive after the logo while slow startup reads finish in the background.
func bootConnectMCP(ctx context.Context, a *app.App) bootMCPResult {
	if a == nil || a.MCP == nil {
		return bootMCPResult{}
	}
	st := a.ConnectMcp(ctx)
	result := bootMCPResult{status: st}
	if st.Connected {
		debuglog.BootTrace("boot.mcp.connected")
		// ConnectMcp has already completed the atomic startup-context refresh, so the
		// cached project is the authoritative non-blocking value here. A miss is fine:
		// the post-bootstrap ProjectNameMsg path retains its bounded fallback retries.
		result.projectName = a.ProjectName()
		if result.projectName != "" {
			debuglog.BootTrace("boot.projectname.done")
		}
	}
	return result
}

func bootHandshakeBackend(ctx context.Context, a *app.App) error {
	if a == nil || a.Backend == nil {
		return nil
	}
	started := time.Now()
	hctx, cancel := context.WithTimeout(ctx, bootBackendHandshakeTimeout)
	defer cancel()
	caps, err := a.Backend.Capabilities(hctx)
	logBootBackendHandshake(a, time.Since(started), err)
	if err == nil {
		// The handshake already has the task inventory in hand, so auditing it here
		// costs nothing extra — no second HTTP call, no goroutine. A drifted id set
		// otherwise surfaces only as a 404 mid-turn (the 2026-07-07 incident); this
		// puts it in the session log where a "why did that tool fail" archaeology
		// pass will actually find it. /doctor carries the user-facing row.
		logBackendTaskAudit(a, caps)
	}
	debuglog.BootTrace("boot.backend.handshake")
	return err
}

// logBackendTaskAudit writes a backend.tasks debug-log line whenever the ids this
// CLI depends on cannot all be found in the backend's advertised inventory.
// Silent on a clean or unverifiable audit — a log line every boot would be noise.
func logBackendTaskAudit(a *app.App, caps backend.Capabilities) {
	if a == nil {
		return
	}
	av := backend.CheckTasks(caps, a.Config.WorkflowIntelligence)
	if av.OK() {
		return
	}
	debuglog.LogDebug(debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir}, "backend.tasks", map[string]any{
		"missing":    av.Missing,
		"required":   av.Required,
		"advertised": len(caps.Tasks),
		"detail":     "CLI/backend task-id drift — these task calls will fail at runtime",
	})
}

func logBootBackendHandshake(a *app.App, dur time.Duration, err error) {
	if a == nil {
		return
	}
	fields := map[string]any{
		"baseURL":    "",
		"durationMs": dur.Milliseconds(),
		"ok":         err == nil,
	}
	if a.Backend != nil {
		fields["baseURL"] = a.Backend.BaseURL()
	}
	if err != nil {
		fields["error"] = err.Error()
	}
	debuglog.LogDebug(
		debuglog.Config{DebugLog: a.Config.DebugLog, LogDir: a.Config.LogDir},
		"boot.backend_handshake",
		fields,
	)
}

func (p *bootPrefetch) projectName() string {
	if p == nil || p.mcpDone == nil {
		return ""
	}
	select {
	case <-p.mcpDone:
		return p.mcpResult.projectName
	default:
		return ""
	}
}

// awaitMCP returns the splash-started connect/discovery result. The wait happens in a
// tea.Cmd goroutine, never on the Update loop, so the post-logo composer remains fully
// editable while a slow Daintree read settles.
func (p *bootPrefetch) awaitMCP(ctx context.Context, a *app.App) mcp.Status {
	if p == nil || p.mcpDone == nil {
		if a == nil {
			return mcp.Status{}
		}
		return a.ConnectMcp(ctx)
	}
	select {
	case <-p.mcpDone:
		return p.mcpResult.status
	case <-ctx.Done():
		if a == nil || a.MCP == nil {
			return mcp.Status{}
		}
		return a.MCP.Status()
	}
}

func (p *bootPrefetch) backendHandshakeComplete() bool {
	if p == nil || p.backendDone == nil {
		return false
	}
	select {
	case <-p.backendDone:
		return true
	default:
		return false
	}
}

func (p *bootPrefetch) stop() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

const bootBackendHandshakeTimeout = 3 * time.Second
