package host

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"github.com/daintreehq/assistant/internal/tools"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/redact"
)

// Approval/redaction constants.
const (
	// DefaultApprovalTimeoutMs is the unanswered-confirm auto-timeout (5 min).
	DefaultApprovalTimeoutMs = 5 * 60_000 // 300000
	// argsSummaryMaxString collapses any string longer than this in redactArgs.
	argsSummaryMaxString = 80
)

// PostFunc is the transport sink the bridge writes events through. Injected so
// the bridge has no transport dependency (tests + the NDJSON owner share it).
type PostFunc func(HostEvent)

// RiskOfFunc looks up a tool's risk class for the danger hint. The bool is false
// when the tool is unknown.
type RiskOfFunc func(toolName string) (domain.RiskClass, bool)

// BridgeOptions configures a Bridge.
type BridgeOptions struct {
	SessionID string
	Post      PostFunc
	// PostStream is the backpressure lane for high-volume events. Defaults to Post
	// when nil (tests, and any consumer that does not care to separate the two).
	PostStream        PostFunc
	RiskOf            RiskOfFunc   // default: always unknown
	Now               func() int64 // default: domain.NowMS
	ApprovalTimeoutMs int          // default: DefaultApprovalTimeoutMs (0 disables the timer)
}

// pendingApproval is one outstanding confirm: a channel the confirm() caller
// blocks on plus the auto-timeout timer.
// ErrQuestionDismissed reports that the user closed the question without choosing.
// Aliased to the tools sentinel so the question handler can tell "asked and declined"
// from "could not ask" — they lead the model to different next moves.
var ErrQuestionDismissed = tools.ErrQuestionDismissed

// pendingQuestion parks a user.askMultipleChoice dispatch until the host answers.
type pendingQuestion struct {
	resolve chan questionOutcome
	options []QuestionOption
}

// questionOutcome is the settled selection; Index -1 means dismissed.
type questionOutcome struct {
	Index int
}

type pendingApproval struct {
	resolve chan ConfirmationDecision
	timer   *time.Timer
}

// Bridge adapts the in-process agent EventSink + tool-confirm hook into wire
// HostEvents. It owns the single-turn lifecycle, approvals, redaction, and audit
// mapping. The agent loop runs Send() on another goroutine and calls the sink
// methods concurrently, so all mutable state is guarded by mu. No transport
// dependency — events go through the injected Post.
type Bridge struct {
	sessionID         string
	post              PostFunc
	postStream        PostFunc
	riskOf            RiskOfFunc
	now               func() int64
	approvalTimeoutMs int

	mu               sync.Mutex
	activeTurnID     string
	interrupted      bool // latched until next startExchange
	pendingApprovals map[string]*pendingApproval
	pendingQuestions map[string]*pendingQuestion
	// liveTools tracks announced-but-unsettled calls by id → last known state, so an
	// interrupt can terminalize them. Without it a cancelled turn left every in-flight
	// row rendering as "Running" forever: the host had been told the call started and
	// was never told anything else.
	liveTools     map[string]string
	toolStartedAt map[string]int64
	// wakeTurn marks the current exchange as one the assistant started ITSELF, so the
	// assistant turn can carry it and a host can tell that Stop will not reach it.
	wakeTurn bool
}

// NewBridge builds a Bridge with defaults filled.
func NewBridge(opts BridgeOptions) *Bridge {
	if opts.RiskOf == nil {
		opts.RiskOf = func(string) (domain.RiskClass, bool) { return "", false }
	}
	if opts.Now == nil {
		opts.Now = domain.NowMS
	}
	if opts.ApprovalTimeoutMs == 0 {
		opts.ApprovalTimeoutMs = DefaultApprovalTimeoutMs
	}
	if opts.PostStream == nil {
		opts.PostStream = opts.Post
	}
	return &Bridge{
		sessionID:         opts.SessionID,
		post:              opts.Post,
		postStream:        opts.PostStream,
		riskOf:            opts.RiskOf,
		now:               opts.Now,
		approvalTimeoutMs: opts.ApprovalTimeoutMs,
		pendingApprovals:  make(map[string]*pendingApproval),
		pendingQuestions:  make(map[string]*pendingQuestion),
		liveTools:         make(map[string]string),
		toolStartedAt:     make(map[string]int64),
	}
}

