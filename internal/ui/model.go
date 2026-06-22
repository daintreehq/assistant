package ui

import (
	"context"
	"path/filepath"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// model.go is the root tea.Model state (ui-input.md §7.3, ui-transcript.md §12). It
// owns the transcript, the explicit run phase, the work-serialization slice (the
// single-flight lock + FIFO queue + wake queue), the scrollback commit queue, view
// state, and the resize/clear nonces. ALL mutation happens in Update.

// viewMode is the active top-level surface.
type viewMode int

const (
	viewHome viewMode = iota
	viewOperations
	viewHelp
)

// pendingConfirm is the in-flight approval: the request + the resolve channel the
// runtime goroutine blocks on. showArgs is reset when a new request takes the sheet.
type pendingConfirm struct {
	req      tools.ConfirmRequest
	resolve  chan bool
	showArgs bool
}

// Model is the root cockpit model.
type Model struct {
	ctx        context.Context
	app        *app.App
	controller *controller
	pump       *eventPump
	theme      theme.Theme
	md         *markdown.Renderer

	// terminal geometry (from tea.WindowSizeMsg).
	columns  int
	rows     int
	embedded bool // under a Daintree xterm (WindowID set) → 2-col gutter

	// view / focus.
	view        viewMode
	expanded    bool     // ^X raw tool detail
	activePanel PanelKey // /watchers,/inbox,/timers,/audit,/help filter
	composer    composer.Model
	pending     *pendingConfirm

	// boot splash overlay (never gates input).
	//
	// The boot hand-off is a 3-GATE LOCK matching the original controller
	// (useDaintreeController): booting flips false ONLY once startupSettled (MCP
	// connect resolved — connected or degraded — and the first dashboard snapshot is
	// in) AND animationDone (the splash draw + linger finished) AND projectSettled
	// (the authoritative project name resolved, the link is down, or the retries gave
	// up) are ALL true. A fast connect can't cut the animation short and a slow one
	// can't flash a half-built cockpit; the 8s bootCap is the backstop so a hung
	// startup never strands the user on the splash. The masthead is committed to
	// scrollback only AFTER the hand-off (see scheduleCommit), exactly like the
	// original withholding the header (booting ? null) until the cockpit is up.
	booting        bool
	splash         splashModel
	startupSettled bool
	animationDone  bool
	projectSettled bool
	mcpResolved    bool // MCP connect settled (connected or degraded) — half of startupSettled
	bootSnapshotIn bool // the first dashboard snapshot landed — the other half

	// transcript + scrollback commit queue.
	transcript  []TranscriptCell
	queue       scrollbackQueue
	masthead    mastheadParams
	commitArmed bool // first scrollback commit deferred one render cycle (see scheduleCommit)

	// work serialization (§6.3/§6.4).
	inFlight    bool                // exactly one Session.Send outstanding
	activeTurn  string              // id of the live TurnCell (for streaming routing)
	queuedInput []queuedTurn        // FIFO user follow-ups typed while busy
	pendingWake []domain.QueueEvent // autonomous wakes drained after the user queue
	activeWake  []domain.QueueEvent // the burst the in-flight wake turn is reacting to (kept for one retry, #9)
	wakeRetried bool                // per-burst wake retry budget

	// dashboard + usage rollup.
	dashboard  Dashboard
	hasUsage   bool
	contextPct int
	cost       float64
	model      string
	degraded   bool

	// out-of-band cues.
	clearNonce    int
	redrawNonce   int
	resizePending int  // latest debounce nonce
	sizedOnce     bool // first WindowSizeMsg seen (its redraw is suppressed)
	attentionN    int

	// spinner frame (advanced on a periodic tick) for animated active rows.
	spinnerFrame int

	// shutdown signalling.
	quitting bool
}

// queuedTurn is one queued user follow-up: the prompt text + its visible TurnCell
// id (a dimmed queued turn shown immediately, promoted in place when it starts).
type queuedTurn struct {
	prompt string
	cellID string
}

// newModel builds the root model over an already-constructed App. The composer is
// seeded with the slash palette immediately (boot never gates input).
func newModel(ctx context.Context, a *app.App, pump *eventPump) Model {
	th := theme.Resolve()
	cmp := composer.New(th)
	cmp.SetCommands(paletteCommands())

	provisionalName := filepath.Base(a.Config.ProjectPath)

	m := Model{
		ctx:      ctx,
		app:      a,
		pump:     pump,
		theme:    th,
		md:       markdown.New(th),
		columns:  80, // provisional until the first WindowSizeMsg
		rows:     24,
		embedded: a.Config.WindowID != "",
		view:     viewHome,
		composer: cmp,
		// The splash is played BEFORE the program starts (see boot_splash.go), so the
		// program begins already in the cockpit: View() is the short footer from frame
		// one, and the masthead commits cleanly to scrollback (no tall-View handoff).
		booting: false,
		masthead: mastheadParams{
			Version:     UIVersion,
			ProjectName: provisionalName,
			Tier:        a.Config.Tier,
			Logging:     a.Config.DebugLog,
		},
	}
	m.splash = newSplash(m.columns)
	return m
}

// UIVersion is stamped into the masthead. Kept as a package var so a build can
// override it; the cli already prints its own version separately.
var UIVersion = "0.1.0"

// paletteCommands maps the command registry's entries into composer.Command rows so
// the palette can't drift from the handlers that accept them.
func paletteCommands() []composer.Command {
	entries := commands.PaletteEntries()
	out := make([]composer.Command, 0, len(entries))
	for _, e := range entries {
		out = append(out, composer.Command{Name: e[0], Desc: e[1]})
	}
	return out
}

// composerFocus reports whether the composer owns keys: home view AND no pending
// approval sheet (ui-input.md §7.3). Crucially NOT gated on busy — the composer
// stays editable while a turn runs.
func (m *Model) composerFocus() bool {
	return m.view == viewHome && m.pending == nil
}

// gutter / chrome / content width helpers measured from the current geometry.
func (m *Model) gutter() int   { return gutterFor(m.embedded) }
func (m *Model) chromeW() int  { return chromeWidth(m.columns, m.gutter()) }
func (m *Model) contentW() int { return contentWidth(m.chromeW()) }
