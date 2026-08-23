package mcpserver

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// questions.go brokers the assistant's multiple-choice questions
// (user.askMultipleChoice) for a caller driving this server.
//
// It exists because the surface built to TEST the product could not exercise the product.
// The native host answers questions; MCP reported QUESTION_UNAVAILABLE, so a turn that
// needed a planning decision took a different path here than it takes in Daintree — and
// an end-to-end run that cannot reach the same branch is not testing the thing it claims
// to test.
//
// AN APPROVAL AND A QUESTION ARE NOT THE SAME DECISION, and the difference decides the
// defaults. An approval asks "may I do this?", which has one safe answer — no — so an
// unanswered one times out to REJECTED and the turn carries on having skipped the call.
// A question asks "which of these did you mean?", which has no safe answer at all:
// inventing one puts words in the caller's mouth and then acts on them. So an unanswered
// question times out to CANCELLED, an out-of-range index cancels rather than clamping,
// and there is no default answer anywhere in this file.

// QuestionMode is how a session answers a multiple-choice question.
type QuestionMode string

const (
	// QuestionDecline cancels every question immediately. The safe default and the only
	// sensible one for a caller that is not polling: the tool call fails as cancelled,
	// which the model can see and plan around, rather than blocking a turn nobody is
	// watching.
	QuestionDecline QuestionMode = "decline"
	// QuestionDelegate parks the question and hands it to the CALLER AGENT to answer.
	// Named for what it does, exactly as the approval mode is: nobody else sees it.
	QuestionDelegate QuestionMode = "delegate"
)

// Valid reports whether m is a known mode.
func (m QuestionMode) Valid() bool {
	return m == QuestionDecline || m == QuestionDelegate
}

// DefaultQuestionTimeout bounds an unanswered question. It matches the approval default:
// both park a dispatch, and the reason the bound exists — a forgotten decision must not
// pin a turn for the session's lifetime — is identical.
const DefaultQuestionTimeout = 5 * time.Minute

// MaxQuestionTimeout caps how long a question may park, for the same reason
// MaxApprovalTimeout does: the timeout is the ONLY thing that unblocks a dispatch nobody
// answers, so a caller must not be able to stretch it until it stops being a bound.
const MaxQuestionTimeout = time.Hour

// PendingQuestion is one parked question, as reported to a caller.
type PendingQuestion struct {
	ID string `json:"id"`
	// RunID ties the question to the turn that is blocked on it.
	RunID string `json:"runId,omitempty"`
	// ToolCallID is the call that asked, so a caller can correlate it with the run's
	// event timeline.
	ToolCallID string `json:"toolCallId,omitempty"`
	Question   string `json:"question"`
	// Options are the labelled choices IN ORDER. The index a caller answers with is an
	// index into this slice.
	Options     []QuestionOption `json:"options"`
	RequestedAt int64            `json:"requestedAt"`
	// DecisionAuthority says whose answer releases this call — "caller-agent" under
	// delegate. Never "human": the same model that asked is the one answering.
	DecisionAuthority string `json:"decisionAuthority"`

	resolve chan questionOutcome
	timer   *time.Timer
}

// QuestionOption is one labelled choice.
type QuestionOption struct {
	// Label is assigned by the runtime (A, B, C…), never by the model, so the model
	// cannot collide with or misspell them.
	Label string `json:"label"`
	Text  string `json:"text"`
}

// questionOutcome is an answer or a cancellation. There is no third state: a question
// either got an index the caller chose, or it did not get one.
type questionOutcome struct {
	index     int
	cancelled bool
}

// Questions brokers multiple-choice questions for one session.
type Questions struct {
	mode    QuestionMode
	timeout time.Duration
	// onChange is called with the affected run id whenever the pending set changes. A
	// run parked on a question emits no further events of its own, so without this a
	// long poll would sit through its whole budget without ever reporting that the turn
	// had STOPPED rather than merely being slow — the same wake the approval broker
	// needs, for the same reason.
	onChange func(runID string)

	mu      sync.Mutex
	pending map[string]*PendingQuestion
	order   []string
	// answered keeps the last outcomes so a caller that polls after the timer fired
	// learns WHY its question vanished rather than finding it simply gone.
	answered      map[string]string
	answeredOrder []string
}