// genID returns "<prefix>_<8 hex>" (e.g. turn_1a2b3c4d). domain.NewID expects a
// trailing-underscore prefix.
func genID(prefix string) string { return domain.NewID(prefix + "_") }

// ---------------------------------------------------------------------------
// EventSink — the agent.EventSink handed to the App via setHooks.
// ONE assistant turn spans a whole Session.Send() across model iterations + tool
// calls: AssistantStart fires once per round but only the FIRST opens the turn.
// ---------------------------------------------------------------------------

// Phase forwards the explicit run lifecycle. v2 dropped it as "live-only UI
// vocabulary"; under v3 the host IS the UI, and liveness inferred from "has any
// token arrived" is exactly the heuristic domain.RunPhase exists to replace.
func (b *Bridge) Phase(p domain.RunPhase) {
	// Checked and enqueued under one hold, like every other lifecycle event: a phase
	// that slipped through after an interrupt put the status line back to "Working"
	// for a turn the user had just stopped.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.interrupted {
		return
	}
	b.postStream(EvTurnPhase{TurnID: b.activeTurnID, Phase: p.String(), Wake: b.wakeTurn})
}

func (b *Bridge) AssistantStart() {
	b.mu.Lock()
	if b.interrupted || b.activeTurnID != "" {
		b.mu.Unlock()
		return
	}
	turnID := genID("turn")
	b.activeTurnID = turnID
	now := b.now()
	wake := b.wakeTurn
	b.mu.Unlock()
	b.post(EvTurnStart{TurnID: turnID, Role: RoleAssistant, StartedAt: now, Wake: wake})
}

func (b *Bridge) AssistantToken(chunk string) {
	b.mu.Lock()
	if b.interrupted || b.activeTurnID == "" {
		b.mu.Unlock()
		return
	}
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.postStream(EvTurnToken{TurnID: turnID, Chunk: chunk})
}

// AssistantEnd closes the turn: "answered" if content is non-blank else "unknown".
//
// The content is carried on turn:end as the AUTHORITATIVE text (see EvTurnEnd) so a
// consumer can replace whatever it accumulated from turn:token. Reasoning, when the
// round produced any, goes out first as its own event — ahead of turn:end, so a host
// that renders it can attach it to a turn that is still open.
func (b *Bridge) AssistantEnd(content, reasoning string) {
	if trimNonEmpty(reasoning) {
		b.mu.Lock()
		turnID := b.activeTurnID
		interrupted := b.interrupted
		b.mu.Unlock()
		if !interrupted && turnID != "" {
			b.post(EvTurnReasoning{TurnID: turnID, Text: reasoning})
		}
	}
	outcome := OutcomeUnknown
	if trimNonEmpty(content) {
		outcome = OutcomeAnswered
	}
	b.closeTurnWithContent(outcome, content, true)
}

// AssistantCancelled closes the turn as cancelled. The streamed buffer is dropped by
// contract, so no authoritative content is claimed — a host keeps what it rendered
// and marks the turn cancelled rather than blanking it.
func (b *Bridge) AssistantCancelled(string) { b.closeTurn(OutcomeCancelled) }

// Interjection reports a mid-turn steer at the moment the loop FOLDS IT IN. The host
// sent the text, so v2 called echoing it redundant — but only the runtime knows when
// it actually landed, and a transcript that shows the steer in the wrong place
// misrepresents what the model saw when it answered.
func (b *Bridge) Interjection(text string) {
	b.mu.Lock()
	turnID := b.activeTurnID
	interrupted := b.interrupted
	b.mu.Unlock()
	if interrupted {
		return
	}
	b.post(EvTurnInterjection{TurnID: turnID, Text: text})
}

// SkillLoaded stays unforwarded. It is a per-ATTEMPT cue that fires on a delta, so a
// retried round can report a load the committed round did not repeat — reconstructing
// the active set from it is wrong by construction. SkillDecision is the authority.
func (b *Bridge) SkillLoaded([]string) {}

// SkillDecision is diagnostic, not conversational: backend skill selection is prompt
// assembly the user neither approves nor steers, and the runtime contract is that no
// sink folds it into the transcript. It reaches a human only through an explicit
// `/explain <run>` replay, which reads the durable run log — so the bridge still
// drops it rather than putting prompt-assembly machinery on the conversation wire.
func (b *Bridge) SkillDecision(agent.SkillDecisionEvent) {}

