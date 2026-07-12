package ui

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/composer"
	"github.com/daintreehq/daintree-assistant/internal/ui/markdown"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// model.go is the root tea.Model state. It
// owns the transcript, the explicit run phase, the work-serialization fields (the
// single-flight lock + mid-turn injection cue + wake queue), the scrollback commit
// queue, view state, and the resize/clear nonces. ALL mutation happens in Update.

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

// pendingQuestion is the in-flight multiple-choice question (user.askMultipleChoice):
// the request, the highlighted option, and the reply channel the tool goroutine blocks
// on. It REPLACES the composer while up. shownAt debounces a typed-ahead answer so a
// keystroke buffered before the sheet appeared can't pick an option the user never saw.
type pendingQuestion struct {
	req      tools.AskChoiceRequest
	selected int
	reply    chan questionReply
	shownAt  int64
}

// Model is the root cockpit model.
type Model struct {
	ctx        context.Context
	app        *app.App
	controller *controller
	pump       *eventPump
	// bootPrefetch is the single connect/discovery future started under the raw splash.
	// bootstrapCmd awaits it off-loop so hand-off never cancels and repeats startup.
	bootPrefetch *bootPrefetch
	theme        theme.Theme
	md           *markdown.Renderer

	// terminal geometry (from tea.WindowSizeMsg).
	columns  int
	rows     int
	embedded bool // under a Daintree xterm (WindowID set) → 2-col gutter

	// view / focus.
	view        viewMode
	expanded    bool     // ^X raw tool detail
	activePanel PanelKey // /watchers,/inbox,/timers,/audit,/help filter
	// Per-deck scroll offsets (top line index). The ops deck and help view can outgrow a
	// short terminal; rather than truncate (the old "resize taller" dead-end), they scroll.
	// Kept SEPARATE so switching ^O↔? preserves each deck's position. Reset to 0 on entry
	// to a deck and clamped to its content height in onKey (see clampWindow / deck routing).
	opsScroll       int
	helpScroll      int
	composer        composer.Model
	pending         *pendingConfirm
	pendingQuestion *pendingQuestion

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
	// footerRows is the live View's last-RENDERED height (line count), written by View() and read by
	// scrollbackChunkRows(). It is a shared pointer so the write survives the Model's value-copy
	// semantics. It lags m's state by exactly one frame — which is precisely the cell-buffer height
	// Bubble Tea's insertAbove uses for a commit fired in the NEXT Update — so reserving it keeps a
	// tea.Println safe even when the footer grew tall to hold a withheld block (bubbletea#1613). nil
	// in headless tests (which never call View); scrollbackChunkRows falls back to the static bound.
	footerRows *int
	// Footer re-pin ledger (repin.go): rows the RENDERED footer has shrunk that no print
	// has slid the view back down over yet. footerPrevRows is the last height the ledger
	// reconciled against; repinCredit is rows our own insertAboves (commit prints, turn
	// flushes) have laid down but whose matching rendered shrink has not been observed
	// yet; repinPending/repinNonce arm at most ONE live re-pin barrier; repinArmedRows
	// is the footer height the pending barrier was armed against (a change re-arms).
	footerDebt     int
	footerPrevRows int
	repinCredit    int
	repinPending   bool
	repinNonce     int
	repinArmedRows int

	// work serialization.
	inFlight   bool   // exactly one Session.Send outstanding
	activeTurn string // id of the live TurnCell (for streaming routing)
	// pendingInject counts messages typed while busy that the Session has buffered but
	// not yet folded into the running turn (InjectPrompt). It drives the composer's
	// "queued for next step" cue; it is incremented on submit-while-busy, decremented
	// on a successful Esc-retract, and zeroed by the inline interjection event (delivery)
	// or on cancel/clear. The buffered TEXT lives in the Session, not here.
	pendingInject int
	pendingWake   []domain.QueueEvent // autonomous wakes drained after a turn settles
	activeWake    []domain.QueueEvent // the burst the in-flight wake turn is reacting to (kept for one retry, #9)
	wakeRetried   bool                // per-burst wake retry budget
	// summarizedTerminals is the cross-burst memory the wake prompt builder reads so a
	// terminal already reported this session is downgraded to a one-line ack (mirrors
	// the host's Host.summarizedTerminals) — recorded only on a real (non-failure) wake.
	summarizedTerminals map[string]struct{}

	// dashboard + usage rollup.
	dashboard Dashboard
	// preview-throttle state. The deck ticks ~1s but terminal previews poll at
	// PreviewPollMS (2500ms), so the off-loop build only re-fetches when
	// now-lastPreviewFetchedAt clears that gate; between fetches it folds the cached
	// tails back in. previewFetchInFlight is single-flight backpressure: a slow MCP
	// fetch must not let a second build pile on behind it on the next tick. Both are
	// advanced in Update from DashboardSnapshotMsg so the reducer never reads the clock.
	lastPreviewFetchedAt int64
	previewFetchInFlight bool
	previewCache         []daemon.TerminalPreview
	cost                 float64
	degraded             bool
	// modelRateLimited latches a provider 429 (retry budget exhausted) as a footer
	// health badge; cleared on the next successful Usage event (the model resumed).
	modelRateLimited bool

	// out-of-band cues.
	clearNonce    int
	redrawNonce   int
	resizePending int  // latest debounce nonce
	sizedOnce     bool // first WindowSizeMsg seen (its redraw is suppressed)
	// True from a resize's commit-disarm until its debounced RedrawMsg runs.
	// CommitArmMsg must NOT re-arm commits inside this window: the arm tick
	// from Init (or a prior redraw's sequence) is timer-ordered, not
	// state-ordered, and re-arming mid-window lets a commit Println against
	// the pre-redraw geometry (#1613) before the nuclear redraw wipes it.
	redrawPending bool
	// Dimensions the pre-program boot hand-off frame was PAINTED at (run.go), or
	// 0×0 when no hand-off frame was painted (splash skipped / non-TTY). The
	// first WindowSizeMsg is only swallowed when it MATCHES these dims — a host
	// that resized the terminal during the splash (an embedded pane hydrating
	// its layout) leaves the painted frame stale, and the mismatch must trigger
	// the nuclear redraw that a plain first-size swallow would skip (onResize).
	handoffCols int
	handoffRows int
	attentionN  int

	// spinner frame (advanced on a periodic tick) for animated active rows. The tick only
	// runs while a turn is in flight (spinnerRunning) — idle, it lapses so the process can
	// quiesce instead of repainting ~10×/s forever.
	spinnerFrame   int
	spinnerRunning bool

	// Slash commands in flight (commands run independently of turn single-flight, so a
	// count not a bool) + the latest stage label a running command reported through the
	// pump (/compact's "Checkpointing conversation…"). While non-zero and no turn is
	// active, the composer shows the stage as its busy cue — a model-backed command
	// (two serial backend calls) must never leave the cockpit looking idle.
	commandsRunning  int
	commandStage     string
	commandStartedAt int64 // NowMS of the most-recent command submit (drives elapsed)

	// shutdown signalling.
	quitting bool

	// staged Ctrl+C (the interrupt-then-quit contract): the first
	// press cancels any in-flight turn (or, when idle, just arms) and surfaces "press
	// again to exit"; a second press within quitArmWindow quits. quitArmGen dedupes the
	// expiry tick so a lapsed timer can't disarm a freshly re-armed prompt.
	quitArmed  bool
	quitArmGen int
}

