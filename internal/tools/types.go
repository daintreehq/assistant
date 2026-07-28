// Package tools is the tool-dispatch choke point. EVERY tool invocation (agent
// loop, watchers, timers, workflows) flows through ToolRegistry.Dispatch, which
// validates args, applies the safety policy (internal/safety), runs the handler,
// recovers panics, and writes an audit row. The no-file-edit invariant and the
// tier/confirmation matrix are enforced here.
//
// Tool families live in sub-packages (internal/tools/<group>) that import this
// package and register their Tools via NewRegistry/Register. The canonical Tool
// type plus ToolContext, ToolProgress, ConfirmRequest, and the constructors are
// all exported for them.
package tools

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Re-export the ToolResult envelope + constructors from domain so tool families
// can build results without importing two packages. These are aliases, not new
// types — identical to domain.ToolResult etc.
type (
	// ToolResult is the universal envelope every handler returns.
	ToolResult = domain.ToolResult
	// ToolError is the error half of a ToolResult.
	ToolError = domain.ToolError
)

// Ok builds a successful ToolResult (summary required; result may be nil).
func Ok(summary string, result any) ToolResult { return domain.Ok(summary, result) }

// Fail builds a failed ToolResult. Defaults recoverable=true, summary==message.
func Fail(code, message string, opts ...domain.FailOption) ToolResult {
	return domain.Fail(code, message, opts...)
}

// Unrecoverable marks a Fail as non-recoverable.
func Unrecoverable() domain.FailOption { return domain.Unrecoverable() }

// WithDetails attaches a structured details payload to a Fail.
func WithDetails(details any) domain.FailOption { return domain.WithDetails(details) }

// NoArgs is the standard empty-object JSON Schema for no-argument tools.
var NoArgs = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{},
	"additionalProperties": false,
}

// ToolProgress is one in-tool progress beat the registry (and long handlers)
// emit so the live footer never looks frozen.
type ToolProgress struct {
	// Phase is one of "validating" | "awaiting_approval" | "running" | "retrying".
	Phase string `json:"phase"`
	// Message is a short human-facing substep ("launching terminal").
	Message string `json:"message,omitempty"`
	// Completed/Total are an optional progress fraction.
	Completed int `json:"completed,omitempty"`
	Total     int `json:"total,omitempty"`
}

// Standard progress phases the registry emits automatically.
const (
	ProgressValidating       = "validating"
	ProgressAwaitingApproval = "awaiting_approval"
	ProgressAwaitingQuestion = "awaiting_question"
	ProgressRunning          = "running"
	ProgressRetrying         = "retrying"
)

// ErrNoAskChoiceHook is returned by a main-actor ToolContext.AskChoice when the
// runtime has no interactive question surface wired (one-shot, host --stdio, non-TTY).
// The user.askMultipleChoice handler maps it to a QUESTION_UNAVAILABLE failure. A nil
// ToolContext.AskChoice (a non-interactive watcher/timer/workflow actor) is distinct —
// the handler reports QUESTION_NOT_INTERACTIVE for that case instead.
var ErrNoAskChoiceHook = errors.New("tools: no interactive question surface available")

// ChoiceOption is one labelled option in an AskChoiceRequest. The Label (A, B, C…) is
// assigned by the CLI, never by the model, so the model supplies only Text and can't
// collide with or misspell the letters.
type ChoiceOption struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// AskChoiceRequest is handed to ToolContext.AskChoice when the model needs a finite
// user decision (user.askMultipleChoice). The interactive surface renders a selection
// sheet in place of the composer and blocks the tool call until the user answers.
type AskChoiceRequest struct {
	// ToolCallID ties the sheet to its live footer row (informational).
	ToolCallID string `json:"toolCallId,omitempty"`
	// Question is the concise, human-facing prompt (no option labels baked in).
	Question string `json:"question"`
	// Options are the labelled choices, in order (2–26 entries).
	Options []ChoiceOption `json:"options"`
	// Default is the 0-based index of the option highlighted first.
	Default int `json:"default"`
}

// AskChoiceAnswer is the user's selection returned by ToolContext.AskChoice. Index is
// 0-based; Label/Text mirror the chosen ChoiceOption.
type AskChoiceAnswer struct {
	Label string `json:"label"`
	Index int    `json:"index"`
	Text  string `json:"text"`
}