// postLive enqueues a tool-lifecycle event only if the turn has not been interrupted,
// with the check and the enqueue under ONE lock hold.
//
// Splitting them is what let a stopped turn come back to life: Interrupt would
// terminalize a call as cancelled, and an in-flight ToolState/ToolBatch/ToolCall —
// already past its own check — would enqueue "queued" or "active" behind it, leaving a
// row running on a turn the user had stopped. `post` is the transport's enqueue and
// never re-enters the bridge, so holding the lock across it cannot deadlock.
func (b *Bridge) postLive(ev HostEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.interrupted {
		return
	}
	b.post(ev)
}

// ToolBatch announces the WHOLE batch as queued before dispatch begins. Without it a
// host can only reveal calls one at a time as each starts, which reads as the
// assistant improvising rather than working through a plan it already made.
func (b *Bridge) ToolBatch(calls []agent.BatchedToolCall) {
	if len(calls) == 0 {
		return
	}
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	turnID := b.activeTurnID
	for _, c := range calls {
		b.liveTools[c.ID] = toolStateQueued
	}
	b.mu.Unlock()
	out := make([]BatchedCall, 0, len(calls))
	for _, c := range calls {
		verb, _ := presentToolVerb(c.Name)
		out = append(out, BatchedCall{
			ToolCallID:  c.ID,
			ToolID:      c.Name,
			ArgsSummary: redactArgs(c.Args),
			Danger:      b.isDanger(c.Name),
			Verb:        verb,
			ActiveVerb:  presentToolActiveVerb(c.Name),
			// Redacted like every other free-text field: the target is lifted straight
			// out of the raw arguments, which is exactly the material redactArgs exists
			// to guard. A command line or a saved memory body is a plausible place for a
			// credential to appear.
			Target: redact.String(presentToolTarget(c.Name, c.Args)),
		})
	}
	b.postLive(EvToolBatch{TurnID: turnID, Calls: out})
}

// ToolState promotes one announced call. "waiting" is the load-bearing one: it means
// blocked on the USER, not on the tool, and a host that renders it as ordinary
// progress leaves someone watching a spinner for their own unanswered approval.
func (b *Bridge) ToolState(id string, state agent.ToolState) {
	// The interrupted check and the liveTools write happen under ONE lock hold.
	// Splitting them let Interrupt run in the gap: it would terminalize this call as
	// cancelled, and then this function — already past its check — would re-post
	// "active" over the top and re-add the id, leaving a row spinning forever on a
	// turn the user had stopped.
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	turnID := b.activeTurnID
	// A TERMINAL state leaves the live set rather than joining it. The engine emits
	// tool:state(done) after every successful result — after ToolResult has already
	// forgotten the call — so recording it here put a finished call back among the
	// live ones, and a later interrupt would rewrite it as "not-run": a call that
	// demonstrably ran, reported as never started.
	if state == "done" || state == "failed" {
		delete(b.liveTools, id)
	} else {
		b.liveTools[id] = string(state)
	}
	b.mu.Unlock()
	b.postLive(EvToolState{ToolCallID: id, State: string(state), TurnID: turnID})
}

// ToolProgress carries an in-tool substep so a long call does not look frozen.
func (b *Bridge) ToolProgress(id string, msg string) {
	b.mu.Lock()
	turnID := b.activeTurnID
	interrupted := b.interrupted
	b.mu.Unlock()
	if interrupted {
		return
	}
	b.postStream(EvToolProgress{ToolCallID: id, Message: msg, TurnID: turnID})
}

func (b *Bridge) ToolCall(ev agent.ToolCallEvent) {
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	b.toolStartedAt[ev.ID] = ev.StartedAt
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.postLive(EvToolStarted{
		ToolCallID:  ev.ID,
		ToolID:      ev.Name,
		ArgsSummary: redactArgs(ev.Args),
		StartedAt:   ev.StartedAt,
		TurnID:      turnID,
		Danger:      b.isDanger(ev.Name),
	})
}

