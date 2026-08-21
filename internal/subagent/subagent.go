// Package subagent runs a bounded, READ-ONLY agent loop in its own isolated
// conversation, so the main thread can learn one answer without paying the
// context cost of finding it.
//
// The problem it solves is specific. Some questions are cheap to ANSWER and
// ruinously expensive to RESEARCH: which of four thousand issues describes the
// terrain flicker; which files under a 900-directory tree belong in a copytree
// scope; where a symbol is actually defined in a repo nobody has read yet. Doing
// that work on the main thread means every listing, every search dump, and every
// dead end lands permanently in the conversation the human is having — it burns
// the context window, drags every later turn's prompt behind it, and buries the
// answer in the transcript that produced it.
//
// A sub-agent runs that search in a conversation the main thread never sees. It
// gets its own history, its own backend session id, its own narrow tool
// inventory, and a hard round budget. When it finishes, exactly ONE thing crosses
// back: a compact report. Everything else — every tool result it read, every
// intermediate round — is written to a durable transcript artifact and dropped
// from live context. The main thread gets the finding and a receipt, not the
// search.
//
// Three properties are load-bearing and deliberately not configurable:
//
//   - **Read-only, structurally.** The tool set is filtered to domain.RiskRead
//     before the loop starts (Deps.Tools is built that way in internal/app), and
//     dispatch runs under domain.ActorSubagent with no Confirm and no AskChoice
//     hook. A sub-agent therefore cannot mutate anything even if the model asks
//     it to: there is no surface to approve on, nobody to approve, and no
//     mutating tool in its inventory. That is what makes it safe to run
//     unattended, in parallel, and without an approval prompt of its own — and
//     it is why subagent.run is itself a read-risk tool.
//   - **Bounded.** Rounds, wall clock, per-result size, and total transcript size
//     are all capped. A runaway sub-agent costs a known maximum and then reports
//     what it has, marked partial — it never silently spends the user's key
//     forever, and it never returns nothing after spending it.
//   - **Never fatal to the caller.** Every failure — a backend error, a bad tool
//     call, a cancel, an exhausted budget — resolves to a Report with a Status,
//     not an error the calling tool has to invent a message for. A sub-agent that
//     could sink the turn that dispatched it would be worse than not delegating.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// Bounds. These cap the worst case rather than describe the normal one: a
// well-briefed sub-agent settles in three to six rounds, and anything near these
// numbers is a run that lost its way.
const (
	// DefaultMaxRounds is the round budget when a brief does not set one. A round
	// is one model call plus its tool batch, so this is ~10 chances to search,
	// read, and verify — comfortably above the observed 3–6 while still bounding a
	// loop that keeps re-reading the same directory.
	DefaultMaxRounds = 10
	// MaxRoundsCeiling is the hard ceiling a brief may not exceed. A model asking
	// for 200 rounds has misunderstood the tool, and honouring it would spend real
	// money on the user's own key.
	MaxRoundsCeiling = 24
	// MaxToolResultChars bounds ONE serialized tool result inside the sub-agent's
	// history. Generous on purpose — a truncated fs.read is a real loss of
	// information, and the whole point of the sub-agent is that it can afford to
	// read things the main thread cannot.
	MaxToolResultChars = 24_000
	// MaxTranscriptChars bounds the sub-agent's accumulated history. Reaching it
	// ends the search and forces the report round: a sub-agent whose own context
	// has grown past this has stopped narrowing and started hoarding, and the
	// answer it has now is the best one it is going to produce.
	MaxTranscriptChars = 220_000
	// minToolResultChars is the floor a clamped result is never taken below, even
	// when the transcript budget is spent. A tool call MUST get a result message or
	// its assistant turn is left unmatched, so the budget can shrink a result but
	// never suppress it.
	minToolResultChars = 400
	// MaxReportChars bounds the report that crosses back into the main thread.
	// This is the number that actually protects the caller's context window, so it
	// is the tightest bound here by a wide margin.
	MaxReportChars = 12_000
	// DefaultTimeout bounds a whole run in wall clock, independent of rounds — a
	// single tool call can block far longer than a round budget suggests.
	DefaultTimeout = 5 * time.Minute
	// reportRoundTimeout bounds the forced wrap-up round, which deliberately runs
	// OUTSIDE the run deadline (it exists to salvage a run that just hit it).
	reportRoundTimeout = 90 * time.Second
)

// Status is a run's terminal outcome. Every field of a Report is meaningful for
// every status: a run that failed or exhausted its budget still reports the
// rounds it spent and the transcript it produced, because "what did it do with my
// money" is exactly the question a failed run raises.
type Status string

const (
	// StatusCompleted means the sub-agent stopped calling tools and produced a
	// report of its own accord. It does NOT assert the report answers the brief —
	// "I could not find it, here is where I looked" is a completed run.
	StatusCompleted Status = "completed"
	// StatusExhausted means a bound was hit (rounds, transcript size, or wall
	// clock) and the report was produced by the forced final round. The finding is
	// real but may be partial, and the caller must say so when it relays it.
	StatusExhausted Status = "exhausted"
	// StatusFailed means the run could not produce a report at all — a backend
	// error that outlived its retries, or a tool inventory that would not project.
	StatusFailed Status = "failed"
	// StatusCancelled means the caller's context was cancelled (the human pressed
	// Escape, or the turn was abandoned).
	StatusCancelled Status = "cancelled"
)