// NewQuestions builds a broker. A zero timeout uses DefaultQuestionTimeout; a
// non-positive or over-long one is CLAMPED, never honoured — there is deliberately no way
// to disable the bound, from inside this package or from a tool argument.
func NewQuestions(mode QuestionMode, timeout time.Duration) *Questions {
	if !mode.Valid() {
		mode = QuestionDecline
	}
	switch {
	case timeout <= 0:
		timeout = DefaultQuestionTimeout
	case timeout > MaxQuestionTimeout:
		timeout = MaxQuestionTimeout
	}
	return &Questions{
		mode:     mode,
		timeout:  timeout,
		pending:  map[string]*PendingQuestion{},
		answered: map[string]string{},
	}
}

// Mode reports the configured mode.
func (q *Questions) Mode() QuestionMode { return q.mode }

// Timeout is how long an unanswered question parks before it is cancelled.
func (q *Questions) Timeout() time.Duration { return q.timeout }

// SetOnChange installs the pending-set change hook. Set once, at construction.
func (q *Questions) SetOnChange(fn func(runID string)) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.onChange = fn
}

// Ask is the ToolContext.AskChoice hook. It runs on the agent's dispatch goroutine and
// blocks there until the question settles, so every exit path must be bounded.
//
// A cancellation is reported as tools.ErrNoAskChoiceHook in decline mode — the same
// error a runtime with no question surface returns, which is what the handler already
// maps to QUESTION_UNAVAILABLE. Under delegate, a cancellation returns the context error
// so the model sees a cancelled call rather than an absent surface: the difference
// matters, because "nobody can answer this" and "someone declined to" lead a model to
// different next steps.
func (q *Questions) Ask(ctx context.Context, req tools.AskChoiceRequest, runID string) (tools.AskChoiceAnswer, error) {
	if q.mode != QuestionDelegate {
		// Wrapped, not bare. The handler maps ErrNoAskChoiceHook to QUESTION_UNAVAILABLE,
		// which is the right CODE — the model cannot get an answer either way — but
		// "there is no question surface" is not what happened. There is one; this session
		// was opened without it. Saying so lets a person reading the run fix it.
		return tools.AskChoiceAnswer{}, fmt.Errorf(
			"this session was opened with questions disabled, so the assistant cannot ask one "+
				"(open it with questions:\"delegate\" to answer them yourself): %w", tools.ErrNoAskChoiceHook)
	}
	if len(req.Options) == 0 {
		// Nothing to choose between. Refused rather than parked: a caller cannot answer
		// a question with no options, so parking it would burn the whole timeout to
		// reach the same place.
		return tools.AskChoiceAnswer{}, fmt.Errorf("a multiple-choice question needs at least one option")
	}

	pq := &PendingQuestion{
		ID:                domain.NewID("qst_"),
		RunID:             runID,
		ToolCallID:        req.ToolCallID,
		Question:          req.Question,
		Options:           make([]QuestionOption, 0, len(req.Options)),
		RequestedAt:       domain.NowMS(),
		DecisionAuthority: "caller-agent",
		resolve:           make(chan questionOutcome, 1),
	}
	for _, opt := range req.Options {
		pq.Options = append(pq.Options, QuestionOption{Label: opt.Label, Text: opt.Text})
	}

	q.mu.Lock()
	// Always armed — see NewQuestions for why there is no unbounded case. Started inside
	// the lock so the callback (which takes the same lock) cannot fire before the map
	// insert below; AfterFunc returns immediately, so this cannot self-deadlock.
	pq.timer = time.AfterFunc(q.timeout, func() { q.cancel(pq.ID) })
	q.pending[pq.ID] = pq
	q.order = append(q.order, pq.ID)
	onChange := q.onChange
	q.mu.Unlock()

	if onChange != nil {
		onChange(runID)
	}

	select {
	case out := <-pq.resolve:
		// Cancellation DOMINATES, exactly as it does for an approval: when an answer and
		// a cancellation are both ready on this select, Go picks arbitrarily, so an
		// answer could otherwise be applied after interrupt or close had already stopped
		// the turn.
		if out.cancelled || ctx.Err() != nil {
			return tools.AskChoiceAnswer{}, context.Canceled
		}
		opt := pq.Options[out.index]
		return tools.AskChoiceAnswer{Label: opt.Label, Index: out.index, Text: opt.Text}, nil
	case <-ctx.Done():
		// The turn was cancelled or the session is closing. Unpark as a cancellation so
		// the dispatch returns rather than holding the goroutine — and so teardown,
		// which waits for the turn, is not blocked on a question nobody will answer.
		q.cancel(pq.ID)
		return tools.AskChoiceAnswer{}, ctx.Err()
	}
}