func (b *Bridge) ToolResult(ev agent.ToolResultEvent) {
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	startedAt, hadStart := b.toolStartedAt[ev.ID]
	delete(b.toolStartedAt, ev.ID)
	// liveTools is NOT cleared here. Removing it before the settle is enqueued opens a
	// window where an interrupt neither terminalizes this call (it is already gone
	// from the snapshot) nor lets the settle through (the recheck suppresses it) —
	// leaving the row active forever. It is cleared below, atomically with the post.
	turnID := b.activeTurnID
	b.mu.Unlock()

	var durationMs int64
	if hadStart {
		d := ev.EndedAt - startedAt
		if d > 0 {
			durationMs = d
		}
	}
	result, severity, errorCode := resultToAudit(ev.Result)
	errorMessage := ""
	if ev.Result.Error != nil {
		errorMessage = redact.String(ev.Result.Error.Message)
	}
	settled := EvToolSettled{
		// Redacted on the way out, like every other model/tool-authored string that
		// crosses this boundary: a summary or an error can quote an argument, and an
		// argument can be a token.
		Summary:      redact.String(ev.Result.Summary),
		ErrorMessage: errorMessage,
		ToolCallID:   ev.ID,
		ToolID:       ev.Name,
		DurationMs:   durationMs,
		Result:       result,
		Severity:     severity,
		ErrorCode:    errorCode,
		TurnID:       turnID,
	}
	// An accepted async handle: surface it so the host can render "accepted,
	// still running in the background" instead of a finished success (parity
	// with the host's yellow async-pending state).
	if ev.Result.Ok && ev.Result.Async != nil {
		settled.AsyncID = ev.Result.Async.ID
		settled.AsyncTitle = redact.String(ev.Result.Async.Title)
	}
	// Validate, forget and enqueue under ONE hold. An interrupt either wins — and
	// terminalizes this call, because it is still in liveTools — or loses, and the
	// genuine settle goes out. There is no ordering in which the row is left running.
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.interrupted {
		return
	}
	delete(b.liveTools, ev.ID)
	b.post(settled)
}

func (b *Bridge) Error(message string) {
	b.post(EvError{Code: "turn-error", Message: message})
	b.closeTurn(OutcomeUnknown)
}

// Warn forwards a non-fatal warning (a tool loop repeating the same failure, a
// pinned skill the backend could not honour). v2 dropped these, which meant that once
// the local renderer was gone they reached nobody at all.
func (b *Bridge) Warn(message string) { b.postNotice("warning", message) }

// Info forwards an informational notice.
func (b *Bridge) Info(message string) { b.postNotice("info", message) }

// postNotice is the shared Warn/Info path.
func (b *Bridge) postNotice(level, message string) {
	if !trimNonEmpty(message) {
		return
	}
	b.mu.Lock()
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvNotice{Level: level, Message: message, TurnID: turnID})
}

// Usage forwards per-round token accounting. ContextTokens against ContextWindow is
// what drives a context meter, and ContextThreshold is where auto-compaction fires —
// none of which a host can compute for itself.
func (b *Bridge) Usage(ev agent.UsageEvent) {
	b.mu.Lock()
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvUsage{
		TurnID:           turnID,
		PromptTokens:     ev.PromptTokens,
		CompletionTokens: ev.CompletionTokens,
		TotalTokens:      ev.TotalTokens,
		CachedTokens:     ev.CachedTokens,
		CacheHitRatio:    ev.CacheHitRatio,
		ContextTokens:    ev.ContextTokens,
		ContextThreshold: ev.ContextThreshold,
		ContextWindow:    ev.ContextWindow,
	})
}

// TurnPrompt has no host-protocol channel — Daintree originated the prompt, so the
// bridge drops it (it's persisted for /explain by the run-event sink).
func (b *Bridge) TurnPrompt(string) {}

// ModelRateLimited signals the provider throttled us after the retry budget was
// exhausted. A health cue that clears on the next usage event, not a turn failure.
func (b *Bridge) ModelRateLimited() {
	b.mu.Lock()
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvModelRateLimited{TurnID: turnID})
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// StartExchange resets per-turn state and emits a zero-duration user turn
// (start+end at the same ts). Prompt text is NOT carried — Daintree originated it.
// StartWakeExchange is StartExchange for a turn the assistant started on its own.
// The wake flag rides the assistant turn, since that is the one a host would offer a
// Stop control for.
func (b *Bridge) StartWakeExchange() { b.startExchange(true) }

func (b *Bridge) StartExchange() { b.startExchange(false) }