// Partial reports whether a report of this status may be incomplete, and is the
// single place that judgement is made so the tool result and the UI agree.
func (s Status) Partial() bool { return s != StatusCompleted }

// Brief is the assignment. Task is the only required field; everything else
// sharpens the run or bounds it.
type Brief struct {
	// Task is the question, in the orchestrator's own words. It is the whole
	// instruction — a sub-agent has no other steer and cannot ask for one.
	Task string
	// Context is background the sub-agent could not discover for itself: a
	// decision made earlier in the conversation, a constraint the user stated, an
	// identifier already in hand. Optional, and genuinely optional — a sub-agent
	// with tools can find facts, so this is for things that are not IN the project.
	Context string
	// Deliverable names the shape of the answer ("the issue number, title and
	// URL"). Optional but high-value: it is the difference between a report the
	// caller can act on directly and one it has to re-interpret.
	Deliverable string
	// MaxRounds overrides DefaultMaxRounds, clamped to [1, MaxRoundsCeiling].
	MaxRounds int
}

// Report is what crosses back to the main thread — and, apart from the durable
// transcript, the ONLY thing that does.
type Report struct {
	// ID is the sub_… handle for this run.
	ID string `json:"subagentId"`
	// Status is the terminal outcome.
	Status Status `json:"status"`
	// Partial mirrors Status.Partial() onto the wire, so a model reading the
	// result does not have to know which statuses imply incompleteness.
	Partial bool `json:"partial"`
	// Text is the sub-agent's final message: the deliverable.
	Text string `json:"report"`
	// Note explains a non-completed status in one line, for both the model and the
	// human ("hit the 10-round budget"). Empty on a clean completion.
	Note string `json:"note,omitempty"`
	// TranscriptID is the artifact handle holding the full run — every round, every
	// tool call, every result. Readable with artifact.read when the caller needs
	// detail the report omitted. Empty when no transcript sink was wired.
	TranscriptID string `json:"transcriptId,omitempty"`

	Rounds      int `json:"rounds"`
	ToolCalls   int `json:"toolCalls"`
	FailedCalls int `json:"failedCalls,omitempty"`

	PromptTokens     int      `json:"promptTokens,omitempty"`
	CompletionTokens int      `json:"completionTokens,omitempty"`
	CostUSD          *float64 `json:"costUsd,omitempty"`
	DurationMS       int64    `json:"durationMs"`
}

// Backend is the generation seam (satisfied by *backend.Client and by the app's
// Swappable wrapper, so a /login mid-run reaches the next round).
type Backend interface {
	RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error)
}

// Dispatcher is the sub-agent's tool seam. The implementation in internal/app
// filters to read-risk tools and pins domain.ActorSubagent, so the read-only
// guarantee is enforced at construction rather than trusted here — but Run
// re-checks membership per call anyway (see dispatchCall), because "the model
// asked for a tool it was not offered" and "the model was somehow offered a
// mutating tool" must fail the same closed way.
type Dispatcher interface {
	// Tools projects the sub-agent's inventory to model-facing specs. Called once
	// per run.
	Tools() ([]models.ChatTool, error)
	// ResolveWireName maps a wire name (fs__read) back to its internal dotted name
	// (fs.read); "" when unknown.
	ResolveWireName(wireName string) string
	// Dispatch runs one call by internal name. It never returns an error — every
	// failure is a domain.ToolResult.
	Dispatch(ctx context.Context, name, argsJSON string) domain.ToolResult
}

// TranscriptSink stores a finished run's full transcript and returns a handle the
// caller can page back with artifact.read. Optional: a nil sink simply leaves
// Report.TranscriptID empty, which costs the caller its receipt but never the run.
type TranscriptSink interface {
	Put(content string) string
}

// Deps wires one Runner. Backend and Tools are required; the rest degrade.
type Deps struct {
	Backend Backend
	Tools   Dispatcher
	// Transcript persists the full run. Nil ⇒ no transcript handle is reported.
	Transcript TranscriptSink
	// Startup supplies the stable project snapshot the sub-agent needs to know
	// which project it is standing in. Nil ⇒ an empty startup block, which the
	// backend accepts (it is a required VALUE, not a required content).
	Startup func() backend.StartupContext
	// Trace emits a structured debug-log line. Nil ⇒ no tracing. It must never
	// block and must never panic the run (calls are guarded).
	Trace func(event string, fields map[string]any)
	// Now is injectable for deterministic tests. Nil ⇒ time.Now.
	Now func() time.Time
	// Timeout overrides DefaultTimeout. Zero ⇒ DefaultTimeout.
	Timeout time.Duration
}

// Progress reports a run's live state so the calling tool can forward it to the
// cockpit's activity row. Called from the run goroutine; it must not block.
type Progress func(msg string)

