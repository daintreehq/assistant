package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/redact"
)

// approvals.go is how a mutating turn gets permission when the only "human" on the
// pipe is another agent.
//
// The two obvious designs are both wrong on their own. Blanket auto-approve makes the
// assistant perform git pushes and terminal work with nothing gating it. Blanket
// auto-decline — what this server did before — means a session can never do the
// mutating work it exists for, and the turn silently completes having skipped it.
//
// So the mode is chosen per session and the parked case is made SAFE: an unanswered
// approval fails closed on a timer, cancellation unparks it, and teardown rejects every
// outstanding one. A dispatch goroutine can therefore never be parked forever, which is
// the failure that would otherwise wedge a session with no way out.
//
// WHAT THIS IS NOT. The middle mode used to be called "ask", and the name was a lie by
// implication. Nobody is asked. The pending approval is handed to the SAME model that is
// driving the session, which then calls daintree.approve — so a request the assistant
// made is answered by the agent that prompted it, and any repository text able to steer
// that agent can steer the answer too. That is workflow delegation, and it is genuinely
// useful for a harness driving a controlled project; it is not human authorization, and
// naming it "ask" invited exactly the wrong inference from a caller deciding whether it
// was safe to enable. So the mode is DELEGATE and every pending approval says whose
// decision it actually is (see PendingApproval.DecisionAuthority).

// ApprovalMode is how a session answers a confirmation request.
type ApprovalMode string

const (
	// ApprovalDecline refuses every mutating tool immediately. The safe default and the
	// only sensible one for an unattended caller that is not watching for approvals:
	// the turn continues, having skipped the call, and the refusal is in the timeline.
	ApprovalDecline ApprovalMode = "decline"
	// ApprovalDelegate parks the call and hands it to the CALLER AGENT to decide. It is
	// explicitly not a human safety boundary — see the file comment. Useful for a
	// harness driving a controlled project; wrong for a session over a repository whose
	// contents could steer the caller.
	ApprovalDelegate ApprovalMode = "delegate"
	// ApprovalAuto never asks — dispatch skips the confirm hook entirely because the
	// session set the runtime's auto-approve. Reported, never inferred.
	ApprovalAuto ApprovalMode = "auto"
)

// Valid reports whether m is a known mode.
func (m ApprovalMode) Valid() bool {
	switch m {
	case ApprovalDecline, ApprovalDelegate, ApprovalAuto:
		return true
	}
	return false
}

// DecisionAuthority names who actually settles an approval in this mode. It is reported
// on every pending approval so the answer is in the payload rather than in a caller's
// assumption about what a mode name implies.
func (m ApprovalMode) DecisionAuthority() string {
	switch m {
	case ApprovalDelegate:
		// Not "human". The caller agent decides, and it is the same agent that asked.
		return "caller-agent"
	case ApprovalAuto:
		return "none"
	default:
		return "none"
	}
}

// Decision is the outcome of one approval.
type Decision string

const (
	DecisionApproved Decision = "approved"
	DecisionRejected Decision = "rejected"
	// DecisionTimeout is an approval nobody answered. It denies the call — failing open
	// on silence would make the timeout a way to get anything approved by waiting.
	DecisionTimeout Decision = "timeout"
	// DecisionCancelled is an approval unparked because the turn ended under it.
	DecisionCancelled Decision = "cancelled"
)

// DefaultApprovalTimeout bounds an unanswered approval. It is long enough for a caller
// that polls on a slow cadence and short enough that a forgotten approval does not pin
// a dispatch goroutine — and with it the whole turn — for the session's lifetime.
const DefaultApprovalTimeout = 5 * time.Minute

// PendingApproval is one parked confirmation, as reported to a caller.
type PendingApproval struct {
	ID   string `json:"id"`
	Tool string `json:"tool"`
	// Risk is the tool's risk class (terminal, project, git, system, …) — the reason it
	// needs approval at all.
	Risk string `json:"risk"`
	// Consequence is the human-readable "what this will do", written by the tool.
	Consequence string `json:"consequence,omitempty"`
	Summary     string `json:"summary,omitempty"`
	// Args is a REDACTED preview of the arguments. A caller cannot judge "push to
	// origin" without seeing which remote and branch, but the raw args can carry
	// credentials, so they pass through the same redactor the audit rows use.
	Args        string `json:"args,omitempty"`
	RequestedAt int64  `json:"requestedAt"`
	// RunID ties the approval to the turn that is blocked on it.
	RunID string `json:"runId,omitempty"`
	// NeedsTypedConfirm is the safety layer's own verdict that this action deserves
	// more friction than a yes. The caller renders its own approval UX and this server
	// cannot impose a keystroke on it — but dropping the flag made a system-risk
	// action and an ordinary project mutation arrive as the same boolean with
	// different prose, which is exactly the distinction a caller needs to apply its
	// own friction. Reported, never enforced here.
	// It is NOT omitempty. A caller distinguishing "this action does not need extra
	// friction" from "the peer is too old to tell me" cannot do it from an absent field,
	// and an approval whose friction requirement silently disappeared is the one an
	// automated caller waves through.
	NeedsTypedConfirm bool `json:"needsTypedConfirm"`
	// DecisionAuthority says whose decision releases this call — "caller-agent" under
	// delegate, "none" otherwise. Present so a caller reads the authority off the
	// payload instead of inferring it from a mode name, which is how "ask" came to be
	// read as human authorization.
	DecisionAuthority string `json:"decisionAuthority"`

	resolve chan Decision
	timer   *time.Timer
}