func (b *Bridge) startExchange(wake bool) {
	b.mu.Lock()
	b.interrupted = false
	b.activeTurnID = ""
	b.mu.Unlock()
	turnID := genID("turn")
	now := b.now()
	if !wake {
		// A wake has no user turn to open: nobody said anything.
		b.post(EvTurnStart{TurnID: turnID, Role: RoleUser, StartedAt: now})
		b.post(EvTurnEnd{TurnID: turnID, EndedAt: now})
	}
	b.mu.Lock()
	b.wakeTurn = wake
	b.mu.Unlock()
}

// SettleTurn closes any dangling assistant turn (no-op if already closed). Called
// in the prompt/wake finally.
func (b *Bridge) SettleTurn(outcome TurnOutcomeClass) {
	if outcome == "" {
		outcome = OutcomeUnknown
	}
	b.closeTurn(outcome)
}

// rememberable reports whether a risk class may be added to a session "don't ask
// again" list. The highest classes are always re-confirmed. Ported verbatim from the
// cockpit, which owned this rule when it owned the approval sheet.
func rememberable(r domain.RiskClass) bool {
	// An ALLOW-list, not a deny-list. A deny-list fails open: a risk class added later
	// — or one this build does not recognise — would default to rememberable, quietly
	// making a new class of action eligible for "don't ask again" that nobody decided
	// should be. The classes named here are the ones judged safe to remember.
	switch r {
	case domain.RiskRead,
		domain.RiskLocal,
		domain.RiskUI,
		domain.RiskTerminal,
		domain.RiskProject,
		domain.RiskExternal:
		return true
	}
	return false
}

// Tool lifecycle states, as they appear on the wire.
const (
	toolStateQueued  = "queued"
	toolStateActive  = "active"
	toolStateWaiting = "waiting"
	// toolStateCancelled is a call that WAS running when the user stopped the turn.
	toolStateCancelled = "cancelled"
	// toolStateNotRun is a call that was announced but never started.
	toolStateNotRun = "not-run"
)

// PostCost emits the session's cumulative spend. Called after a turn settles, which is
// when the figure has actually moved and when a reader is most likely to look at it.
//
// EvCost existed on the wire from the start with no producer, so every embedded
// session reported unknown cost forever — the one readout a user cannot reconstruct
// from anything else on screen.
func (b *Bridge) PostCost(total float64, complete bool) {
	b.mu.Lock()
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvCost{TurnID: turnID, Total: total, Complete: complete})
}

// Interrupt is the display side of an interrupt: latch interrupted, terminalize every
// outstanding call, and close the turn as CANCELLED.
//
// Two things changed here and both were wrong before. The outcome was "agent-stuck",
// which describes an agent that hung — but the user pressed Stop, and recording their
// deliberate interruption as a fault misreports it in the transcript and in every
// tally built from outcomes. And nothing terminalized the calls: the host had been
// told each one started and was never told anything else, so a stopped turn left rows
// rendering as "Running" permanently, describing work that is not happening.
//
// A call that was running becomes "cancelled"; one that never started becomes
// "not-run". The distinction matters to anyone reading back what a stop actually
// interrupted.
func (b *Bridge) Interrupt() {
	b.mu.Lock()
	if b.activeTurnID == "" {
		b.mu.Unlock()
		return
	}
	b.interrupted = true
	turnID := b.activeTurnID
	// Snapshot and clear under the lock; the posts happen outside it.
	terminal := make(map[string]string, len(b.liveTools))
	for id, state := range b.liveTools {
		switch state {
		case toolStateActive, toolStateWaiting:
			terminal[id] = toolStateCancelled
		default:
			terminal[id] = toolStateNotRun
		}
	}
	b.liveTools = make(map[string]string)
	// Posted under the SAME hold that latched `interrupted` and took the snapshot.
	// Releasing first would let a ToolState already past its own check enqueue behind
	// these terminal states and revive the row. Ordered before turn:end so a consumer
	// applying events in order never sees the turn close with calls still live.
	for id, state := range terminal {
		b.post(EvToolState{ToolCallID: id, State: state, TurnID: turnID})
	}
	b.mu.Unlock()

	b.closeTurn(OutcomeCancelled)
}

// closeTurn nulls the active turn and emits turn:end. Idempotent (guarded by the
// active turn id) so AssistantEnd + AssistantCancelled can both fire — the first
// closes, the second no-ops.
func (b *Bridge) closeTurn(outcome TurnOutcomeClass) {
	b.closeTurnWithContent(outcome, "", false)
}