// Runner executes briefs. It holds no per-run state, so one Runner serves every
// concurrent sub-agent in a batch — which is what lets the model fan out several
// subagent.run calls in a single tool batch (subagent.run is Parallelizable).
type Runner struct{ deps Deps }

// New builds a Runner. It does not validate deps: a missing Backend or Tools
// surfaces as a StatusFailed report on the first Run, which is the contract every
// other failure here follows.
func New(deps Deps) *Runner { return &Runner{deps: deps} }

func (r *Runner) now() time.Time {
	if r.deps.Now != nil {
		return r.deps.Now()
	}
	return time.Now()
}

func (r *Runner) trace(event string, fields map[string]any) {
	if r.deps.Trace == nil {
		return
	}
	defer func() { _ = recover() }()
	r.deps.Trace(event, fields)
}

// callerGone reports whether the CALLER abandoned this run (cancelled, or its own
// deadline fired) — as opposed to the run simply exhausting its own time budget.
// The two produce the same error on the derived context and want opposite
// handling, so the question is always put to the caller's context directly.
func (st *run) callerGone() bool {
	return st.caller != nil && st.caller.Err() != nil
}

// clampRounds applies the brief's round budget within the package's bounds.
func clampRounds(n int) int {
	switch {
	case n <= 0:
		return DefaultMaxRounds
	case n > MaxRoundsCeiling:
		return MaxRoundsCeiling
	default:
		return n
	}
}

// run carries the mutable state of ONE Run call. It exists so the loop's many
// small steps can share state without either threading a dozen parameters or
// putting per-run state on the Runner (which must stay safe for concurrent runs).
type run struct {
	id        string
	brief     Brief
	maxRounds int
	startedAt time.Time
	// caller is the context handed to Run, BEFORE the run's own timeout is layered
	// on. Two things need it and neither can use the derived context: the wrap-up
	// round must stay cancellable by the human even after the RUN's deadline has
	// fired, and telling "the caller gave up" apart from "we ran out of time"
	// requires asking the caller's context directly — the derived one reports
	// DeadlineExceeded for both.
	caller context.Context

	messages []models.ChatMessage
	// state is the backend's opaque session token, replayed each round. A
	// sub-agent has its own session id, so this never collides with the main
	// thread's — and the backend skips selection for this profile anyway, which
	// makes the token cheap to carry and pointless to omit.
	state string

	rounds int
	// completedRounds counts rounds that SUCCEEDED. rounds counts attempts and is
	// incremented before a failed round returns, so it can never be 0 at a failure
	// site — using it as the "do we have anything to salvage" predicate made that
	// branch dead code, and a round-0 backend failure took the salvage path with no
	// research behind it instead of failing cleanly.
	completedRounds int
	toolCalls       int
	failedCalls     int
	promptTok       int
	completeTok     int
	cost            *float64
	// costKnown: at least one round reported a cost. costComplete: EVERY round that
	// ran reported one. Both are required before a total is published — the
	// codebase rule is that nil means UNKNOWN, never zero, and that a sum missing a
	// contribution is a FLOOR, which must not be rendered as a total.
	costKnown    bool
	costComplete bool

	// transcript accumulates the human-readable run log written to the artifact.
	// It is built as we go rather than reconstructed from messages at the end so
	// it can record things the message list does not carry: per-round timings,
	// truncation notices, and the reason a run stopped.
	transcript strings.Builder
	// historyChars tracks the live history size against MaxTranscriptChars.
	historyChars int
}

// Run executes one brief to a terminal Report. It never returns an error: every
// outcome, including a total failure, is a Report the caller can render and the
// model can read. progress may be nil.
func (r *Runner) Run(ctx context.Context, brief Brief, progress Progress) Report {
	st := &run{
		id:        domain.NewID(domain.PrefixSubagent),
		brief:     brief,
		maxRounds: clampRounds(brief.MaxRounds),
		startedAt: r.now(),
		caller:    ctx,
		// Starts true: with no rounds yet, nothing is missing. Any round that
		// reports no cost poisons it permanently (accumulateUsage).
		costComplete: true,
	}

	if strings.TrimSpace(brief.Task) == "" {
		return r.finish(st, StatusFailed, "", "The brief had no task.")
	}

	timeout := r.deps.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	// The run's own deadline is INDEPENDENT of the round budget: a single tool call
	// can block for minutes, so a 10-round cap alone does not bound wall clock.
	// Derived from the caller's ctx, so a cancelled turn still cancels the run.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	tools, err := r.deps.ToolsOrNil()
	if err != nil {
		return r.finish(st, StatusFailed, "", "Tool inventory could not be projected: "+err.Error())
	}
	btools, err := agent.ToBackendTools(tools)
	if err != nil {
		return r.finish(st, StatusFailed, "", "Tool inventory rejected before send: "+err.Error())
	}
	allowed := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		allowed[t.Function.Name] = struct{}{}
	}

	st.header(r, len(tools))
	st.push(models.TextMessage("user", buildOpeningMessage(brief, st.maxRounds)))

	r.trace("subagent.start", map[string]any{
		"subagentId": st.id,
		"task":       clampRunes(brief.Task, 200),
		"maxRounds":  st.maxRounds,
		"toolCount":  len(tools),
	})

	report := r.loop(ctx, st, btools, allowed, progress)
	r.trace("subagent.end", map[string]any{
		"subagentId": report.ID,
		"status":     string(report.Status),
		"rounds":     report.Rounds,
		"toolCalls":  report.ToolCalls,
		"durationMs": report.DurationMS,
		"reportLen":  len([]rune(report.Text)),
	})
	return report
}

