package ui

import (
	"context"
	"io"
	"os"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
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
	// the splash duration to connect MCP and fetch the authoritative project name. The
	// hand-off frame is built at the end of the logo linger so it can include that name
	// if it arrived in time; otherwise the stable placeholder remains.
	if cols, rows, ok := terminalSize(os.Stdout); ok {
		m.columns = cols
		m.rows = rows
	}
	bootPrefetch := startBootMCPPrefetch(ctx, a)
	defer bootPrefetch.stop()
	handoffFrame := func() string {
		if name := bootPrefetch.projectName(); name != "" {
			m.masthead.ProjectName = name
		}
		bootPrefetch.stop()
		m.syncComposer()
		return m.bootHandoffFrame()
	}

	// Play the boot animation OUTSIDE Bubble Tea, then start the program with a stable
	// short footer (see boot_splash.go for why this — not a tall animated View() — is
	// the correct pattern for an inline cockpit). No-op on non-TTY / tiny terminals.
	if playBootSplash(ctx, os.Stdout, th, handoffFrame) {
		defer io.WriteString(os.Stdout, "\x1b[?25h") // defensive if BT exits before its first cursor restore
		// The hand-off frame includes the masthead above the live footer. Treat it as
		// already committed so the first scrollback commit does not duplicate it; later
		// redraws still reset and recommit through the normal queue path.
		m.queue.headerDone = true
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

	_, err := prog.Run()

	// Restore a clean window title on exit (Bubble Tea has released the terminal).
	terminal.SetTitle(os.Stdout, "Daintree")
	return err
}

type bootMCPPrefetch struct {
	done   <-chan bootMCPPrefetchResult
	cancel context.CancelFunc
}

type bootMCPPrefetchResult struct {
	projectName string
}

func startBootMCPPrefetch(ctx context.Context, a *app.App) *bootMCPPrefetch {
	bootCtx, cancel := context.WithCancel(ctx)
	ch := make(chan bootMCPPrefetchResult, 1)
	go func() {
		var res bootMCPPrefetchResult
		if st := a.ConnectMcp(bootCtx); st.Connected {
			for {
				if name := a.MCP.FetchProjectName(bootCtx); name != "" {
					res.projectName = name
					break
				}
				select {
				case <-bootCtx.Done():
					ch <- res
					return
				case <-time.After(bootProjectNameRetryDelay):
				}
			}
		}
		ch <- res
	}()
	return &bootMCPPrefetch{done: ch, cancel: cancel}
}

func (p *bootMCPPrefetch) projectName() string {
	if p == nil || p.done == nil {
		return ""
	}
	select {
	case res := <-p.done:
		return res.projectName
	default:
		return ""
	}
}

func (p *bootMCPPrefetch) stop() {
	if p != nil && p.cancel != nil {
		p.cancel()
	}
}

const bootProjectNameRetryDelay = 150 * time.Millisecond