// newModel builds the root model over an already-constructed App. The composer is
// seeded with the slash palette immediately (boot never gates input).
func newModel(ctx context.Context, a *app.App, pump *eventPump) Model {
	th := theme.Resolve()
	cmp := composer.New(th)
	cmp.SetCommands(paletteCommands())

	m := Model{
		ctx:                 ctx,
		app:                 a,
		pump:                pump,
		theme:               th,
		md:                  markdown.New(th),
		columns:             80, // provisional until the first WindowSizeMsg
		rows:                24,
		footerRows:          new(int),
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
			ProjectName: projectNamePlaceholder,
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
	// clutter under the masthead. There is no model-key boot note anymore — the CLI holds no
	// model credentials (the backend owns them); a turn surfaces a clear backend error if the
	// backend is unreachable.
	return m
}

// UIVersion is stamped into the masthead. Kept as a package var so a build can
// override it; the cli already prints its own version separately.
var UIVersion = "0.1.0"

const projectNamePlaceholder = "Daintree project"

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
// approval sheet AND no pending question sheet. Crucially NOT gated on busy — the
// composer stays editable while a turn runs (a question/approval sheet is the exception:
// it takes the keys until the user decides).
func (m *Model) composerFocus() bool {
	return m.view == viewHome && m.pending == nil && m.pendingQuestion == nil
}

// gutter / chrome / content width helpers measured from the current geometry.
func (m *Model) gutter() int   { return gutterFor(m.embedded) }
func (m *Model) chromeW() int  { return chromeWidth(m.columns, m.gutter()) }
func (m *Model) contentW() int { return contentWidth(m.chromeW()) }