// ToolsOrNil projects the inventory, mapping a nil Dispatcher to a clean error
// rather than a nil-pointer panic (Deps are not validated at construction).
func (d Deps) ToolsOrNil() ([]models.ChatTool, error) {
	if d.Tools == nil {
		return nil, errors.New("no tool dispatcher wired")
	}
	return d.Tools.Tools()
}

// loop runs rounds until the sub-agent stops calling tools or a bound is hit.
//
// The final round is special and always paid for: when a bound ends the search,
// the loop spends ONE more round with tools withheld (tool_choice "none") to make
// the model write up what it has. Without it, an exhausted run returns an
// assistant message that is mid-search — a tool call, or a sentence about what it
// was going to look at next — which is worth nothing to the caller after it has
// already spent the whole budget getting there.
func (r *Runner) loop(
	ctx context.Context,
	st *run,
	btools []backend.Tool,
	allowed map[string]struct{},
	progress Progress,
) Report {
	// nudged latches the one-shot recovery for a silent round (see below).
	nudged := false
	for {
		if ctx.Err() != nil {
			// Ask the CALLER's context, not the derived one. The derived context
			// reports DeadlineExceeded both when OUR timeout fired and when the
			// caller's own deadline did — and those want opposite treatment: our
			// timeout means "stop searching, write up what you have", the caller's
			// means the work is no longer wanted and must not be continued for
			// another 90 seconds under a shed deadline.
			if st.callerGone() {
				return r.finish(st, StatusCancelled, "", "Cancelled before the run completed.")
			}
			return r.reportRound(ctx, st, "the time budget ran out", progress)
		}
		if st.rounds >= st.maxRounds {
			return r.reportRound(ctx, st, fmt.Sprintf("the %d-round budget ran out", st.maxRounds), progress)
		}
		if st.historyChars >= MaxTranscriptChars {
			return r.reportRound(ctx, st, "the context budget ran out", progress)
		}

		emit(progress, fmt.Sprintf("round %d/%d", st.rounds+1, st.maxRounds))

		result, err := r.round(ctx, st, btools, "auto")
		if err != nil {
			if ctx.Err() != nil {
				if st.callerGone() {
					return r.finish(st, StatusCancelled, "", "Cancelled before the run completed.")
				}
				return r.reportRound(ctx, st, "the time budget ran out", progress)
			}
			// A backend failure mid-search is not automatically fatal: if the
			// sub-agent has already read something, one report round on the history
			// it has is worth more than nothing. A failure before ANY round
			// succeeded has nothing to salvage — and the predicate is
			// completedRounds, not rounds, because rounds counts ATTEMPTS and is
			// already incremented by the failed one.
			if st.completedRounds == 0 {
				return r.finish(st, StatusFailed, "", "The backend call failed: "+err.Error())
			}
			return r.reportRound(ctx, st, "the backend call failed: "+err.Error(), progress)
		}

		calls := agent.BackendToolCalls(result.Message.ToolCalls)
		st.push(agent.BackendAssistantMessage(result.Message))

		if len(calls) == 0 {
			text := strings.TrimSpace(result.Message.Content)
			if text == "" {
				// A round that produced neither prose nor a tool call is a dead
				// round. Nudge ONCE — it is cheap and usually recovers — then stop
				// asking. The round budget alone would bound this, but spending eight
				// remaining rounds re-asking a model that has gone silent buys
				// nothing, and the wrap-up round is a better use of the last one.
				if nudged {
					return r.reportRound(ctx, st, "the sub-agent stopped producing output", progress)
				}
				nudged = true
				st.note("round produced no content and no tool calls — nudging once for the report")
				st.push(models.TextMessage("user",
					"That round produced nothing. Write your final report now, from what you have already found."))
				continue
			}
			return r.finish(st, StatusCompleted, text, "")
		}

		emit(progress, fmt.Sprintf("round %d/%d · %s", st.rounds, st.maxRounds, callSummary(calls)))
		r.dispatchBatch(ctx, st, calls, allowed)
	}
}

