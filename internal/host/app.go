package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
)

// App is the SEAM to the full assistant runtime. The host depends on this
// interface and the host/cli wave fills it with the concrete App. The surface
// it needs: wire the agent event sink + confirm hook, connect MCP best-effort,
// start the daemon, drive the session, and shut down.
//
// The host NEVER touches the DB, models, or tools directly — everything flows
// through this seam, so this package compiles in isolation against the providers
// that already exist (agent/config/domain).
type App interface {
	// SetHooks installs the agent event sink (bridge.sink) and the tool-confirm
	// hook (bridge.Confirm). Must be called before ConnectMCP/StartScheduler so a
	// wake or early tool call is bridged.
	SetHooks(hooks AppHooks)

	// RunCommand executes a slash line (e.g. "/status") and returns its printed
	// output, whether it asked to quit, and whether the command was unknown.
	//
	// The host routes commands here rather than sending them to the model, because a
	// command is not conversation: "/clear" as prose produces an answer ABOUT
	// clearing and leaves the conversation intact.
	RunCommand(ctx context.Context, line string) CommandOutcome

	// Operations returns one reading of the operations deck — what the assistant is
	// watching, running and has recently done.
	//
	// Built on demand, as the cockpit built it: pushing every store change to a host
	// that may not be showing the deck is a great deal of traffic for a view nobody is
	// looking at.
	Operations(ctx context.Context) OperationsSnapshot

	// Timers returns the scheduled-timer list on its own — the timer manager's
	// read. Same rows as Operations().Timers, built from the same place, so a host
	// showing both cannot be told two different things.
	//
	// `ok` is false when the store could not be read. It is separate from an empty
	// list because a manager must not tell a user nothing is scheduled on the
	// strength of a failed read.
	Timers(ctx context.Context) (rows []TimerRow, ok bool)

	// TimerOutcomes returns what recently-fired timers did — the queue events the
	// scheduler stamped with a timer id, newest first.
	//
	// Separate from Timers because a fired timer is gone from the schedule list: the
	// two answer "what is queued" and "did the last one work", and only together do
	// they describe a timer's whole life.
	TimerOutcomes(ctx context.Context) []TimerOutcomeRow

	// CancelTimer retires one timer on the USER's behalf and revokes the automation
	// grants scoped to it.
	//
	// It does NOT run the model's timer.cancel tool. That call would dispatch under
	// an actor meaning "the assistant decided to", and the audit log would then
	// record a button press as something the model chose to do. This goes straight
	// to the shared operation and records the row under domain.ActorUser.
	CancelTimer(ctx context.Context, timerID string) TimerCancelOutcome

	// CommandCatalog is the command set this engine will accept.
	CommandCatalog() []CommandMeta

	// McpStatus reports whether the Daintree control plane is reachable and how many
	// tools it offers (nil count when the catalog is cold).
	McpStatus() (connected bool, toolCount *int, errMsg string)

	// CostSnapshot reports what this session has spent so far, in USD, and whether
	// that figure is a total or a floor.
	//
	// Cumulative and session-wide, not per-turn: it includes the utility calls that
	// watchers and background tasks make, which never appear as a turn and which a
	// user has no other way to see.
	CostSnapshot() (total float64, complete bool)

	// ConnectMCP attempts the MCP connection. Best-effort: a degraded MCP is NOT a
	// boot failure (it surfaces in prompt context + tool results), so the returned
	// error is informational only — the host logs it to stderr and proceeds.
	ConnectMCP(ctx context.Context) error

	// StartScheduler starts the daemon (watchers/timers tick in-host). onAttention
	// receives each surfaced attention burst; the host filters for actionable wakes.
	// onTimerFired receives the id of each timer that fires — a separate channel
	// because a fired timer is not an attention event and mostly never becomes one.
	StartScheduler(onAttention func(events []domain.QueueEvent), onTimerFired func(timerID string))

	// RearmAttention durably re-arms delivered-but-unhandled attention events
	// (nulls their notifiedAt in the project store) so the NEXT owner's notify
	// pass re-digests and re-delivers them. Teardown calls it with whatever is
	// left in pendingWake — a burst a shutdown/hibernate-cancelled wake turn
	// requeued, or one that was queued but never started: those events were
	// already marked notified when they were handed to this process, and the
	// in-memory queue dies with it — without the durable re-arm the wake would
	// be silently lost across the restart. Best-effort (the host only logs a
	// failure).
	RearmAttention(ids []string) error

	// Session is the turn engine driven by prompt/wake.
	Session() *agent.Session

	// RiskOf looks up a tool's risk class for the danger hint (false if unknown).
	RiskOf(toolName string) (domain.RiskClass, bool)

	// Config is the resolved runtime config (used for debug-log start).
	Config() config.AppConfig

	// Shutdown tears down the runtime (best-effort; the host exits regardless).
	Shutdown(ctx context.Context) error
}

