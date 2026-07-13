package ui

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
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
	// warm the Assistant backend. At the end of the logo linger, fold in any project
	// name and final dimensions that arrived in time; otherwise keep the stable defaults.
	if cols, rows, ok := terminalSize(os.Stdout); ok {
		m.columns = cols
		m.rows = rows
	}
	debuglog.BootTrace("boot.splash.start")
	bootPrefetch := startBootPrefetch(ctx, a)
	m.bootPrefetch = bootPrefetch
	defer bootPrefetch.stop()
	finishSplash := func(cols, rows int) {
		debuglog.BootTrace("boot.splash.complete")
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
	}

	// Play the boot animation OUTSIDE Bubble Tea, then leave a clean viewport and let
	// Bubble Tea establish the inline origin itself. Pre-painting the cockpit and parking
	// the cursor at an absolute footer row made the renderer's origin depend on host
	// geometry that xterm can reflow during Daintree's project-load reveal pass. Once
	// the program starts, Bubble Tea exclusively owns the live region and the normal
	// commit queue places the masthead above it.
	playBootSplash(ctx, os.Stdout, th, finishSplash)
	defer io.WriteString(os.Stdout, "\x1b[?25h")
	m.syncComposer()

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
	_, err := prog.Run()

	// Restore a clean window title on exit (Bubble Tea has released the terminal).
	terminal.SetTitle(os.Stdout, "Daintree")
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
	_, err := a.Backend.Capabilities(hctx)
	logBootBackendHandshake(a, time.Since(started), err)
	debuglog.BootTrace("boot.backend.handshake")
	return err
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