// reportRound spends the final round: tools withheld, one instruction to write up
// what was found. Its failures degrade rather than escalate — if even this round
// cannot run, the run still reports, using the last prose the sub-agent produced.
func (r *Runner) reportRound(ctx context.Context, st *run, why string, progress Progress) Report {
	note := "Stopped early: " + why + "."
	emit(progress, "writing report")
	st.note("bound reached (" + why + ") — forcing the report round")

	// Detached from the (possibly already-expired) RUN deadline, on a short budget
	// of its own: the whole point of this round is to salvage a run whose time ran
	// out, and inheriting the dead deadline would make it fail on entry.
	//
	// The caller's cancel is then re-linked UNCONDITIONALLY. It used to be linked
	// only when the run context was still live, which meant the one case that
	// matters most — the deadline has fired, we are now spending up to another 90
	// seconds — was exactly the case where pressing Escape did nothing.
	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), reportRoundTimeout)
	defer cancel()
	reportCtx, stop := withCallerCancel(reportCtx, st.caller)
	defer stop()

	st.push(models.TextMessage("user",
		"STOP SEARCHING — "+why+". Write your final report now, using only what you have already found. "+
			"State plainly which parts of the brief you did not get to and where you looked, so the "+
			"orchestrator knows what is missing."))

	// tool_choice "none" rather than an empty inventory: the history already
	// contains tool calls, and some providers reject a conversation whose tool
	// calls reference tools absent from the request.
	result, err := r.round(reportCtx, st, nil, "none")
	if err != nil {
		if text := st.lastAssistantText(); text != "" {
			return r.finish(st, StatusExhausted, text,
				note+" The wrap-up round also failed, so this is the sub-agent's last prose, not a report.")
		}
		return r.finish(st, StatusFailed, "", note+" It produced no report: "+err.Error())
	}
	text := strings.TrimSpace(result.Message.Content)
	// Tools were WITHHELD for this round (tool_choice "none"), so any tool call
	// here is the provider ignoring that. Push the message WITHOUT them: they will
	// never be dispatched, and an assistant turn carrying unmatched tool calls
	// violates the one-result-per-call invariant the rest of the loop maintains.
	// The transcript records that it happened.
	reported := result.Message
	if len(reported.ToolCalls) > 0 {
		st.note(fmt.Sprintf("wrap-up round returned %d tool call(s) despite tool_choice=none — dropped", len(reported.ToolCalls)))
		reported.ToolCalls = nil
	}
	st.push(agent.BackendAssistantMessage(reported))
	if text == "" {
		if text = st.lastAssistantText(); text == "" {
			return r.finish(st, StatusFailed, "", note+" It produced no report.")
		}
	}
	return r.finish(st, StatusExhausted, text, note)
}

// round performs one backend generation. It streams (the CLI's transport is built
// around the streamed path and its retry policy) but discards the deltas: nobody
// is reading a sub-agent's tokens live, so only the settled message matters.
func (r *Runner) round(ctx context.Context, st *run, btools []backend.Tool, toolChoice string) (backend.RespondResult, error) {
	if r.deps.Backend == nil {
		return backend.RespondResult{}, errors.New("no backend wired")
	}
	msgs, err := agent.ToBackendMessages(st.messages)
	if err != nil {
		return backend.RespondResult{}, err
	}
	var startup backend.StartupContext
	if r.deps.Startup != nil {
		startup = r.deps.Startup()
	}
	var statePtr *string
	if st.state != "" {
		s := st.state
		statePtr = &s
	}

	req := backend.RespondRequest{
		Profile: backend.ProfileSubagent,
		Session: backend.RespondSession{
			// The sub-agent's OWN session id. This is what isolates it: the backend
			// keys skill state and selector cadence on it, so a sub-agent can never
			// disturb the main conversation's state — and vice versa.
			ID:     st.id,
			TurnID: st.id + "-r" + fmt.Sprint(st.rounds),
			Round:  st.rounds,
		},
		Startup: startup,
		State:   statePtr,
		Input: backend.RespondInput{
			Messages:   msgs,
			Tools:      btools,
			ToolChoice: toolChoice,
		},
		Generation: &backend.Generation{ResponseFormat: "text"},
	}

	start := r.now()
	result, err := r.deps.Backend.RespondStream(ctx, req, backend.StreamCallbacks{
		OnMeta: func(m backend.StreamMeta) {
			if m.State != "" {
				st.state = m.State
			}
		},
	})
	st.rounds++
	if err != nil {
		st.note(fmt.Sprintf("round %d: backend error after %s: %v", st.rounds, r.now().Sub(start).Round(time.Millisecond), err))
		return result, err
	}
	st.completedRounds++
	st.accumulateUsage(result)
	st.roundHeader(st.rounds, r.now().Sub(start), result)
	return result, nil
}

// dispatchBatch runs one round's tool calls in order and appends their results.
//
// Serial, deliberately. The registry's parallel-dispatch machinery is opt-in per
// tool and reasons about batches from the MAIN loop; a sub-agent's batches are
// small, its calls are cheap reads, and running them in order keeps the transcript
// readable and the failure attribution exact. The concurrency that matters for
// sub-agents is between RUNS (the model fans out several subagent.run calls), not
// within one.
func (r *Runner) dispatchBatch(ctx context.Context, st *run, calls []models.ToolCallRequest, allowed map[string]struct{}) {
	for _, call := range calls {
		if ctx.Err() != nil {
			// Stub the remainder so the transcript stays structurally valid: every
			// assistant tool call needs its matching tool result, or the next
			// request is malformed.
			st.pushToolResult(call, failResult("SUBAGENT_CANCELLED", "The sub-agent run was cancelled before this call ran."))
			continue
		}
		st.toolCalls++
		res := r.dispatchCall(ctx, st, call, allowed)
		if !res.Ok {
			st.failedCalls++
		}
		st.pushToolResult(call, res)
	}
}