// turnSession is the narrow slice of *agent.Session the host's turn paths drive
// (command prompts and autonomous wake turns). It exists as a seam so loop tests
// can run the REAL prompt/wake/teardown wiring against a cooperative fake session;
// boot fills it with h.app.Session().
//
// RetractPendingInjection/DiscardPendingInjections exist for the injection-strand
// race: InjectPrompt only BUFFERS — the running turn folds the buffer in at its
// next iteration boundary, but a turn past its FINAL fold check can complete
// before the injection lands, leaving the prompt buffered forever while the host
// has told the parent it was folded. The host therefore reclaims unconsumed
// injections when a turn finishes (and re-dispatches them as a fresh turn), and
// discards them when the turn was aborted. See finishPromptTurn/handlePrompt.
type turnSession interface {
	Send(ctx context.Context, text string, opts agent.SendOptions) (string, error)
	InjectPrompt(text string)
	// RetractPendingInjection removes and returns the most recently buffered
	// injection that has NOT yet been folded into the running turn (LIFO); ok is
	// false when nothing is buffered.
	RetractPendingInjection() (string, bool)
	// DiscardPendingInjections drops every buffered-but-unfolded injection.
	DiscardPendingInjections()
}

// ConfirmRequest is the confirm payload the host bridges to an approval. The App
// adapts its tool-context ConfirmRequest (tools.ConfirmRequest) into this when
// calling the installed hook. Beyond the tool name + summary, it carries the
// display context the approval:requested wire event surfaces so Daintree's
// timeline matches a local host approval: the risk class, the human-readable
// consequence, and the raw args (redacted by the bridge before they cross the
// wire). RiskClass is passed through (not re-derived from the registry) so a
// tool's explicit per-confirm override — e.g. grant.create electing RiskSystem —
// reaches the UI verbatim.
//
// The embedded host does not render its own approval sheet — it delegates the
// decision to its external caller (Daintree), which owns the approval UX. But the
// VERDICT is not delegated: NeedsTypedConfirm is forwarded verbatim on the wire
// (see EvApprovalRequested) so the caller enforces the friction without re-deriving
// which risk classes are irreversible. Leaving it to be inferred from RiskClass
// would fork a security rule into a second codebase, free to drift permissively.
type ConfirmRequest struct {
	ToolName    string
	Summary     string
	RiskClass   domain.RiskClass
	Consequence string
	// RawArgs is the raw JSON args string the model emitted. The bridge redacts it
	// (redactArgs) before emitting the wire event — it never crosses verbatim.
	RawArgs string
	// NeedsTypedConfirm is safety.NeedsTypedConfirm's verdict for this dispatch,
	// forwarded verbatim so the host never re-derives the rule. See EvApprovalRequested.
	NeedsTypedConfirm bool
	// ToolKey is the effective identity the gates were applied to. See
	// tools.ConfirmRequest.ToolKey.
	ToolKey string
}

// CommandOutcome is the result of RunCommand.
type CommandOutcome struct {
	Text    string
	Quit    bool
	Unknown bool
	// ConversationCleared reports that the command ACTUALLY cleared the conversation.
	//
	// It is not inferable from the command line, and a host that infers it corrupts
	// itself: /clear is refused while a turn is in flight (Session.Clear returns
	// ErrTurnInProgress, because clearing would corrupt the streaming snapshot), so a
	// surface that resets on seeing the word "clear" wipes its transcript, tool rows
	// and live state while the engine keeps the conversation and goes on working in it.
	// The user is then talking to a model whose context they can no longer see, and the
	// two disagree about what was said — strictly worse than the refusal it misread.
	ConversationCleared bool
}

// AskChoiceRequest is a multiple-choice question that must be answered before its
// caller can continue — the model mid-turn (user.askMultipleChoice), or the CLI itself
// on a slash command that exists to pick one of a short list. Mirrored locally rather
// than imported from internal/tools for the same reason ConfirmRequest is: this package
// compiles in isolation against agent/config/domain and must not reach into the tool
// layer.
type AskChoiceRequest struct {
	// ToolCallID ties the question to its tool call (informational).
	ToolCallID string
	// Local marks a question the CLI asked on its own account, which belongs to no turn.
	// See tools.AskChoiceRequest.Local: without it the bridge stamps whatever turn is
	// running and the answer lands in a turn that never asked.
	Local bool
	// Question is the human-facing prompt, with no option labels baked in.
	Question string
	// Options are the labelled choices in order. Labels (A, B, C…) are assigned by the
	// CLI, never by the model.
	Options []AskChoiceOption
	// Default is the 0-based index a host should highlight first.
	Default int
}

// AskChoiceOption is one labelled choice. The LABEL is assigned by the engine so
// every surface shows the same letter for the same option.
type AskChoiceOption struct {
	Label string
	Text  string
}

// ErrQuestionDismissed reports that the question WAS put to the user and they closed it
// without choosing. Distinct from a context cancellation, which means the turn went away
// underneath the sheet — the model must be able to tell "you declined to answer" (decide
// without asking) from "the turn was abandoned" (the question was never reached).
var ErrQuestionDismissed = errors.New("host: question dismissed without an answer")