// closeTurnWithContent closes the active turn, optionally carrying the authoritative
// final text. hasContent distinguishes "the turn said nothing" from "the turn said
// the empty string", which a host needs in order to tell a tool-only round from an
// empty answer.
func (b *Bridge) closeTurnWithContent(outcome TurnOutcomeClass, content string, hasContent bool) {
	b.mu.Lock()
	turnID := b.activeTurnID
	if turnID == "" {
		b.mu.Unlock()
		return
	}
	b.activeTurnID = ""
	now := b.now()
	b.mu.Unlock()
	b.post(EvTurnEnd{
		TurnID:     turnID,
		EndedAt:    now,
		Outcome:    outcome,
		Content:    content,
		HasContent: hasContent,
	})
}

// Confirm is the tool-confirm hook: mint an approval, emit approval:requested,
// and block until decided / timed out / drained. Returns true iff approved. Runs
// on the agent's dispatch goroutine; the command loop resolves via decide/drain.
//
// The emitted approval:requested carries the request's display context — risk
// class (passed through verbatim, so a per-confirm override survives), the
// human-readable consequence, and a redacted args summary (redactArgs, the same
// helper tool:started uses) — so Daintree's timeline matches a local host
// approval. Empty fields are omitted by the event encoder.
func (b *Bridge) Confirm(ctx context.Context, req ConfirmRequest) bool {
	approvalID := genID("apr")
	resolve := make(chan ConfirmationDecision, 1)

	b.mu.Lock()
	turnID := b.activeTurnID
	now := b.now()
	pa := &pendingApproval{resolve: resolve}
	if b.approvalTimeoutMs > 0 {
		// Auto-timeout an unanswered confirm. Go timers don't keep the process
		// alive, so no unref() analog is needed.
		pa.timer = time.AfterFunc(time.Duration(b.approvalTimeoutMs)*time.Millisecond, func() {
			b.ResolveApproval(approvalID, DecisionTimeout)
		})
	}
	b.pendingApprovals[approvalID] = pa
	// Registered AND announced under one hold. Releasing first let a decision — which
	// can arrive the instant the request is visible — enqueue `approval:decided` ahead
	// of the `approval:requested` it answers, leaving a card on screen that nothing
	// will ever close. Unlocked explicitly right after the post, NOT deferred: the
	// select below blocks until the decision arrives, and ResolveApproval needs this
	// same lock to deliver it.
	b.post(EvApprovalRequested{
		ApprovalID:        approvalID,
		ToolID:            req.ToolName,
		Summary:           req.Summary,
		RequestedAt:       now,
		TurnID:            turnID,
		RiskClass:         req.RiskClass,
		Consequence:       req.Consequence,
		ArgsSummary:       redactArgs(req.RawArgs),
		NeedsTypedConfirm: req.NeedsTypedConfirm,
		Rememberable:      rememberable(req.RiskClass),
		ToolKey:           req.ToolKey,
	})
	b.mu.Unlock()

	// Block on the decision. ctx cancellation (turn abort) also frees the dispatch:
	// resolve as rejected so a parked dispatch returns USER_DECLINED.
	select {
	case d := <-resolve:
		return d == DecisionApproved
	case <-ctx.Done():
		b.ResolveApproval(approvalID, DecisionRejected)
		return false
	}
}

// AskChoice is the question hook: mint a question, emit question:requested, and BLOCK
// the tool dispatch until the host answers.
//
// Symmetrical with Confirm above, and for the same reason: the model is asking a human
// something, and only the surface attached to that human can answer. Without this hook
// the host ran with AskChoice nil, which the tool reports as QUESTION_UNAVAILABLE — so
// `user.askMultipleChoice` was advertised to the model and then failed every time it
// was called, which is worse than not offering it at all.
//
// Cancellation resolves as a DISMISSAL rather than a selection. Picking an option on
// the user's behalf because their turn was interrupted is the one outcome a question
// surface must never produce.
func (b *Bridge) AskChoice(ctx context.Context, req AskChoiceRequest) (AskChoiceAnswer, error) {
	questionID := genID("qst")
	resolve := make(chan questionOutcome, 1)

	opts := make([]QuestionOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, QuestionOption{Label: o.Label, Text: o.Text})
	}

	b.mu.Lock()
	turnID := b.activeTurnID
	now := b.now()
	b.pendingQuestions[questionID] = &pendingQuestion{resolve: resolve, options: opts}
	b.mu.Unlock()

	b.post(EvQuestionRequested{
		QuestionID:  questionID,
		ToolCallID:  req.ToolCallID,
		TurnID:      turnID,
		Question:    req.Question,
		Options:     opts,
		Default:     req.Default,
		RequestedAt: now,
	})

	select {
	case out := <-resolve:
		if out.Index < 0 || out.Index >= len(opts) {
			return AskChoiceAnswer{}, ErrQuestionDismissed
		}
		chosen := opts[out.Index]
		return AskChoiceAnswer{Label: chosen.Label, Index: out.Index, Text: chosen.Text}, nil
	case <-ctx.Done():
		b.ResolveQuestion(questionID, -1)
		return AskChoiceAnswer{}, ErrQuestionDismissed
	}
}

