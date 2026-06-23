package host

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// App is the SEAM to the full assistant runtime. The host depends on this
// interface and the cockpit/cli wave fills it with the concrete App. The surface
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

	// Session is the turn engine driven by prompt/wake.
	Session() *agent.Session

	// ConsumeSessionEndedNote clears the one-time "watchers stopped because the prior
	// session ended" NOTE after the first interactive prompt turn, so it surfaces once
	// rather than on every host turn. No-op when there was no such NOTE.
	ConsumeSessionEndedNote()

	// RiskOf looks up a tool's risk class for the danger hint (false if unknown).
	RiskOf(toolName string) (domain.RiskClass, bool)

	// Config is the resolved runtime config (used for debug-log start).
	Config() config.AppConfig

	// Shutdown tears down the runtime (best-effort; the host exits regardless).
	Shutdown(ctx context.Context) error
}

// ConfirmRequest is the confirm payload the host bridges to an approval. The App
// adapts its tool-context ConfirmRequest (tools.ConfirmRequest) into this when
// calling the installed hook. Beyond the tool name + summary, it carries the
// display context the approval:requested wire event surfaces so Daintree's
// timeline matches a local cockpit approval: the risk class, the human-readable
// consequence, and the raw args (redacted by the bridge before they cross the
// wire). RiskClass is passed through (not re-derived from the registry) so a
// tool's explicit per-confirm override — e.g. grant.create electing RiskSystem —
// reaches the UI verbatim.
//
// Intentional scope boundary: this bridge deliberately drops NeedsTypedConfirm.
// The embedded host does not render its own approval sheet — it delegates the
// decision to its external caller (Daintree's orchestration UI), which owns the
// approval UX. The typed-confirm friction (issue #210) is enforced on the surfaces
// that DO render the sheet — the cockpit and the classic REPL — not here. The
// risk-class label travels on RiskClass above so the external timeline can still
// display it.
type ConfirmRequest struct {
	ToolName    string
	Summary     string
	RiskClass   domain.RiskClass
	Consequence string
	// RawArgs is the raw JSON args string the model emitted. The bridge redacts it
	// (redactArgs) before emitting the wire event — it never crosses verbatim.
	RawArgs string
}

// AppHooks bundles the hooks the host installs on the App.
type AppHooks struct {
	// AgentEvents is the bridge sink (agent.EventSink). The session emits through it.
	AgentEvents agent.EventSink
	// Confirm is the tool-confirm hook: a mutating tool calls it and blocks until
	// the approval is decided (true) / rejected / times out (false).
	Confirm func(ctx context.Context, req ConfirmRequest) bool
}

// AppFactory builds the App for a booted session. MCP url/token/tier/projectId
// come from env via loadConfig, NOT the descriptor. The cockpit/cli wave provides
// the concrete factory; the host stores it so it can be tested with a fake.
type AppFactory func(ctx context.Context, params AppParams) (App, error)

// AppParams is the descriptor-derived input to AppFactory. appSessionId is
// resumeSessionId ?? sessionId (so resumed conversation state replays).
type AppParams struct {
	SessionID           string // appSessionId: resume id when resuming, else session id
	ProjectPath         string // descriptor.cwd → overrides.projectPath
	ProjectInstructions string // loaded DAINTREE.md content → overrides
}
