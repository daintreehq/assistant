package ui

import (
	"context"
	"os"

	tea "charm.land/bubbletea/v2"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/terminal"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
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
	// Play the boot animation OUTSIDE Bubble Tea, then start the program with a stable
	// short footer (see boot_splash.go for why this — not a tall animated View() — is
	// the correct pattern for an inline cockpit). No-op on non-TTY / tiny terminals.
	playBootSplash(ctx, os.Stdout, theme.Resolve())

	pump := newEventPump()
	m := newModel(ctx, a, pump)

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
