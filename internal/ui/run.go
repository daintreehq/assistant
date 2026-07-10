package ui

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
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
	// linger so it can include that name if it arrived in time; otherwise the stable
	// placeholder remains.
	if cols, rows, ok := terminalSize(os.Stdout); ok {
		m.columns = cols
		m.rows = rows
	}
	debuglog.BootTrace("boot.splash.start")
	bootPrefetch := startBootPrefetch(ctx, a)
	defer bootPrefetch.stop()
	handoffFrame := func(cols, rows int) string {
		debuglog.BootTrace("boot.splash.handoff")
		if name := bootPrefetch.projectName(); name != "" {
			m.masthead.ProjectName = name
		}
		bootPrefetch.backendHandshakeComplete()
		bootPrefetch.stop()
		// Lay the frame out for the terminal as it IS at hand-off time. The splash
		// re-measures every frame (boot_splash.go) — a host resize mid-animation
		// (embedded-pane layout hydration) would otherwise leave the masthead and
		// footer wrapped for a width the terminal no longer has, autowrapping the
		// pre-painted rows and parking Bubble Tea's inline origin on the wrong row.
		if cols > 0 && rows > 0 {
			m.columns = cols
			m.rows = rows
		}
		m.syncComposer()
		return m.bootHandoffFrame()
	}

	// Play the boot animation OUTSIDE Bubble Tea, then start the program with a stable
	// short footer (see boot_splash.go for why this — not a tall animated View() — is
	// the correct pattern for an inline cockpit). No-op on non-TTY / tiny terminals.
	if painted, paintedCols, paintedRows := playBootSplash(ctx, os.Stdout, th, handoffFrame); painted {
		defer io.WriteString(os.Stdout, "\x1b[?25h") // defensive if BT exits before its first cursor restore
		// The hand-off frame includes the masthead above the live footer. Treat it as
		// already committed so the first scrollback commit does not duplicate it; later
		// redraws still reset and recommit through the normal queue path.
		m.queue.headerDone = true
		// Record the dims the frame was painted at: onResize compares the FIRST
		// WindowSizeMsg against these and fires the nuclear redraw on a mismatch
		// (the terminal changed between the hand-off write and BT's size probe).
		m.handoffCols = paintedCols
		m.handoffRows = paintedRows
	} else {
		bootPrefetch.stop()
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
	_, err := prog.Run()

	// Restore a clean window title on exit (Bubble Tea has released the terminal).
	terminal.SetTitle(os.Stdout, "Daintree")
	return err
}

type bootPrefetch struct {
	projectNameDone <-chan string
	backendDone     <-chan error
	cancel          context.CancelFunc
}

func startBootPrefetch(ctx context.Context, a *app.App) *bootPrefetch {
	bootCtx, cancel := context.WithCancel(ctx)
	projectNameDone := make(chan string, 1)
	backendDone := make(chan error, 1)
	go func() {
		projectNameDone <- bootFetchProjectName(bootCtx, a)
	}()
	go func() {
		backendDone <- bootHandshakeBackend(ctx, a)
	}()
	return &bootPrefetch{
		projectNameDone: projectNameDone,
		backendDone:     backendDone,
		cancel:          cancel,
	}
}

func bootFetchProjectName(ctx context.Context, a *app.App) string {
	if a == nil || a.MCP == nil {
		return ""
	}
	if st := a.ConnectMcp(ctx); st.Connected {
		debuglog.BootTrace("boot.mcp.connected")
		for {
			if name := a.ProjectName(); name != "" {
				debuglog.BootTrace("boot.projectname.done")
				return name
			}
			if name := a.MCP.FetchProjectName(ctx); name != "" {
				debuglog.BootTrace("boot.projectname.done")
				return name
			}
			select {
			case <-ctx.Done():
				return ""
			case <-time.After(bootProjectNameRetryDelay):
			}
		}
	}
	return ""
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
	if p == nil || p.projectNameDone == nil {
		return ""
	}
	select {
	case name := <-p.projectNameDone:
		return name
	default:
		return ""
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

const bootProjectNameRetryDelay = 150 * time.Millisecond
const bootBackendHandshakeTimeout = 3 * time.Second