// QuestionRunMismatchError is an answer aimed at a turn the question does not belong to.
type QuestionRunMismatchError struct {
	QuestionID string
	Want       string
	Actual     string
}

func (e *QuestionRunMismatchError) Error() string {
	if e.Actual == "" {
		return fmt.Sprintf(
			"question %q does not record which run it blocks, so an answer naming run %q cannot be checked "+
				"against it — call daintree.questions and answer without runId if it is still the one you meant",
			e.QuestionID, e.Want)
	}
	return fmt.Sprintf(
		"question %q blocks run %q, not the run %q you named — you are holding an answer about different work; "+
			"call daintree.questions to see what this one is actually asking",
		e.QuestionID, e.Actual, e.Want)
}

// Answer settles a parked question with the caller's chosen index. See AnswerForRun.
func (q *Questions) Answer(id string, index int) (bool, error) {
	return q.AnswerForRun(id, "", index)
}

// AnswerForRun settles a parked question, optionally checking it belongs to the run the
// caller believed it was answering.
//
// Correlation and settlement are ONE operation under one lock hold, for the reason the
// approval broker learned: splitting them leaves a window in which the checked question
// settles and another is inserted before the answer lands, and eight-hex ids make
// "cannot collide" an assumption rather than a fact. A pending question with NO recorded
// run FAILS the correlation rather than passing it — the caller asked for a check, and
// answering "sure" when the provenance is missing is the fail-open answer.
//
// An out-of-range index CANCELS rather than clamping, and that is the whole asymmetry
// with approvals: clamping would answer a question the caller did not answer, and then
// the turn would act on it. "I could not choose" is a real outcome; "I chose the nearest
// valid option to what you typed" is not.
func (q *Questions) AnswerForRun(id, expectRunID string, index int) (bool, error) {
	q.mu.Lock()
	pq, ok := q.pending[id]
	if !ok {
		q.mu.Unlock()
		return false, nil
	}
	if expectRunID != "" && pq.RunID != expectRunID {
		actual := pq.RunID
		q.mu.Unlock()
		return false, &QuestionRunMismatchError{QuestionID: id, Want: expectRunID, Actual: actual}
	}
	if index < 0 || index >= len(pq.Options) {
		q.settleLocked(id, pq, "cancelled")
		q.mu.Unlock()
		q.deliver(pq, questionOutcome{cancelled: true})
		q.notify(pq.RunID)
		return true, fmt.Errorf(
			"choice %d is out of range for a question with %d options, so it was CANCELLED rather than "+
				"guessed at — the tool call fails and the turn continues. Ask again if you meant one of them",
			index, len(pq.Options))
	}
	q.settleLocked(id, pq, fmt.Sprintf("answered %d", index))
	q.mu.Unlock()
	q.deliver(pq, questionOutcome{index: index})
	q.notify(pq.RunID)
	return true, nil
}

