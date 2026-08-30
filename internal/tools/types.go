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

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
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

// Connection names an external service a tool depends on. Used to describe the toolset
// truthfully in degraded mode, where the Daintree control plane is unreachable and a
// large fraction of the registry cannot actually do anything.
type Connection string

const (
	// RequiresNothing is purely local: filesystem, SQLite, or in-memory state. Works in
	// degraded mode exactly as it does normally. The zero value, so a family that never
	// thinks about this is correctly described by default.
	RequiresNothing Connection = ""
	// RequiresDaintreeMCP needs the Daintree control plane (DAINTREE_MCP_URL). Without
	// it there are no terminals to read, no agents to spawn, and no worktrees to
	// inspect — the assistant's whole orchestration role is offline.
	RequiresDaintreeMCP Connection = "daintree-mcp"
	// RequiresBackend needs the Daintree Assistant backend beyond the turn itself — a
	// server-owned utility task (summarize, extract, classify, plan, reconcile). Distinct
	// from the control plane: the backend can be reachable while Daintree is not, and
	// vice versa, and the two failures want different fixes.
	RequiresBackend Connection = "assistant-backend"
	// RequiresInteractive needs a human who can be asked. A one-shot, `--json`, or
	// unattended-wake run has no surface to ask on, and the handler fails cleanly rather
	// than blocking forever. The embedded host IS such a surface — it renders a real
	// sheet — so it is not among them.
	RequiresInteractive Connection = "interactive-session"
)

// SetRequires stamps a connection dependency onto every tool in a family's slice. Family
// constructors call it once instead of repeating the field per tool, so a newly added
// tool inherits its family's dependency rather than silently defaulting to "local".
func SetRequires(in []Tool, c Connection) []Tool {
	for i := range in {
		in[i].Requires = c
	}
	return in
}

// SetRequiresPtr is SetRequires for the families that already return []*Tool.
func SetRequiresPtr(in []*Tool, c Connection) []*Tool {
	for _, t := range in {
		t.Requires = c
	}
	return in
}

// NoArgs is the standard empty-object JSON Schema for no-argument tools.
var NoArgs = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{},
	"additionalProperties": false,
}

