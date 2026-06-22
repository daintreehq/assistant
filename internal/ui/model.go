package ui

import (
	"context"
	"path/filepath"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// model.go is the root tea.Model state. It
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

	// shownAt stamps when the sheet appeared, so a typed-ahead / buffered affirmative
	// can be debounced for a short window (the accidental-approve guard).
	shownAt int64
	// requireType marks the highest-risk actions (system / git history-rewrite): single-
	// key approval is disabled and the user must type confirmInput == confirmPhrase.
	requireType  bool
	confirmInput string
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

	// approvedTools is the session "don't ask again for this tool" allow-list (set by the
	// approval sheet's A / F actions, consulted in onApprovalRequested). The value is a
	// REMAINING-COUNT: a missing key or 0 means "ask"; a positive n means "auto-approve n
	// more times then re-prompt" (A, bounded); allowForeverCount (-1) means "for the whole
	// session" (F, unbounded). In-memory and session-scoped — never persisted — and never
	// holds a git/system tool (rememberable()).
	approvedTools map[string]int

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

	// work serialization.
	inFlight    bool                // exactly one Session.Send outstanding
	activeTurn  string              // id of the live TurnCell (for streaming routing)
	queuedInput []queuedTurn        // FIFO user follow-ups typed while busy
	pendingWake []domain.QueueEvent // autonomous wakes drained after the user queue
	activeWake  []domain.QueueEvent // the burst the in-flight wake turn is reacting to (kept for one retry, #9)
	wakeRetried bool                // per-burst wake retry budget
	// summarizedTerminals is the cross-burst memory the wake prompt builder reads so a
	// terminal already reported this session is downgraded to a one-line ack (mirrors
	// the host's Host.summarizedTerminals) — recorded only on a real (non-failure) wake.
	summarizedTerminals map[string]struct{}

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

	// spinner frame (advanced on a periodic tick) for animated active rows. The tick only
	// runs while a turn is in flight (spinnerRunning) — idle, it lapses so the process can
	// quiesce instead of repainting ~10×/s forever.
	spinnerFrame   int
	spinnerRunning bool

	// shutdown signalling.
	quitting bool

	// staged Ctrl+C (the interrupt-then-quit contract): the first
	// press cancels any in-flight turn (or, when idle, just arms) and surfaces "press
	// again to exit"; a second press within quitArmWindow quits. quitArmGen dedupes the
	// expiry tick so a lapsed timer can't disarm a freshly re-armed prompt.
	quitArmed  bool
	quitArmGen int
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
		ctx:                 ctx,
		app:                 a,
		pump:                pump,
		theme:               th,
		md:                  markdown.New(th),
		columns:             80, // provisional until the first WindowSizeMsg
		rows:                24,
		embedded:            a.Config.WindowID != "",
		view:                viewHome,
		composer:            cmp,
		summarizedTerminals: map[string]struct{}{},
		// The splash is played BEFORE the program starts (see boot_splash.go), so the
		// program begins already in the cockpit: View() is the short footer from frame
		// one, and the masthead commits cleanly to scrollback (no tall-View handoff).
		booting: false,
		masthead: mastheadParams{
			Version:     UIVersion,
			ProjectName: provisionalName,
			Tier:        a.Tier(),
			Logging:     a.Config.DebugLog,
			// The debug log is opened (StartDebugLog) on the interactive path BEFORE
			// this runner is invoked, so the active path is already resolvable here —
			// surface it in the badge so the operator sees exactly where the trace
			// goes. Empty when logging is off; the badge is gated on Logging anyway.
			LogFile: debuglog.CurrentDebugLogPath(),
		},
	}
	m.splash = newSplash(m.columns)

	// No standalone welcome line: the composer placeholder ("Ask Daintree… · / for commands"),
	// the hint row, and "?" for help already cover discoverability, so a banner would just be
	// clutter under the masthead. We DO surface a genuine degraded state up front, though:
	// a missing model key at boot rather than only failing on the first turn (clig.dev).
	if a.Config.FireworksAPIKey == "" {
		m.transcript = append(m.transcript, TranscriptCell{Note: &NoteCell{
			ID: domain.NewID("note_"), Level: NoteError, Ts: domain.NowMS(),
			Text: "FIREWORKS_API_KEY is not set — I can't reach the model. Run `daintree-assistant doctor` to check your setup.",
		}})
	}
	return m
}

// UIVersion is stamped into the masthead. Kept as a package var so a build can
// override it; the cli already prints its own version separately.
var UIVersion = "0.1.0"

// paletteCommands maps the command registry's entries into composer.Command rows so
// the palette can't drift from the handlers that accept them.
func paletteCommands() []composer.Command {
	rows := commands.PaletteRows()
	out := make([]composer.Command, 0, len(rows))
	for _, r := range rows {
		out = append(out, composer.Command{Name: r.Name, Desc: r.Desc, Syntax: r.Syntax})
	}
	return out
}

// composerFocus reports whether the composer owns keys: home view AND no pending
// approval sheet. Crucially NOT gated on busy — the composer
// stays editable while a turn runs.
func (m *Model) composerFocus() bool {
	return m.view == viewHome && m.pending == nil
}

// gutter / chrome / content width helpers measured from the current geometry.
func (m *Model) gutter() int   { return gutterFor(m.embedded) }
func (m *Model) chromeW() int  { return chromeWidth(m.columns, m.gutter()) }
func (m *Model) contentW() int { return contentWidth(m.chromeW()) }