// ConfirmRequest is handed to ToolContext.Confirm for a mutating action. The UI
// approval sheet leads with Consequence (plain-English effect / reversibility /
// secret exposure), falling back to a per-risk phrase when empty.
type ConfirmRequest struct {
	ToolName    string           `json:"toolName"`
	Risk        domain.RiskClass `json:"risk"`
	Summary     string           `json:"summary"`
	Consequence string           `json:"consequence,omitempty"`
	Args        json.RawMessage  `json:"args,omitempty"`
	// NeedsTypedConfirm is the pre-computed safety.NeedsTypedConfirm(Risk) verdict,
	// stamped at construction so every approval surface (cockpit, classic REPL)
	// enforces the typed-phrase requirement for git/system actions without
	// re-deriving the rule. Zero value (false) ⇒ a single-key approval is enough.
	NeedsTypedConfirm bool `json:"needsTypedConfirm,omitempty"`
}

// Handler is a tool implementation. It receives the (already validated/decoded)
// raw args plus the ToolContext, and must NEVER panic to the caller — the
// registry recovers panics into a TOOL_THREW failure, but handlers should still
// return Fail(...) for expected errors. context.Context carries cancellation
// (Escape-to-cancel).
type Handler func(ctx context.Context, args json.RawMessage, tctx *ToolContext) ToolResult

// DecodeFunc optionally validates/coerces raw args before the handler runs. It
// returns the parsed args (which may differ from the input after defaults/
// coercion) or an error whose message becomes the INVALID_ARGS detail. A nil
// DecodeFunc means "pass raw args through unvalidated". Replaces Zod safeParse.
type DecodeFunc func(raw json.RawMessage) (json.RawMessage, error)

// Tool is the canonical typed tool adapter. Internal Name is dotted (fs.read);
// the registry maps it to/from the OpenAI wire name (fs__read).
type Tool struct {
	// Name is the internal dotted name (fs.read, daintree.call).
	Name string
	// Description is model-facing; can be long/instructional.
	Description string
	// Risk drives tier gating + the confirmation matrix (internal/safety).
	Risk domain.RiskClass
	// Consequence is short human Y/N prose for the approval sheet (optional).
	Consequence string
	// Schema is the raw JSON Schema object emitted as OpenAI `parameters`.
	// Stored as json.RawMessage; the registry canonicalizes it at registration.
	Schema json.RawMessage
	// Decode optionally validates/coerces args (replaces Zod). May be nil.
	Decode DecodeFunc
	// Handle is the implementation.
	Handle Handler

	// Parallelizable opts this tool INTO concurrent dispatch: when the model emits a
	// batch containing a consecutive run of parallelizable calls, the turn loop runs
	// them at once instead of one-at-a-time (see agent.runToolBatch). Default false —
	// serial. Set true ONLY on tools that are read-only, free of observable side
	// effects, and have NO ordering dependency on their batch siblings: a fresh,
	// independent snapshot/summary read (e.g. terminal.extract). NEVER set it on a
	// barrier/wait tool (terminal.awaitAll) or anything a later call in the same batch
	// depends on — that would let the dependent call run before the barrier settled.
	// The registry double-gates this on RiskRead (see the ParallelSafe adapter), so a
	// mutating tool can't be parallelized even if this is set by mistake.
	Parallelizable bool

	// ParallelHomogeneous opts a MUTATING tool into concurrent dispatch with
	// consecutive SAME-NAME batch siblings — the spawn-fan-out case: the model emits
	// N independent agentTask.spawnForEdits calls in one batch and serial dispatch
	// costs N×5s where the launches don't depend on each other. This is a SEPARATE,
	// narrower contract than Parallelizable (which means "read-only, groups with any
	// other read"): a homogeneous-mutation cohort groups only calls of this exact
	// tool, only when every member is ALREADY fully authorized (interactive main
	// actor + auto-approve + tier allows — see the ParallelMutationSafe adapter;
	// anything that would need a confirmation prompt or an automation grant stays
	// serial, because the cockpit holds ONE pending approval and grant consumption
	// order must stay deterministic), and only up to the mutation cohort cap.
	// Ordering caveat mirrors Parallelizable: never set it on a tool a later batch
	// sibling depends on.
	ParallelHomogeneous bool

	// ParallelConflictKey, when set on a ParallelHomogeneous tool, classifies a
	// call's INDEPENDENCE within a candidate cohort from its raw args. Return
	// (keys, true) where each key names one conflict dimension the call occupies
	// (a shared target, a collision-prone identity, …) — two calls sharing ANY key
	// conflict and never run concurrently (e.g. two edit-mode spawns into one
	// worktree, or two spawns whose launch identities would collide). Return
	// (nil, true) for a call that is freely independent, or (_, false) for a call
	// that must not join any cohort at all (e.g. an edit-mode spawn into the
	// implicit active worktree, whose target is unknown here). Keys must be
	// computed from the NORMALIZED identity the handler will actually act on
	// (defaults applied, values canonicalized) — raw-spelling keys let two
	// spellings of one target slip into the same cohort. nil func ⇒ every call of
	// the tool is treated as independent.
	ParallelConflictKey func(args json.RawMessage) (keys []string, ok bool)

	// projectionParams is the canonical compact JSON the projection emits as the
	// tool's `parameters`, computed ONCE from Schema at Register (the cold path) so
	// OpenAITools never re-unmarshals the schema on the hot projection path (rebuilt
	// once per model round, of which a turn may run many). Populated by Register; never
	// set by callers.
	projectionParams json.RawMessage
}