// ResolveQuestion settles an outstanding question (answer / dismiss / drain). No-op if
// not pending. Emits question:answered and unblocks the AskChoice caller.
func (b *Bridge) ResolveQuestion(questionID string, index int) {
	b.mu.Lock()
	pq, ok := b.pendingQuestions[questionID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.pendingQuestions, questionID)
	turnID := b.activeTurnID
	b.mu.Unlock()

	label, text := "", ""
	if index >= 0 && index < len(pq.options) {
		label, text = pq.options[index].Label, pq.options[index].Text
	} else {
		index = -1
	}
	b.post(EvQuestionAnswered{
		QuestionID: questionID,
		TurnID:     turnID,
		Index:      index,
		Label:      label,
		Text:       text,
	})
	select {
	case pq.resolve <- questionOutcome{Index: index}:
	default:
	}
}

// ResolveApproval settles an outstanding approval (decide / timeout / drain).
// No-op if not pending. Emits approval:decided and unblocks the Confirm caller.
func (b *Bridge) ResolveApproval(approvalID string, decision ConfirmationDecision) {
	b.mu.Lock()
	pa, ok := b.pendingApprovals[approvalID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.pendingApprovals, approvalID)
	if pa.timer != nil {
		pa.timer.Stop()
	}
	now := b.now()
	b.mu.Unlock()

	b.post(EvApprovalDecided{ApprovalID: approvalID, Decision: decision, DecidedAt: now})
	// Non-blocking: the channel is buffered(1) and only ever receives once.
	select {
	case pa.resolve <- decision:
	default:
	}
}

// SettlePendingApprovals rejects (default) every outstanding approval. Used on
// interrupt + teardown drain so a parked dispatch never strands busy.
func (b *Bridge) SettlePendingApprovals(decision ConfirmationDecision) {
	if decision == "" {
		decision = DecisionRejected
	}
	b.mu.Lock()
	ids := make([]string, 0, len(b.pendingApprovals))
	for id := range b.pendingApprovals {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.ResolveApproval(id, decision)
	}
}

// isDanger: any non-read risk class = danger. Unknown tool → not danger.
func (b *Bridge) isDanger(toolName string) bool {
	risk, ok := b.riskOf(toolName)
	return ok && risk != domain.RiskRead
}

// ---------------------------------------------------------------------------
// Audit mapping + redaction
// ---------------------------------------------------------------------------

// errorCodeToResult maps a tool error code → AuditResult.
var errorCodeToResult = map[string]AuditResult{
	"CONFIRMATION_REQUIRED": AuditConfirmationPending,
	"UNAUTHORIZED":          AuditUnauthorized,
	"TIER_REJECTED":         AuditUnauthorized,
	"FORBIDDEN":             AuditUnauthorized,
	"RATE_LIMITED":          AuditRateLimited,
	"DEDUP":                 AuditDedup,
	"DUPLICATE":             AuditDedup,
	"COLLISION":             AuditCollision,
}

// resultToAudit maps a ToolResult to {result, severity, errorCode}. Success → no
// errorCode. Otherwise the error code drives the result class (unknown → error).
func resultToAudit(res domain.ToolResult) (AuditResult, AuditSeverity, string) {
	if res.Ok {
		return AuditSuccess, SeverityInfo, ""
	}
	code := ""
	if res.Error != nil {
		code = res.Error.Code
	}
	result, ok := errorCodeToResult[code]
	if !ok {
		result = AuditError
	}
	return result, SeverityForResult(result), code
}