// dispatchCall resolves one call's name and runs it, failing CLOSED on anything
// unexpected. The membership re-check is not redundant with the app's read-only
// filter: it is the guarantee that holds even if a future wiring change hands this
// package a wider dispatcher than it should have.
func (r *Runner) dispatchCall(ctx context.Context, st *run, call models.ToolCallRequest, allowed map[string]struct{}) domain.ToolResult {
	wire := call.Function.Name
	if _, ok := allowed[wire]; !ok {
		st.note("refused out-of-inventory tool " + wire)
		return failResult("TOOL_NOT_OFFERED", fmt.Sprintf(
			"%q is not one of the read-only tools available to you. Sub-agents cannot change anything — "+
				"if the task needs a change, say so in your report instead.", wire))
	}
	name := r.deps.Tools.ResolveWireName(wire)
	if name == "" {
		return failResult("TOOL_UNKNOWN", fmt.Sprintf("%q did not resolve to a known tool.", wire))
	}
	args := call.Function.Arguments
	if strings.TrimSpace(args) == "" {
		args = "{}"
	}
	return r.deps.Tools.Dispatch(ctx, name, args)
}

// finish stamps the terminal report and writes the transcript artifact.
func (r *Runner) finish(st *run, status Status, text, note string) Report {
	text = clampRunes(stripReportPreamble(strings.TrimSpace(text)), MaxReportChars)
	st.footer(status, note, text)

	rep := Report{
		ID:               st.id,
		Status:           status,
		Partial:          status.Partial(),
		Text:             text,
		Note:             note,
		Rounds:           st.rounds,
		ToolCalls:        st.toolCalls,
		FailedCalls:      st.failedCalls,
		PromptTokens:     st.promptTok,
		CompletionTokens: st.completeTok,
		DurationMS:       r.now().Sub(st.startedAt).Milliseconds(),
	}
	// Published ONLY when every round that ran reported a cost. A partial sum is a
	// FLOOR, and the one thing the codebase's cost rule forbids is rendering a
	// floor as a total — nil (unknown) is the honest answer instead. The per-round
	// detail is in the transcript for anyone who needs the lower bound.
	if st.costKnown && st.costComplete {
		rep.CostUSD = st.cost
	}
	if r.deps.Transcript != nil {
		// Best-effort and last: a transcript sink that fails must not turn a
		// successful run into a failed one. The report is already complete.
		func() {
			defer func() { _ = recover() }()
			rep.TranscriptID = r.deps.Transcript.Put(st.transcript.String())
		}()
	}
	return rep
}

// --- run helpers -----------------------------------------------------------

func (st *run) push(m models.ChatMessage) {
	st.messages = append(st.messages, m)
	st.historyChars += messageChars(m)
}

// messageChars sizes one message for the MaxTranscriptChars budget.
//
// Tool-call ARGUMENTS are counted, not just content. An assistant turn that only
// calls tools carries null content, so sizing by ContentToText alone scored it at
// zero while it really contributed every byte of its arguments — leaving the one
// bound that is supposed to stop a sub-agent hoarding context blind to a whole
// component of that context. A search-heavy run is mostly these turns, which is
// exactly the run the bound exists for.
func messageChars(m models.ChatMessage) int {
	n := len([]rune(m.ContentToText()))
	for _, c := range m.ToolCalls {
		n += len([]rune(c.Function.Name)) + len([]rune(c.Function.Arguments))
	}
	return n
}

// pushToolResult appends a tool result, clamped, and records it in the transcript
// at FULL length. That split is the whole design: the sub-agent's live context
// pays the clamped size, while the durable record keeps everything, so a caller
// paging the transcript later sees what the sub-agent actually read.
func (st *run) pushToolResult(call models.ToolCallRequest, res domain.ToolResult) {
	full := serializeResult(res)
	clamped := st.clampForBudget(full)
	st.messages = append(st.messages, models.ChatMessage{
		Role:          "tool",
		ToolCallID:    call.ID,
		Name:          call.Function.Name,
		StringContent: clamped,
	})
	st.historyChars += len([]rune(clamped))

	st.transcript.WriteString("\n  → " + call.Function.Name + " " + clampRunes(oneLine(call.Function.Arguments), 300) + "\n")
	status := "ok"
	if !res.Ok {
		status = "FAILED"
	}
	st.transcript.WriteString("    [" + status + "] " + full + "\n")
}

// truncationNote is appended to a clamped tool result. It is part of the BUDGET,
// not an addition to it — counting only the payload made MaxToolResultChars a
// number the result reliably exceeded.
const truncationNote = "\n\n[result truncated — narrow the query if you need the rest]"

