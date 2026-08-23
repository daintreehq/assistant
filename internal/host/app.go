package host

import (
	"context"
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

	// ConnectMCP attempts the MCP connection. Best-effort: a degraded MCP is NOT a
	// boot failure (it surfaces in prompt context + tool results), so the returned
	// error is informational only — the host logs it to stderr and proceeds.
	ConnectMCP(ctx context.Context) error

	// StartScheduler starts the daemon (watchers/timers tick in-host). onAttention
	// receives each surfaced attention burst; the host filters for actionable wakes.
	StartScheduler(onAttention func(events []domain.QueueEvent))

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
}

// AskChoiceRequest is a multiple-choice question the model needs answered before it can
// continue (user.askMultipleChoice). Mirrored locally rather than imported from
// internal/tools for the same reason ConfirmRequest is: this package compiles in
// isolation against agent/config/domain and must not reach into the tool layer.
type AskChoiceRequest struct {
	// ToolCallID ties the question to its tool call (informational).
	ToolCallID string
	// Question is the human-facing prompt, with no option labels baked in.
	Question string
	// Options are the labelled choices in order. Labels (A, B, C…) are assigned by the
	// CLI, never by the model.
	Options []AskChoiceOption
	// Default is the 0-based index a host should highlight first.
	Default int
}

// AskChoiceOption is one labelled choice.
type AskChoiceOption struct {
	Label string
	Text  string
}

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
	// AskChoice is the question hook: the model asks a finite question and the
	// dispatch blocks until the host answers or the question is cancelled.
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