// ApprovalRequest is what a tool dispatch asks about.
//
// It carries the dispatch's NeedsTypedConfirm verdict verbatim, matching the embedded
// host. Typed-confirm friction is ENFORCED only on the surfaces that render an approval
// sheet — the host and the line REPL — never on one that delegates the decision to an
// external caller owning its own approval UX. But forwarding the verdict costs nothing
// and is the only way that caller can tell a system-risk action from an ordinary
// project mutation without re-deriving the safety layer's rules from the risk class.
type ApprovalRequest struct {
	Tool              string
	Risk              domain.RiskClass
	Consequence       string
	Summary           string
	RawArgs           string
	RunID             string
	NeedsTypedConfirm bool
}

// Approvals brokers confirmations for one session.
type Approvals struct {
	mode    ApprovalMode
	timeout time.Duration
	// notify, when set, is called on a NEW request so a transport that can push
	// (MCP elicitation) may ask the client directly instead of waiting to be polled.
	// It must not block; it is invoked on its own goroutine.
	notify func(PendingApproval)
	// onChange, when set, is called with the affected run id whenever the pending set
	// changes. A run parked on an approval emits no further events of its own, so
	// without this a long poll would sit through the whole wait budget without ever
	// reporting that the turn had STOPPED rather than merely being slow. Set once at
	// construction, so it cannot race the per-ask SetNotify rebinding.
	onChange func(runID string)

	mu      sync.Mutex
	pending map[string]*PendingApproval
	order   []string
	// decided keeps the last outcomes so a caller that polls after the timer fired
	// learns WHY its approval vanished rather than finding it simply gone.
	decided map[string]Decision
	// decidedOrder makes the eviction FIFO. Dropping an arbitrary map entry would be
	// bounded but capricious — it could evict the outcome a caller is about to ask
	// about while keeping one from an hour ago.
	decidedOrder []string
}

// MaxApprovalTimeout caps how long an approval may park. It exists because the timeout
// is the ONLY thing that bounds a parked dispatch when nobody answers, so a caller must
// not be able to stretch it to the point where it stops being a bound.
const MaxApprovalTimeout = time.Hour

// NewApprovals builds a broker. A zero timeout uses DefaultApprovalTimeout.
//
// A non-positive or over-long timeout is CLAMPED, never honoured: a disabled timer would
// let an unanswered approval park the dispatch — and with it the turn and the session's
// teardown — forever. There is deliberately no way to switch it off, from inside this
// package or from a tool argument.
func NewApprovals(mode ApprovalMode, timeout time.Duration) *Approvals {
	if !mode.Valid() {
		mode = ApprovalDecline
	}
	switch {
	case timeout <= 0:
		timeout = DefaultApprovalTimeout
	case timeout > MaxApprovalTimeout:
		timeout = MaxApprovalTimeout
	}
	return &Approvals{
		mode:    mode,
		timeout: timeout,
		pending: map[string]*PendingApproval{},
		decided: map[string]Decision{},
	}
}

// Mode reports the configured mode.
func (a *Approvals) Mode() ApprovalMode { return a.mode }

// Timeout is how long an unanswered approval parks before it is denied.
func (a *Approvals) Timeout() time.Duration { return a.timeout }

// SetOnChange installs the pending-set change hook. Set once, at construction.
func (a *Approvals) SetOnChange(fn func(runID string)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.onChange = fn
}

// SetNotify installs the push hook (elicitation). Safe to call before any request.
func (a *Approvals) SetNotify(fn func(PendingApproval)) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.notify = fn
}