// clampForBudget sizes one tool result against BOTH the per-result cap and what
// remains of the whole-history budget.
//
// Without the second half, MaxTranscriptChars was a post-batch tripwire rather
// than a bound: the check runs at the top of the next loop iteration, so a single
// round requesting eight tools could add eight near-cap results — ~192k runes —
// and history only noticed once it was already far past the limit. The forced
// wrap-up round then had to run on exactly the oversized context the bound existed
// to prevent.
//
// The floor matters as much as the cap: every call still gets a REAL result
// message, however small, because dropping one would leave its tool call unmatched
// and the next request malformed.
func (st *run) clampForBudget(full string) string {
	allow := MaxToolResultChars
	if remaining := MaxTranscriptChars - st.historyChars; remaining < allow {
		allow = remaining
	}
	if allow < minToolResultChars {
		allow = minToolResultChars
	}
	if len([]rune(full)) <= allow {
		return full
	}
	// The note rides inside the allowance so the stored value never exceeds it.
	body := allow - len([]rune(truncationNote))
	if body < 1 {
		body = 1
	}
	return clampRunes(full, body) + truncationNote
}

func (st *run) note(msg string) { st.transcript.WriteString("\n  ! " + msg + "\n") }

func (st *run) header(r *Runner, toolCount int) {
	st.transcript.WriteString("# Sub-agent run " + st.id + "\n\n")
	st.transcript.WriteString("started:     " + st.startedAt.Format(time.RFC3339) + "\n")
	st.transcript.WriteString("round budget: " + fmt.Sprint(st.maxRounds) + "\n")
	st.transcript.WriteString("tools:        " + fmt.Sprint(toolCount) + " (read-only)\n\n")
	st.transcript.WriteString("## Brief\n\n" + st.brief.Task + "\n")
	if st.brief.Context != "" {
		st.transcript.WriteString("\ncontext: " + st.brief.Context + "\n")
	}
	if st.brief.Deliverable != "" {
		st.transcript.WriteString("\ndeliverable: " + st.brief.Deliverable + "\n")
	}
}

// roundHeader records one round in the transcript. Prose is written only for a
// round that ALSO called tools — a round with no calls is the sub-agent's final
// message, which the footer records under "## Report", and writing it in both
// places gave every transcript the same text twice (seen on the first live run).
func (st *run) roundHeader(n int, dur time.Duration, result backend.RespondResult) {
	st.transcript.WriteString(fmt.Sprintf("\n## Round %d  (%s)\n", n, dur.Round(time.Millisecond)))
	if len(result.Message.ToolCalls) == 0 {
		return
	}
	if txt := strings.TrimSpace(result.Message.Content); txt != "" {
		st.transcript.WriteString("\n" + txt + "\n")
	}
}

func (st *run) footer(status Status, note, text string) {
	st.transcript.WriteString("\n## Outcome\n\n")
	st.transcript.WriteString("status:     " + string(status) + "\n")
	if note != "" {
		st.transcript.WriteString("note:       " + note + "\n")
	}
	// The round count INCLUDES the forced wrap-up round, so an exhausted run
	// legitimately reports one more round than its budget. Say so rather than leave
	// a reader to reconcile "rounds: 11" against "the 10-round budget ran out".
	roundNote := ""
	if status == StatusExhausted {
		roundNote = " (includes the forced wrap-up round)"
	}
	st.transcript.WriteString(fmt.Sprintf("rounds:     %d%s\ntool calls: %d (%d failed)\n",
		st.rounds, roundNote, st.toolCalls, st.failedCalls))
	st.transcript.WriteString("\n## Report\n\n" + text + "\n")
}

// lastAssistantText is the salvage path: the most recent prose the sub-agent
// produced, used when the wrap-up round itself cannot run.
func (st *run) lastAssistantText() string {
	for i := len(st.messages) - 1; i >= 0; i-- {
		m := st.messages[i]
		if m.Role != "assistant" {
			continue
		}
		if txt := strings.TrimSpace(m.ContentToText()); txt != "" {
			return txt
		}
	}
	return ""
}

func (st *run) accumulateUsage(result backend.RespondResult) {
	st.promptTok += result.Usage.PromptTokens
	st.completeTok += result.Usage.CompletionTokens
	// Cost is a POINTER upstream because nil means "the provider reported
	// nothing", which is NOT zero.
	//
	// The subtlety this got wrong at first: latching "we saw a cost" is not the
	// same as "the total is complete". With round 1 at $0.01 and round 2 reporting
	// nothing, a single latch published $0.01 as the run's cost — a floor wearing a
	// total's clothes, which is precisely what the codebase's cost rule exists to
	// prevent. So completeness is tracked separately and is poisoned permanently by
	// ANY unreported round, in either order.
	if result.Cost == nil {
		// Terminal: one silent round makes any total a floor, whichever order the
		// rounds arrived in.
		st.costComplete = false
		return
	}
	total := result.Cost.Total
	if st.cost == nil {
		st.cost = &total
	} else {
		sum := *st.cost + total
		st.cost = &sum
	}
	st.costKnown = true
}

// --- pure helpers ----------------------------------------------------------