// ToolContext is everything a handler can reach. Built once at startup; the
// optional per-turn/per-actor fields are filled by the caller and handlers fail
// gracefully when they are absent. Cross-subsystem deps (Store, MCPClient,
// Queue) are reached through the SMALL consumer-defined interfaces in
// deps.go — NOT the concrete packages — so this package compiles in isolation.
type ToolContext struct {
	// --- required ---
	Config      config.AppConfig // carries Tier, AutoApprove (read in dispatch)
	MCP         MCPClient        // MCP transport (daintree.call)
	DB          Store            // ConsumeGrant + InsertAudit used by the registry
	Queue       Queue            // attention queue; registry publishes denial events
	ProjectPath string           // project root (fs path containment)
	Actor       domain.ToolActor // gates the confirmation branch
	// Confirm approves a mutating action. A returned error is treated as a
	// DECLINE (never an approval).
	Confirm func(ctx context.Context, req ConfirmRequest) (bool, error)
	// AskChoice presents a multiple-choice question to the interactive user and BLOCKS
	// until they answer, then returns the chosen option (user.askMultipleChoice). It is
	// set ONLY for the interactive main actor; nil for non-interactive actors
	// (watcher/timer/workflow), so a handler detects "cannot prompt" by a nil check. A
	// returned error means the user cancelled or the runtime can't ask — context.Canceled
	// (or ctx expiry) is a cancellation, ErrNoAskChoiceHook is a non-interactive runtime.
	AskChoice func(ctx context.Context, req AskChoiceRequest) (AskChoiceAnswer, error)
	// Log emits an out-of-band line to the user.
	Log func(msg string)

	// --- liveness (always present in the cockpit; may be zero in tests) ---
	// ToolCallID identifies this call's live footer row.
	ToolCallID string
	// ReportProgress emits an in-tool progress beat. The registry calls it for the
	// standard validating→awaiting_approval→running phases; long handlers call it
	// for meaningful substeps. Nil-safe via reportProgress().
	ReportProgress func(ToolProgress)

	// --- optional (per-turn / per-actor / test-stripped) ---
	SessionID string // skill step-progress checkpoints
	// ActorID is the wch_…/tmr_… of the non-interactive actor — REQUIRED for the
	// grant lookup in dispatch Branch A.
	ActorID string
	RunID   string // one AgentSession.send() turn; stamped on each audit row
	// ActiveToolNames are the tools offered this turn; nil ⇒ all callable.
	ActiveToolNames []string
	// DaemonActive reports whether the scheduler is running; nil ⇒ assume active.
	DaemonActive func() bool
	// InjectionsPending reports whether the human has typed a message that is
	// buffered for the running turn but not yet folded in. A long-running in-turn
	// wait (terminal.awaitAll) polls this to break early and hand control back so
	// the message is acted on at the next iteration boundary — instead of trapping
	// the user behind a multi-minute block. nil ⇒ no interactive injector (tests,
	// non-interactive actors) ⇒ never interrupted.
	InjectionsPending func() bool
}

// reportProgress safely emits a progress beat (no-op when the callback is unset).
// A panicking callback must never abort dispatch — progress is a cosmetic
// side-channel, so swallow any panic (the top-level Dispatch firewall would also
// catch it, but containing it here keeps the call point's flow intact).
func (c *ToolContext) reportProgress(p ToolProgress) {
	if c == nil || c.ReportProgress == nil {
		return
	}
	defer func() { _ = recover() }()
	c.ReportProgress(p)
}