// trimNonEmpty reports whether s has any non-whitespace content (TS content.trim()).
func trimNonEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' && r != '\v' && r != '\f' {
			return true
		}
	}
	return false
}

// strLen returns the JS String.length (UTF-16 code-unit count) of s. The TS
// redactArgs collapses on `.length > 80`, which is UTF-16 units; we match it
// exactly so the "<string: N chars>" count and the 80-char cap agree with
// Daintree's MCP-audit redaction for non-BMP input. (Rune count would diverge on
// astral-plane characters; UTF-16 parity is the deliberate choice.)
func strLen(s string) int { return len(utf16.Encode([]rune(s))) }

// redactArgs builds a single-level, redacted JSON view of tool args for the
// timeline. Raw values may carry file/terminal/prompt content and must never
// cross verbatim. args is the raw JSON arguments string the model emitted.
//
// Quirk preserved: a top-level JSON array is iterated by key (Object.entries),
// so its indices become string keys and it serializes as an object {"0":…}, not
// an array.
func redactArgs(rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	// CREDENTIAL MASKING FIRST, structural summarization second.
	//
	// The structural pass below only collapses values by SHAPE and LENGTH — a short
	// string passes through verbatim, so `{"password":"hunter2hunter2"}` used to cross
	// the wire unchanged. Ordinary tool:started events were safe because the agent
	// EventSink already sanitizes at the source, but a CONFIRM request does not come
	// through that path: it is built straight from the dispatch arguments.
	//
	// So the masking has to happen at this boundary, not upstream of it. redact.String
	// removes registered exact secrets first, then credential shapes (sensitive JSON
	// keys, env assignments, PEM blocks, URL userinfo). Running it on the raw JSON text
	// keeps key-aware rules working, since it can still see `"password":` next to its
	// value — a value-by-value pass after the structure was flattened could not.
	rawArgs = redact.String(rawArgs)
	var v any
	if err := json.Unmarshal([]byte(rawArgs), &v); err != nil {
		// Not valid JSON: treat the raw string itself as the top-level string value.
		return redactTopLevelString(rawArgs)
	}
	switch val := v.(type) {
	case nil:
		return "" // null → empty string
	case string:
		return redactTopLevelString(val)
	case map[string]any:
		return redactObject(val)
	case []any:
		// Object.entries on an array → string-keyed object {"0":…}.
		obj := make(map[string]any, len(val))
		for i, item := range val {
			obj[fmt.Sprintf("%d", i)] = redactValue(item)
		}
		return marshalRedacted(obj)
	default:
		// number / bool → JSON.stringify(v).
		b, err := json.Marshal(val)
		if err != nil {
			return "<unserializable>"
		}
		return string(b)
	}
}

// redactTopLevelString: >80 → "<string: N chars>" else the quoted string. Uses
// no-HTML-escape marshaling so a short string containing <, >, & is emitted
// verbatim rather than escaped.
func redactTopLevelString(s string) string {
	if strLen(s) > argsSummaryMaxString {
		return fmt.Sprintf("<string: %d chars>", strLen(s))
	}
	if q, ok := jsonNoEscape(s); ok {
		return q
	}
	return "<unserializable>"
}

func redactObject(m map[string]any) string {
	out := make(map[string]any, len(m))
	for k, item := range m {
		out[k] = redactValue(item)
	}
	return marshalRedacted(out)
}

// redactValue collapses a per-key value: long string → marker, short string
// as-is, array → "<array>", object → "<object>", else (number/bool/null) as-is.
func redactValue(v any) any {
	switch val := v.(type) {
	case string:
		if strLen(val) > argsSummaryMaxString {
			return fmt.Sprintf("<string: %d chars>", strLen(val))
		}
		return val
	case []any:
		return "<array>"
	case map[string]any:
		return "<object>"
	default:
		// number / bool / null pass through as-is.
		return val
	}
}

// jsonNoEscape marshals v WITHOUT Go's default HTML escaping of <, >, &. The TS
// redactArgs uses JSON.stringify, which leaves "<string: N chars>"/"<array>"
// literal; Go would emit <… and diverge from the wire string. The trailing
// newline encoder.Encode appends is trimmed.
func jsonNoEscape(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

func marshalRedacted(v any) string {
	if s, ok := jsonNoEscape(v); ok {
		return s
	}
	return "<unserializable>"
}