// Confirm is the tool-confirm hook. It runs on the agent's dispatch goroutine and
// blocks there until the approval settles, so every exit path must be bounded.
func (a *Approvals) Confirm(ctx context.Context, req ApprovalRequest) bool {
	// Auto never reaches here in practice (dispatch skips the hook when the runtime's
	// auto-approve is set), but answering true keeps the two paths consistent if it
	// ever does.
	if a.mode == ApprovalAuto {
		return true
	}
	if a.mode != ApprovalDelegate {
		return false
	}

	pa := &PendingApproval{
		ID:          domain.NewID("apr_"),
		Tool:        req.Tool,
		Risk:        string(req.Risk),
		Consequence: req.Consequence,
		Summary:     req.Summary,
		// The same redactor that guards the durable audit rows and the attached session's
		// approval sheet. An args preview a caller cannot see is useless; one that
		// leaks a token is worse than useless.
		Args:        redact.String(req.RawArgs),
		RequestedAt: domain.NowMS(),
		RunID:       req.RunID,
		resolve:     make(chan Decision, 1),

		NeedsTypedConfirm: req.NeedsTypedConfirm,
		DecisionAuthority: a.mode.DecisionAuthority(),
	}

	a.mu.Lock()
	// Always armed — see NewApprovals for why there is no unbounded case. Started
	// inside the lock so the callback (which takes the same lock) cannot fire before
	// the map insert below; AfterFunc itself returns immediately, so this cannot
	// self-deadlock.
	pa.timer = time.AfterFunc(a.timeout, func() { a.Resolve(pa.ID, DecisionTimeout) })
	a.pending[pa.ID] = pa
	a.order = append(a.order, pa.ID)
	notify := a.notify
	onChange := a.onChange
	// Copy only the REPORTABLE fields: the resolve channel and the timer are this
	// broker's business, and handing them to an external hook would let it settle an
	// approval behind Resolve's back, skipping the bookkeeping entirely.
	snapshot := PendingApproval{
		ID: pa.ID, Tool: pa.Tool, Risk: pa.Risk, Consequence: pa.Consequence,
		Summary: pa.Summary, Args: pa.Args, RequestedAt: pa.RequestedAt, RunID: pa.RunID,
		NeedsTypedConfirm: pa.NeedsTypedConfirm,
		DecisionAuthority: pa.DecisionAuthority,
	}
	a.mu.Unlock()

	if onChange != nil {
		onChange(pa.RunID)
	}
	if notify != nil {
		go notify(snapshot)
	}

	select {
	case d := <-pa.resolve:
		return settleDecision(d, ctx.Err() != nil)
	case <-ctx.Done():
		// The turn was cancelled or the session is closing. Unpark as a denial so the
		// dispatch returns rather than holding the goroutine — and so teardown, which
		// waits for the turn, is not blocked by an approval nobody will ever answer.
		a.Resolve(pa.ID, DecisionCancelled)
		return false
	}
}

// settleDecision folds a decision and the turn's cancellation state into the one answer
// the dispatch gets. Cancellation DOMINATES: when a decision and a cancellation are both
// ready on Confirm's select, Go picks arbitrarily, so an approved mutating call could
// otherwise run after interrupt or close had already stopped the turn — the one outcome
// a caller who pressed stop must never get.
//
// It is a function rather than two lines inline because the race it guards cannot be
// scheduled on demand from a test: normally the cancellation wakes Confirm before the
// decision is even sent, and the window where BOTH are ready is a descheduling accident.
// Pulling the rule out makes it directly assertable.
func settleDecision(d Decision, cancelled bool) bool {
	if cancelled {
		return false
	}
	return d == DecisionApproved
}

// Resolve settles an outstanding approval. Reports false when there was nothing
// pending under that id, which lets a tool tell "decided" from "never existed".
func (a *Approvals) Resolve(id string, d Decision) bool {
	a.mu.Lock()
	pa, ok := a.pending[id]
	if !ok {
		a.mu.Unlock()
		return false
	}
	a.settleLocked(id, pa, d)
	a.mu.Unlock()
	a.deliver(pa, d)
	return true
}

// settleLocked removes a pending approval and records its outcome. Callers hold a.mu.
func (a *Approvals) settleLocked(id string, pa *PendingApproval, d Decision) {
	delete(a.pending, id)
	for i, existing := range a.order {
		if existing == id {
			a.order = append(a.order[:i], a.order[i+1:]...)
			break
		}
	}
	if pa.timer != nil {
		pa.timer.Stop()
	}
	a.rememberLocked(id, d)
}

// deliver hands the decision to the parked Confirm. Buffered(1) and written once, so it
// never blocks even if that caller has already left through the ctx branch.
func (a *Approvals) deliver(pa *PendingApproval, d Decision) {
	select {
	case pa.resolve <- d:
	default:
	}
}