// buildOpeningMessage renders the brief as the sub-agent's first user message.
// The budget is stated in the message rather than left implicit because the
// persona prompt tells the model to spend it deliberately — an instruction it
// cannot follow without knowing the number.
func buildOpeningMessage(b Brief, maxRounds int) string {
	var sb strings.Builder
	sb.WriteString("## Your task\n\n")
	sb.WriteString(strings.TrimSpace(b.Task))
	sb.WriteString("\n")
	if c := strings.TrimSpace(b.Context); c != "" {
		sb.WriteString("\n## Context from the orchestrator\n\n")
		sb.WriteString(c)
		sb.WriteString("\n")
	}
	if d := strings.TrimSpace(b.Deliverable); d != "" {
		sb.WriteString("\n## What to report back\n\n")
		sb.WriteString(d)
		sb.WriteString("\n")
	}
	sb.WriteString(fmt.Sprintf(
		"\n## Budget\n\nYou have %d rounds. Each round is one message from you plus any tools you call in it. "+
			"Nobody is reading this conversation — write no prose until the final report.\n", maxRounds))
	return sb.String()
}

// serializeResult renders a ToolResult for the sub-agent's history. Compact JSON
// of the same {ok,summary,result,error} envelope the main loop uses, so the model
// reads the shape it was trained on in this system.
func serializeResult(res domain.ToolResult) string {
	payload := struct {
		Ok      bool              `json:"ok"`
		Summary string            `json:"summary,omitempty"`
		Result  any               `json:"result,omitempty"`
		Error   *domain.ToolError `json:"error,omitempty"`
	}{Ok: res.Ok, Summary: res.Summary, Result: res.Result, Error: res.Error}
	b, err := json.Marshal(payload)
	if err != nil {
		// Never lose the call: an unserializable result still has a summary, and
		// reporting that beats handing the model an empty tool message it cannot
		// interpret.
		return fmt.Sprintf(`{"ok":%t,"summary":%q,"error":{"code":"SERIALIZE_FAILED","message":%q}}`,
			res.Ok, res.Summary, err.Error())
	}
	return string(b)
}

func failResult(code, msg string) domain.ToolResult { return domain.Fail(code, msg) }

// reportPreambleHeadings are the self-titling lines a sub-agent sometimes puts
// above its answer. Matched case-insensitively against a whole trimmed line.
var reportPreambleHeadings = []string{"## report", "### report", "# report", "**report**", "report:"}

// stripReportPreamble removes a redundant self-titling heading ("## Report") when
// it is the FIRST thing in the report, so the report leads with the answer.
//
// It exists because of what the first live run produced: a report beginning
// "I have everything needed.\n\n## Report\n\n**Dispatch is defined at …**". Those
// wasted lines are invisible to the sub-agent — nobody reads its draft — and land
// squarely in the caller's context, which is the one thing this feature protects.
//
// It strips ONLY a heading that leads the report, and that limit is deliberate.
// "I have everything needed. / ## Report / <answer>" and "The answer is X. /
// ## Report / <detail>" are structurally identical, so a rule that removed
// everything above the heading would delete a real answer as readily as a
// throat-clear. Deleting the answer costs the whole delegation; leaving two stray
// lines costs about ten tokens. The prompt (main/subagent/30-report.md) is what
// actually stops the preamble being written; this only catches the common
// leading-heading case, and never guesses.
func stripReportPreamble(text string) string {
	for i, ln := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if isReportHeading(strings.ToLower(trimmed)) {
			return strings.TrimSpace(strings.Join(strings.Split(text, "\n")[i+1:], "\n"))
		}
		// The first real line is not a heading, so the report has already started.
		return text
	}
	return text
}

func isReportHeading(lowerTrimmed string) bool {
	for _, h := range reportPreambleHeadings {
		if lowerTrimmed == h {
			return true
		}
	}
	return false
}

// callSummary names a round's tool batch for the progress line, deduped and
// capped — "fs.search, fs.read" reads better than a count, and better than eight
// repetitions of one name.
func callSummary(calls []models.ToolCallRequest) string {
	seen := make(map[string]struct{}, len(calls))
	names := make([]string, 0, len(calls))
	for _, c := range calls {
		n := strings.ReplaceAll(c.Function.Name, "__", ".")
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
		if len(names) == 3 {
			break
		}
	}
	out := strings.Join(names, ", ")
	if len(calls) > len(names) {
		out += fmt.Sprintf(" (+%d)", len(calls)-len(names))
	}
	return out
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(s, "\n", " ")), " ")
}

// clampRunes truncates to n RUNES (never bytes — a byte cut can split a multibyte
// character and produce invalid UTF-8 on the wire).
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func emit(p Progress, msg string) {
	if p == nil {
		return
	}
	defer func() { _ = recover() }()
	p(msg)
}

// withCallerCancel returns a context that is cancelled when EITHER base or the
// caller's ctx is. It exists for the wrap-up round, which sheds the run deadline
// but must still honour an Escape.
func withCallerCancel(base, caller context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(base)
	var once sync.Once
	stop := func() { once.Do(cancel) }
	go func() {
		select {
		case <-caller.Done():
			stop()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}