// ErrQuestionBusy reports that another question is already outstanding. The host renders
// ONE sheet, so a second request would replace the first and strand whoever asked it —
// see Bridge.AskChoice. Recoverable: the caller decides without asking, or asks later.
var ErrQuestionBusy = errors.New("host: a question is already waiting for an answer")

// AskChoiceAnswer is the host's selection. Index is 0-based.
type AskChoiceAnswer struct {
	Label string
	Index int
	Text  string
}

// AppHooks bundles the hooks the host installs on the App.
type AppHooks struct {
	// AgentEvents is the bridge sink (agent.EventSink). The session emits through it.
	AgentEvents agent.EventSink
	// Confirm is the tool-confirm hook: a mutating tool calls it and blocks until
	// the approval is decided (true) / rejected / times out (false).
	Confirm func(ctx context.Context, req ConfirmRequest) bool
	// AskChoice is the question hook: a finite question is put to the host and the
	// caller blocks until it answers or the question is cancelled.
	//
	// It exists because the HOST is the product surface, and without it the one thing
	// a user actually runs was the one surface that could not be asked a question —
	// user.askMultipleChoice returned QUESTION_UNAVAILABLE and the model had to guess
	// or give up in prose, while the developer-only line REPL could ask freely.
	//
	// An error is a cancellation, never a default answer: an unanswered approval can
	// safely resolve to "no", but an unanswered QUESTION has no safe answer at all.
	AskChoice func(ctx context.Context, req AskChoiceRequest) (AskChoiceAnswer, error)
}

// AppFactory builds the App for a booted session. MCP url/token/tier/projectId
// come from env via loadConfig, NOT the descriptor. The host/cli wave provides
// the concrete factory; the host stores it so it can be tested with a fake.
type AppFactory func(ctx context.Context, params AppParams) (App, error)

// AppParams is the descriptor-derived input to AppFactory. appSessionId is
// resumeSessionId ?? sessionId (so resumed conversation state replays).
type AppParams struct {
	SessionID           string // appSessionId: resume id when resuming, else session id
	ProjectPath         string // descriptor.cwd → overrides.projectPath
	ProjectInstructions string // loaded DAINTREE.md content → overrides

	// Declared is what the DESCRIPTOR claimed this session is bound to. The live
	// binding comes from the environment, so these are the host's opportunity to check
	// that the two agree — see Binding.
	Declared Binding
}

// Binding is the identity a session is bound to: which project, which window, at what
// permission tier, in which directory.
//
// It exists because the descriptor and the environment are two independent statements
// of the same fact, and nothing compared them. The descriptor is what Daintree BELIEVES
// it opened; the environment is what the runtime actually USES — so a mismatch means the
// two processes disagree about which project this session can act on, while both report
// success. Redundant identity is only worth carrying if it is cross-checked; otherwise
// it is two truths and no way to tell which one is live.
type Binding struct {
	ProjectID string
	WindowID  string
	Tier      string
	Cwd       string
}

// BindingMismatchError is a descriptor that disagrees with the effective environment.
//
// It is FATAL rather than a warning. The alternative is a session that runs under a
// binding its host does not know about: Daintree renders the conversation as belonging
// to one project while the runtime spawns agents in another, and every terminal that
// appears is attributed to the wrong window. There is no safe way to guess which of the
// two was meant.
type BindingMismatchError struct {
	Field    string
	Declared string
	Actual   string
}

func (e *BindingMismatchError) Error() string {
	return fmt.Sprintf(
		"session descriptor %s is %q but this process is bound to %q — the host and the runtime disagree "+
			"about which session this is, so neither can be trusted to act on it",
		e.Field, e.Declared, e.Actual)
}

// CommandProgressRunner is an App whose commands can report progress while they run.
//
// OPTIONAL, and separate from App on purpose: every existing implementation — including
// the conformance fakes — satisfies App without it, and a command that returns promptly
// has nothing to report. The host type-asserts for it and falls back to RunCommand.
//
// It exists for the commands marked Slow in the registry. `/login` opens a browser and
// then waits up to five minutes for a loopback callback; without a progress channel the
// surface shows nothing at all for the entire part of that which requires the user to go
// and do something in another window.
type CommandProgressRunner interface {
	// RunCommandWithProgress is RunCommand plus a stage reporter. progress is called
	// with short human-readable lines and may be called from the calling goroutine only.
	RunCommandWithProgress(ctx context.Context, line string, progress func(stage string)) CommandOutcome

	// IsExclusiveCommand reports whether a slash line takes the session for itself —
	// the commands that reserve it while waiting on a user decision.
	//
	// The host refuses prompts and other commands while one is running, rather than
	// admitting them and letting the runtime refuse them a moment later: an admitted
	// prompt has already started a turn and already been echoed, so failing it there
	// reads as the engine breaking rather than as the session being busy.
	IsExclusiveCommand(line string) bool

	// IsSlowCommand reports whether a slash line names a command that may block for
	// longer than the command loop can afford to stop for.
	//
	// Answered by the App rather than read from the registry directly, because the
	// registry lives in a package that imports this one — asking through the interface
	// is what keeps that arrow pointing one way.
	IsSlowCommand(line string) bool
}