// ToolProgress is one in-tool progress beat the registry (and long handlers)
// emit so the live footer never looks frozen.
type ToolProgress struct {
	// Phase is one of "validating" | "awaiting_approval" | "awaiting_question" |
	// "running" | "retrying" — the ProgressX constants below.
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

// The wording the registry itself puts on those automatic beats. Named rather than
// written inline at the emit sites so that Lifecycle() below can recognise them
// without a second copy of the strings to drift against.
const (
	ProgressMsgValidating       = "validating request"
	ProgressMsgAwaitingApproval = "waiting for approval"
	ProgressMsgRunning          = "running"
)

var autoProgressMessage = map[string]string{
	ProgressValidating:       ProgressMsgValidating,
	ProgressAwaitingApproval: ProgressMsgAwaitingApproval,
	ProgressRunning:          ProgressMsgRunning,
}

// Lifecycle reports whether this beat is one the registry emits for EVERY call as it
// walks validate → approve → run, in the registry's own wording.
//
// It exists so a consumer that already tracks a call's state can drop them. The
// Daintree host does: it renders queued/running/waiting from tool:state and draws the
// substep underneath, so these beats made every running row restate its own status
// one line down in lowercase ("Waiting on 3 terminals … Running" over "running"), and
// left it there for the life of the call, since the last beat is the one that sticks.
//
// A handler that reuses one of these phases with a message of its own is NOT
// lifecycle — the message is the whole information ("polling terminal 3 of 5"), and
// only the registry's exact automatic wording is matched.
func (p ToolProgress) Lifecycle() bool {
	msg, ok := autoProgressMessage[p.Phase]
	return ok && p.Message == msg
}

// ErrNoAskChoiceHook is returned by a main-actor ToolContext.AskChoice when the
// runtime has no interactive question surface wired (one-shot, a non-TTY). The embedded
// host is NOT among them — it renders a real sheet and answers over question:answer.
// The user.askMultipleChoice handler maps it to a QUESTION_UNAVAILABLE failure. A nil
// ToolContext.AskChoice (a non-interactive watcher/timer/workflow actor) is distinct —
// the handler reports QUESTION_NOT_INTERACTIVE for that case instead.
var ErrNoAskChoiceHook = errors.New("tools: no interactive question surface available")

// ErrQuestionDismissed reports that a question WAS asked and the user closed it
// without choosing. Distinct from ErrNoAskChoiceHook, which means it could not be
// asked at all: the model needs to know the difference, because "nobody could ask you"
// invites a retry through a different route while "you declined to answer" does not.
var ErrQuestionDismissed = errors.New("tools: question dismissed without an answer")

// ChoiceOption is one labelled option in an AskChoiceRequest. The Label (A, B, C…) is
// assigned by the CLI, never by the model, so the model supplies only Text and can't
// collide with or misspell the letters.
type ChoiceOption struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

// MaxChoiceOptions is the option-count ceiling every question surface shares — the size
// of the label alphabet, A–Z. A question with more options than there are letters has no
// way to name them all.
const MaxChoiceOptions = 26

// ChoiceLabel maps a 0-based option index to its display label: 0→A, 1→B, … 25→Z, and ""
// past the end of the alphabet.
//
// One function rather than one per surface, because the label is an IDENTITY: it travels
// on the wire, it is what the transcript records, and it is how the model names what the
// user chose. Two implementations of the same alphabet is two chances for the sheet and
// the answer to disagree about which option "B" was.
func ChoiceLabel(i int) string {
	if i < 0 || i >= MaxChoiceOptions {
		return ""
	}
	return string(rune('A' + i))
}

// AskChoiceRequest is a finite user decision put to the interactive surface, which
// renders a selection sheet and blocks the caller until it is answered or dismissed.
//
// Usually the MODEL asking (user.askMultipleChoice), but not only: a slash command whose
// whole job is "pick one of these" is the same interaction, and is marked Local.
type AskChoiceRequest struct {
	// ToolCallID ties the sheet to its live footer row (informational).
	ToolCallID string `json:"toolCallId,omitempty"`
	// Local marks a question the CLIENT asked on its own account — a slash command —
	// rather than one the model asked mid-turn.
	//
	// It exists because a local question belongs to no turn. Without it the surface
	// stamps whatever turn happens to be running, and the answer is then recorded inside
	// a turn that never asked the question — a transcript that says the model was told
	// something it was not. The model can never set it: the tool builds its own request
	// and never copies this field from arguments.
	Local bool `json:"local,omitempty"`
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
	ToolName string `json:"toolName"`
	// ToolKey is the EFFECTIVE identity the tier and risk gates were applied to, which
	// for a dynamic tool is a composite id rather than the display name.
	//
	// Distinct from ToolName because ToolName is the human-facing label a person is
	// asked to reason about, and two different underlying actions can present the same
	// one. A surface that remembers "don't ask about this again" must key on the
	// identity, or a standing approval given for one action silently covers another.
	ToolKey     string           `json:"toolKey,omitempty"`
	Risk        domain.RiskClass `json:"risk"`
	Summary     string           `json:"summary"`
	Consequence string           `json:"consequence,omitempty"`
	Args        json.RawMessage  `json:"args,omitempty"`
	// NeedsTypedConfirm is the pre-computed safety.NeedsTypedConfirm(Risk) verdict,
	// stamped at construction so every approval surface (attached session, line REPL)
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

// TargetInfo is the EFFECTIVE identity one call resolved to, for a tool whose
// authority depends on its arguments rather than on its registration.
//
// Every registered tool answers "what does this do?" once, at Register time, and
// Tool.Risk is that answer. A target-aware invoker cannot: forwarding
// `terminal.list` and forwarding `worktree.delete` are the same tool and wildly
// different actions. TargetInfo is how such a tool tells dispatch which action
// this particular call actually is, so the tier gate, the confirmation preview,
// grant matching and the audit row all describe the ACTION rather than the
// invoker that carried it.
type TargetInfo struct {
	// Name is the identity dispatch gates and audits on — for MCP dynamic
	// invocation, domain.DynamicTargetName(action). It is deliberately NOT the raw
	// action name: a grant reading `terminal.new` must not silently also authorize
	// a future local tool of that name, and an audit row has to record that the
	// call came through dynamic invocation.
	Name string
	// Display is the raw action shown to the human in the approval preview. The
	// human approves "worktree.delete", not a composite identity.
	Display string
	// Risk is the effective risk class. Dispatch fails the call closed when it is
	// empty, so a resolver that forgets to set it can never widen authority.
	Risk domain.RiskClass
	// Consequence overrides Tool.Consequence in the approval sheet when non-empty,
	// so the preview describes THIS action rather than the generic invoker.
	Consequence string
}

// TargetResolver computes a call's effective identity from its (already decoded)
// arguments. It runs inside Dispatch after Decode and BEFORE the tier gate, so
// everything downstream — Decide, Confirm, ConsumeGrant, audit — sees the target.
//
// Returning a non-nil ToolResult REFUSES the call with that exact failure: the
// resolver owns the refusal prose (typed-wrapper redirect, unknown policy,
// ineligible action), which is tool-family policy dispatch has no business
// re-deriving. The refusal is audited as denied and the handler never runs.
//
// A resolver must be a pure function of (args, live catalog) and must reach the
// same verdict the handler will: the handler re-runs the identical resolution
// over the same immutable args, so the action confirmed is the action forwarded.
type TargetResolver func(ctx context.Context, args json.RawMessage, tctx *ToolContext) (TargetInfo, *ToolResult)

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

	// ResolveTarget, when set, makes this tool's effective risk and audit/grant
	// identity a function of its ARGUMENTS (see TargetResolver). nil — the case for
	// every tool but the MCP dynamic invoker — means the static Name/Risk above are
	// the whole story, so the pipeline is unchanged for them.
	//
	// Risk above stays the FAIL-CLOSED ceiling, not a placeholder: it is what the
	// read-only sub-agent inventory filter, the parallel-dispatch adapters, and the
	// generated capability reference read, none of which can run a resolver. Register
	// a dynamic tool at the worst risk it could ever reach and let the resolver
	// narrow it per call — never the reverse.
	//
	// The parallel half of that rule is ENFORCED, not merely stated: Registry.
	// AssertSafe rejects a resolver-bearing tool that also sets Parallelizable or
	// ParallelHomogeneous at boot, because the concurrency grouping is decided from
	// the static Risk a resolver is free to raise. The sub-agent half stays a matter
	// of registering at the ceiling — a read-risk tool with a resolver is legitimate
	// so long as its resolver only ever narrows.
	ResolveTarget TargetResolver

	// Requires names the connection whose absence makes this tool UNABLE TO DO ITS JOB.
	// The zero value (RequiresNothing) means purely local — filesystem, SQLite, or
	// in-memory — and therefore fully working in degraded mode.
	//
	// It is deliberately the PRIMARY dependency, not a set. A tool that reads a terminal
	// and then summarizes it needs both the control plane and the backend, but without
	// the control plane there is nothing to summarize, so `daintree-mcp` is the answer a
	// reader needs. A tool that merely degrades without a connection — returning less
	// detail, or reporting the outage as its result — declares RequiresNothing: it still
	// works, and `context.snapshot` and `daintree.status` are most useful precisely when
	// Daintree is down.
	//
	// It is DOCUMENTATION, not a gate: dispatch does not consult it, and a tool whose
	// connection is down still runs and returns its own clean "not connected" failure.
	// Gating on it would be actively wrong — it would block the diagnostic tools a
	// disconnected user reaches for first. What it buys is honesty at the surfaces that
	// describe the toolset, so "which of these actually work right now?" has one answer
	// derived from the registry instead of hand-maintained lists that drift apart.
	//
	// Consumed today by the generated capability reference (docs/generated/TOOLS.md) and
	// its drift tests. The degraded-mode banner and `doctor` are the intended next
	// readers; until they are wired, do not describe them as reading it.
	Requires Connection

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
	// serial, because the attached session holds ONE pending approval and grant consumption
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

	// PreflightUnattended, when set, reports why these ARGUMENTS could never work if
	// this tool were dispatched by a non-interactive actor — a timer or a watcher —
	// returning "" when they are fine. It is consulted when such a call is SCHEDULED,
	// not when it runs.
	//
	// It exists because the two moments are hours apart and only the first one has a
	// human in it. A tool that infers something from the turn it is running inside —
	// the active worktree being the case that prompted this — has nothing to infer
	// from at fire time, so a call that reads as complete when the model writes it is
	// already doomed when it is stored. What the user saw was the whole feature
	// failing silently: "Scheduled. Timer tmr_ddd94718 fires in 10 seconds", then ten
	// seconds later a queue row nobody was looking at saying it could not run.
	//
	// The check belongs HERE, on the tool, rather than in a table the scheduler keeps:
	// the fire-time requirement is the tool's own rule, and a copy of it somewhere
	// else is a copy that goes stale the first time the rule changes. Keep this a pure
	// function of the args — it runs at schedule time, where there is no MCP round trip
	// to spend and no live state that will still be true when the timer fires.
	//
	// Advisory for the SCHEDULING surfaces only; dispatch does not consult it, so the
	// handler's own fire-time guard stays the thing that actually refuses to run.
	PreflightUnattended func(args json.RawMessage) string

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
	// FromTimerMessage marks a turn started by a scheduled message (see
	// agent.TurnContext.FromTimerMessage). Tools that must not recurse read this
	// rather than Actor, which is process-wide and cannot distinguish the two.
	FromTimerMessage bool
	// FromWake marks ANY autonomous turn — a scheduled message, a watcher digest, an
	// async completion. Broader than FromTimerMessage on purpose: lineage is not
	// transitive, so a timed message that starts an async wait sheds its own marker at
	// the completion wake, and that turn could schedule again. Every descendant of an
	// autonomous turn is itself autonomous, so this flag is the one that closes the
	// cycle rather than following it.
	FromWake bool
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

	// GatedTarget is the effective identity dispatch resolved and GATED this call,
	// stamped by Dispatch after ResolveTarget and nil for every ordinary tool. A
	// handler behind a TargetResolver reads it to confirm that the action it is about
	// to perform is the action that was tier-checked, previewed to the human, and
	// charged against a grant.
	//
	// It exists because the resolver and the handler run at different times against
	// inputs that are not all immutable: the arguments are, but the live catalog and
	// any future host-supplied policy source are not. Re-deriving the target in the
	// handler keeps the two in step for everything derived from the arguments; this
	// field is what makes a DRIFT in the rest a refusal rather than a silent
	// execution under a policy nobody approved.
	GatedTarget *TargetInfo

	// --- liveness (always present in the attached session; may be zero in tests) ---
	// ToolCallID identifies this call's live footer row.
	ToolCallID string
	// ReportProgress emits an in-tool progress beat. The registry calls it for the
	// standard validating→awaiting_approval→running phases; long handlers call it
	// for meaningful substeps. Nil-safe via reportProgress().
	ReportProgress func(ToolProgress)

	// --- optional (per-turn / per-actor / test-stripped) ---
	SessionID string // runbook step-progress checkpoints
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