// cancel settles a question with no answer.
func (q *Questions) cancel(id string) bool {
	q.mu.Lock()
	pq, ok := q.pending[id]
	if !ok {
		q.mu.Unlock()
		return false
	}
	q.settleLocked(id, pq, "cancelled")
	q.mu.Unlock()
	q.deliver(pq, questionOutcome{cancelled: true})
	q.notify(pq.RunID)
	return true
}

// CancelRun cancels every question raised by one run. Scoped to the run, not the session:
// a captured run can finish and a new one start before an interrupt lands, and cancelling
// the new turn's questions would abort work nobody asked to stop.
func (q *Questions) CancelRun(runID string) {
	q.mu.Lock()
	var ids []string
	for _, id := range q.order {
		if pq, ok := q.pending[id]; ok && pq.RunID == runID {
			ids = append(ids, id)
		}
	}
	q.mu.Unlock()
	for _, id := range ids {
		q.cancel(id)
	}
}

// CancelAll cancels every outstanding question. Teardown calls it BEFORE waiting on the
// turn goroutine — waiting first would block on a dispatch parked on a question nobody is
// left to answer.
func (q *Questions) CancelAll() {
	q.mu.Lock()
	ids := append([]string(nil), q.order...)
	q.mu.Unlock()
	for _, id := range ids {
		q.cancel(id)
	}
}

// Pending lists the parked questions, oldest first.
func (q *Questions) Pending() []PendingQuestion {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]PendingQuestion, 0, len(q.order))
	for _, id := range q.order {
		if pq, ok := q.pending[id]; ok {
			// Copy only the REPORTABLE fields: the resolve channel and the timer are
			// this broker's business, and handing them out would let a caller settle a
			// question behind Answer's back, skipping the bookkeeping entirely.
			out = append(out, PendingQuestion{
				ID: pq.ID, RunID: pq.RunID, ToolCallID: pq.ToolCallID,
				Question: pq.Question,
				// DEEP-copied. A shallow slice copy leaves the backing array shared with
				// the retained question, so a caller could mutate an option while Ask
				// reads it after settlement — a race, and a way to change the answer
				// that comes back.
				Options:           append([]QuestionOption(nil), pq.Options...),
				RequestedAt:       pq.RequestedAt,
				DecisionAuthority: pq.DecisionAuthority,
			})
		}
	}
	return out
}

// Outcome reports how a settled question ended, for a caller that polled too late.
func (q *Questions) Outcome(id string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	out, ok := q.answered[id]
	return out, ok
}

// RunFor reports which run an outstanding question belongs to.
func (q *Questions) RunFor(id string) (string, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	pq, ok := q.pending[id]
	if !ok {
		return "", false
	}
	return pq.RunID, true
}

// settleLocked removes a pending question and records its outcome. Callers hold q.mu.
func (q *Questions) settleLocked(id string, pq *PendingQuestion, outcome string) {
	delete(q.pending, id)
	for i, existing := range q.order {
		if existing == id {
			q.order = append(q.order[:i], q.order[i+1:]...)
			break
		}
	}
	if pq.timer != nil {
		pq.timer.Stop()
	}
	if _, seen := q.answered[id]; !seen {
		q.answeredOrder = append(q.answeredOrder, id)
	}
	q.answered[id] = outcome
	for len(q.answeredOrder) > maxAnsweredHistory {
		delete(q.answered, q.answeredOrder[0])
		q.answeredOrder = q.answeredOrder[1:]
	}
}

// deliver hands the outcome to the parked Ask. Buffered(1) and written once, so it never
// blocks even if that caller has already left through the ctx branch.
func (q *Questions) deliver(pq *PendingQuestion, out questionOutcome) {
	select {
	case pq.resolve <- out:
	default:
	}
}

// notify wakes a long poll on the run this question was blocking.
func (q *Questions) notify(runID string) {
	q.mu.Lock()
	fn := q.onChange
	q.mu.Unlock()
	if fn != nil && runID != "" {
		fn(runID)
	}
}

// maxAnsweredHistory bounds the outcome memory. It exists so a caller that polls after a
// timeout learns why, not so this becomes an audit log.
const maxAnsweredHistory = 64