// ApprovalRunMismatchError is a decision aimed at a turn the approval does not belong to.
//
// It is its own type rather than the session's RunMismatchError because the remedy
// differs: that one tells a caller to poll the run it named for an outcome, while here
// the caller is holding a decision about the wrong piece of work and needs to look at
// what this approval is actually blocking.
type ApprovalRunMismatchError struct {
	ApprovalID string
	Want       string
	Actual     string
}

func (e *ApprovalRunMismatchError) Error() string {
	if e.Actual == "" {
		return fmt.Sprintf(
			"approval %q does not record which run it blocks, so a decision naming run %q cannot be checked against it — "+
				"call daintree.approvals and answer without runId if it is still the one you meant",
			e.ApprovalID, e.Want)
	}
	return fmt.Sprintf(
		"approval %q blocks run %q, not the run %q you named — you are holding a decision about different work; "+
			"call daintree.approvals to see what this one is actually waiting on",
		e.ApprovalID, e.Actual, e.Want)
}

// ResolveForRun settles an approval only if it belongs to the run the caller believed it
// was deciding for. An empty expectRunID skips the correlation.
//
// Correlation and settlement are ONE operation, under one lock hold, because splitting
// them leaves a window: the approval that passed the check can settle and another be
// inserted before the resolve lands. Approval ids are eight hex characters, so treating
// a collision as impossible is an assumption, and a mutating call released on the
// strength of a judgement made about different work is the worst thing this surface can
// get wrong.
//
// A pending approval with NO recorded run fails the correlation rather than passing it.
// The caller asked for a check; answering "sure" when the provenance is simply missing
// is the fail-open answer, and the caller can drop the runId if it still means it.
func (a *Approvals) ResolveForRun(id, expectRunID string, d Decision) (settled bool, mismatch error) {
	a.mu.Lock()
	pa, ok := a.pending[id]
	if !ok {
		a.mu.Unlock()
		// Nothing pending. The caller's own not-found handling (already settled versus
		// never real) is more informative than a correlation error about a ghost.
		return false, nil
	}
	if expectRunID != "" && pa.RunID != expectRunID {
		actual := pa.RunID
		a.mu.Unlock()
		return false, &ApprovalRunMismatchError{ApprovalID: id, Want: expectRunID, Actual: actual}
	}
	a.settleLocked(id, pa, d)
	a.mu.Unlock()
	a.deliver(pa, d)
	return true, nil
}

// maxDecidedHistory bounds the outcome memory. It exists so a caller that polls after a
// timeout learns why, not so this becomes an audit log — the durable audit rows are that.
const maxDecidedHistory = 64

func (a *Approvals) rememberLocked(id string, d Decision) {
	if _, seen := a.decided[id]; !seen {
		a.decidedOrder = append(a.decidedOrder, id)
	}
	a.decided[id] = d
	for len(a.decidedOrder) > maxDecidedHistory {
		delete(a.decided, a.decidedOrder[0])
		a.decidedOrder = a.decidedOrder[1:]
	}
}

// Outcome reports a settled approval's decision.
func (a *Approvals) Outcome(id string) (Decision, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	d, ok := a.decided[id]
	return d, ok
}

// Pending lists outstanding approvals, oldest first.
func (a *Approvals) Pending() []PendingApproval {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]PendingApproval, 0, len(a.order))
	for _, id := range a.order {
		if pa, ok := a.pending[id]; ok {
			out = append(out, *pa)
		}
	}
	return out
}

// RejectRun settles the approvals raised by ONE run. Interrupt uses it rather than
// RejectAll: a session is single-flight, but the run it captured can finish and a new
// one start before the rejection lands, and cancelling the new turn's approvals would
// abort work the caller never asked to stop. An approval with no run id belongs to no
// particular turn and is left alone.
func (a *Approvals) RejectRun(runID string) {
	a.mu.Lock()
	var ids []string
	for _, id := range a.order {
		if pa, ok := a.pending[id]; ok && pa.RunID == runID {
			ids = append(ids, id)
		}
	}
	a.mu.Unlock()
	for _, id := range ids {
		a.Resolve(id, DecisionCancelled)
	}
}

// RejectAll settles every outstanding approval as cancelled. Called when a turn is
// interrupted and when a session closes: a parked dispatch that nobody can now answer
// would otherwise hold the turn open against teardown.
func (a *Approvals) RejectAll() {
	a.mu.Lock()
	ids := append([]string(nil), a.order...)
	a.mu.Unlock()
	for _, id := range ids {
		a.Resolve(id, DecisionCancelled)
	}
}
