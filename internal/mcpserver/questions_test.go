package mcpserver

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/tools"
)

func choiceRequest() tools.AskChoiceRequest {
	return tools.AskChoiceRequest{
		ToolCallID: "call_1",
		Question:   "Which worktree should the agent work in?",
		Options: []tools.ChoiceOption{
			{Label: "A", Text: "feature/login"},
			{Label: "B", Text: "feature/billing"},
		},
	}
}

func awaitOneQuestion(t *testing.T, q *Questions) PendingQuestion {
	t.Helper()
	for i := 0; i < 200; i++ {
		if p := q.Pending(); len(p) == 1 {
			return p[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no question parked")
	return PendingQuestion{}
}

// Decline is the default and refuses without parking. It reports ErrNoAskChoiceHook —
// the same error a runtime with no question surface returns — so the handler maps it to
// QUESTION_UNAVAILABLE and the model plans around the absence rather than waiting.
func TestQuestionDeclineRefusesWithoutParking(t *testing.T) {
	q := NewQuestions(QuestionDecline, time.Minute)
	_, err := q.Ask(context.Background(), choiceRequest(), "mrun_1")
	if !errors.Is(err, tools.ErrNoAskChoiceHook) {
		t.Fatalf("decline returned %v, want ErrNoAskChoiceHook", err)
	}
	if len(q.Pending()) != 0 {
		t.Error("a declining broker parked the question anyway")
	}
}

// The delegate path: the question parks, the caller picks an index, the tool call gets
// that option back.
func TestQuestionDelegateParksThenReturnsTheChosenOption(t *testing.T) {
	q := NewQuestions(QuestionDelegate, time.Minute)
	type result struct {
		ans tools.AskChoiceAnswer
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := q.Ask(context.Background(), choiceRequest(), "mrun_1")
		done <- result{a, err}
	}()

	pending := awaitOneQuestion(t, q)
	if pending.RunID != "mrun_1" || pending.ToolCallID != "call_1" {
		t.Errorf("the question does not name the turn it blocks: %+v", pending)
	}
	if len(pending.Options) != 2 || pending.Options[1].Text != "feature/billing" {
		t.Fatalf("options did not survive: %+v", pending.Options)
	}
	if pending.DecisionAuthority != "caller-agent" {
		t.Errorf("decisionAuthority = %q; nobody but the caller sees this", pending.DecisionAuthority)
	}

	if settled, err := q.Answer(pending.ID, 1); !settled || err != nil {
		t.Fatalf("Answer: settled=%v err=%v", settled, err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("the dispatch got an error: %v", got.err)
	}
	if got.ans.Index != 1 || got.ans.Text != "feature/billing" || got.ans.Label != "B" {
		t.Errorf("answer = %+v, want option B at index 1", got.ans)
	}
}

// THE ASYMMETRY WITH APPROVALS. An unanswered approval times out to REJECTED, because
// "may I?" has a safe answer. A question has none, so an out-of-range index CANCELS
// rather than clamping — an invented answer the turn then acts on is worse than no answer.
func TestAnOutOfRangeChoiceCancelsRatherThanClamping(t *testing.T) {
	for _, choice := range []int{-1, 2, 99} {
		q := NewQuestions(QuestionDelegate, time.Minute)
		done := make(chan error, 1)
		go func() {
			_, err := q.Ask(context.Background(), choiceRequest(), "mrun_1")
			done <- err
		}()
		pending := awaitOneQuestion(t, q)

		settled, err := q.Answer(pending.ID, choice)
		if !settled {
			t.Fatalf("choice %d did not settle the question", choice)
		}
		if err == nil {
			t.Fatalf("choice %d was accepted for a 2-option question", choice)
		}
		if !strings.Contains(err.Error(), "CANCELLED") {
			t.Errorf("the error does not say the call was cancelled: %v", err)
		}
		if derr := <-done; derr == nil {
			t.Errorf("choice %d let the tool call succeed with a guessed answer", choice)
		}
		if got, ok := q.Outcome(pending.ID); !ok || got != "cancelled" {
			t.Errorf("outcome = %q, want cancelled", got)
		}
	}
}

// An unanswered question is CANCELLED on the timer, not defaulted. Defaulting would put
// words in the caller's mouth and then act on them.
func TestAnUnansweredQuestionCancelsOnTheTimer(t *testing.T) {
	q := NewQuestions(QuestionDelegate, 120*time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := q.Ask(context.Background(), choiceRequest(), "mrun_1")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unanswered question produced an answer; there is no safe default for one")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered question parked the dispatch past its timeout")
	}
	if len(q.Pending()) != 0 {
		t.Error("the timed-out question is still listed as pending")
	}
}

// The timeout is the ONLY thing that unparks a dispatch nobody answers, so it cannot be
// disabled or stretched until it stops being a bound.
func TestQuestionTimeoutIsAlwaysBounded(t *testing.T) {
	for _, tc := range []struct {
		given time.Duration
		want  time.Duration
	}{
		{0, DefaultQuestionTimeout},
		{-time.Hour, DefaultQuestionTimeout},
		{30 * time.Second, 30 * time.Second},
		{100 * time.Hour, MaxQuestionTimeout},
	} {
		if got := NewQuestions(QuestionDelegate, tc.given).Timeout(); got != tc.want {
			t.Errorf("NewQuestions(%v).Timeout() = %v, want %v", tc.given, got, tc.want)
		}
	}
}

// Cancellation DOMINATES: when an answer and a cancellation are both ready, an answer
// must not be applied to a turn that has already been stopped.
func TestCancellationBeatsALateAnswer(t *testing.T) {
	q := NewQuestions(QuestionDelegate, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := q.Ask(ctx, choiceRequest(), "mrun_1")
		done <- err
	}()
	pending := awaitOneQuestion(t, q)

	cancel()
	if err := <-done; err == nil {
		t.Fatal("a cancelled turn still got an answer")
	}
	// The question is gone, so a late answer finds nothing rather than releasing a call
	// on a turn that has stopped.
	if settled, _ := q.Answer(pending.ID, 0); settled {
		t.Error("an answer landed on a question whose turn was already cancelled")
	}
}

// Teardown must unpark every question BEFORE waiting on the turn goroutine — waiting
// first would block on a dispatch parked on a question nobody is left to answer.
func TestCancelAllUnparksEveryQuestion(t *testing.T) {
	q := NewQuestions(QuestionDelegate, time.Minute)
	errs := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func() {
			_, err := q.Ask(context.Background(), choiceRequest(), "mrun_1")
			errs <- err
		}()
	}
	for i := 0; i < 200 && len(q.Pending()) < 3; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if len(q.Pending()) != 3 {
		t.Fatalf("expected 3 parked questions, got %d", len(q.Pending()))
	}

	q.CancelAll()
	for i := 0; i < 3; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Error("teardown answered a question instead of cancelling it")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a dispatch stayed parked after CancelAll")
		}
	}
}

// Interrupt is RUN-SCOPED. The captured run can finish and a successor start before the
// cancel lands, and cancelling the new turn's questions would abort work nobody asked to
// stop — the same reasoning that scopes the approval rejection.
func TestCancelRunLeavesAnotherRunsQuestionsAlone(t *testing.T) {
	q := NewQuestions(QuestionDelegate, time.Minute)
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { _, err := q.Ask(context.Background(), choiceRequest(), "mrun_old"); first <- err }()
	go func() { _, err := q.Ask(context.Background(), choiceRequest(), "mrun_new"); second <- err }()
	for i := 0; i < 200 && len(q.Pending()) < 2; i++ {
		time.Sleep(5 * time.Millisecond)
	}

	q.CancelRun("mrun_old")
	select {
	case err := <-first:
		if err == nil {
			t.Error("the interrupted run's question was answered rather than cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("CancelRun did not unpark its own run's question")
	}
	select {
	case <-second:
		t.Error("cancelling one run took down another run's question")
	case <-time.After(150 * time.Millisecond):
	}
	q.CancelAll()
	<-second
}

// A parked question STOPS the turn, so a long poll must wake on it — the revision counter
// alone cannot, because a question that parked between two polls signals nothing further.
func TestAParkedQuestionCountsAsABlockedRun(t *testing.T) {
	run := NewRun("mrun_1", "ses", "p", func() {})
	q := NewQuestions(QuestionDelegate, time.Minute)

	if hasPendingDecision(run, nil, q) {
		t.Fatal("an idle run reported itself blocked")
	}
	go func() { _, _ = q.Ask(context.Background(), choiceRequest(), run.ID) }()
	awaitOneQuestion(t, q)

	if !hasPendingDecision(run, nil, q) {
		t.Error("a run parked on a question is not reported as blocked, so a long poll sleeps through it")
	}
	other := NewRun("mrun_other", "ses", "p", func() {})
	if hasPendingDecision(other, nil, q) {
		t.Error("one run's question marked a different run blocked")
	}
	q.CancelAll()
}

// A question with no options cannot be answered, so parking it would burn the whole
// timeout to reach the same place.
func TestAQuestionWithNoOptionsIsRefusedImmediately(t *testing.T) {
	q := NewQuestions(QuestionDelegate, time.Minute)
	_, err := q.Ask(context.Background(), tools.AskChoiceRequest{Question: "which?"}, "mrun_1")
	if err == nil {
		t.Fatal("a question with no options was accepted")
	}
	if len(q.Pending()) != 0 {
		t.Error("an unanswerable question was parked")
	}
}

// THE ACCEPTANCE TEST for this whole phase, driven through the real MCP tools rather than
// the broker in isolation.
//
// The broker tests above prove the rules; this proves the wiring. Without it, a change
// that stopped registering the answer tool, or dropped AskChoice from the hooks, or lost
// the run id, would leave every other test in this file green while the agent-driven flow
// the feature exists for stayed broken.
func TestAQuestionCanBeAnsweredThroughTheToolSurface(t *testing.T) {
	fake := newFakeRuntime("ses_e2e")
	fake.questions = NewQuestions(QuestionDelegate, 30*time.Second)
	fake.askInSend = &tools.AskChoiceRequest{
		ToolCallID: "call_1",
		Question:   "Which worktree should the agent work in?",
		Options: []tools.ChoiceOption{
			{Label: "A", Text: "feature/login"},
			{Label: "B", Text: "feature/billing"},
		},
	}
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "pick one"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}

	// The question shows up in the listing, tied to the run that is blocked on it.
	var listed QuestionsOutput
	var pending PendingQuestion
	for i := 0; i < 200; i++ {
		if err := call(t, cs, "daintree.questions", SessionRefInput{SessionID: sess.SessionID}, &listed); err != nil {
			t.Fatalf("questions: %v", err)
		}
		if listed.Count == 1 {
			pending = listed.Pending[0]
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if pending.ID == "" {
		t.Fatal("the question never reached daintree.questions; the surface is not wired")
	}
	if pending.RunID != run.RunID {
		t.Errorf("the question names run %q, want %q", pending.RunID, run.RunID)
	}
	if listed.DecisionAuthority != "caller-agent" {
		t.Errorf("decisionAuthority = %q; nobody but the caller sees this", listed.DecisionAuthority)
	}

	// A poll reports the run as BLOCKED on it, not merely slow.
	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(polled.PendingQuestions) != 1 {
		t.Errorf("the run does not report its question, so a caller polls harder at a turn that has stopped")
	}
	if !strings.Contains(polled.NextAction, "daintree.question.answer") {
		t.Errorf("nextAction does not name the tool that unblocks it: %q", polled.NextAction)
	}

	// An answer aimed at the WRONG run is refused rather than applied.
	var acted ActedOutput
	if err := call(t, cs, "daintree.question.answer", AnswerQuestionInput{
		SessionID: sess.SessionID, QuestionID: pending.ID, Choice: 1, RunID: "mrun_someone_else",
	}, &acted); err == nil {
		t.Error("an answer naming a different run was accepted")
	}

	// The real answer reaches the blocked dispatch, with the option the caller chose.
	if err := call(t, cs, "daintree.question.answer", AnswerQuestionInput{
		SessionID: sess.SessionID, QuestionID: pending.ID, Choice: 1, RunID: run.RunID,
	}, &acted); err != nil {
		t.Fatalf("question.answer: %v", err)
	}
	if !acted.Acted {
		t.Error("the answer reported acted:false")
	}

	select {
	case got := <-fake.askResult:
		if got.err != nil {
			t.Fatalf("the dispatch got an error: %v", got.err)
		}
		if got.ans.Index != 1 || got.ans.Text != "feature/billing" {
			t.Errorf("the dispatch received %+v, want option B at index 1", got.ans)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the answer never reached the blocked tool call")
	}

	fake.letFinish()
}

// A session opened with questions declined must refuse them, so an existing caller that
// does not answer questions keeps its previous immediate failure rather than parking for
// five minutes on something nobody will answer.
func TestASessionWithQuestionsDeclinedRefusesImmediately(t *testing.T) {
	fake := newFakeRuntime("ses_declined")
	fake.questions = NewQuestions(QuestionDecline, 30*time.Second)
	fake.askInSend = &tools.AskChoiceRequest{
		Question: "which?",
		Options:  []tools.ChoiceOption{{Label: "A", Text: "one"}, {Label: "B", Text: "two"}},
	}
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "pick one"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	select {
	case got := <-fake.askResult:
		if got.err == nil {
			t.Fatal("a declining session answered the question")
		}
		if !errors.Is(got.err, tools.ErrNoAskChoiceHook) {
			t.Errorf("error = %v, want ErrNoAskChoiceHook so the handler reports QUESTION_UNAVAILABLE", got.err)
		}
		if !strings.Contains(got.err.Error(), "questions disabled") {
			t.Errorf("the error does not say the session was opened without them: %v", got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a declining session PARKED the question instead of refusing it")
	}

	var listed QuestionsOutput
	if err := call(t, cs, "daintree.questions", SessionRefInput{SessionID: sess.SessionID}, &listed); err != nil {
		t.Fatalf("questions: %v", err)
	}
	if listed.Count != 0 {
		t.Error("a declining session parked a question")
	}
	if listed.DecisionAuthority != "none" {
		t.Errorf("decisionAuthority = %q, want none", listed.DecisionAuthority)
	}
	fake.letFinish()
}

// Questions are gated INDEPENDENTLY of approvals — that is the whole point of separating
// them. A harness that wants planning questions while keeping mutations declined must be
// able to have exactly that.
func TestQuestionsAreGatedIndependentlyOfApprovals(t *testing.T) {
	// Wanting questions without approvals is a valid combination.
	p := ServerPolicy{AllowDelegatedQuestions: true}
	if err := p.Check(OpenParams{Questions: QuestionDelegate, Approvals: ApprovalDecline}, 0); err != nil {
		t.Fatalf("questions-without-approvals was refused: %v", err)
	}
	// And allowing approvals does not silently allow questions.
	approvalsOnly := ServerPolicy{AllowDelegatedApprovals: true}
	if err := approvalsOnly.Check(OpenParams{Questions: QuestionDelegate}, 0); err == nil {
		t.Error("AllowDelegatedApprovals silently enabled question delegation")
	}
	// Nor the reverse.
	questionsOnly := ServerPolicy{AllowDelegatedQuestions: true}
	if err := questionsOnly.Check(OpenParams{Approvals: ApprovalDelegate}, 0); err == nil {
		t.Error("AllowDelegatedQuestions silently enabled approval delegation")
	}
}

// The timeouts are separate fields because approvalTimeoutMs is documented as meaningful
// only for approvals — borrowing it made a one-second approval timeout silently cancel
// every question.
func TestQuestionTimeoutIsItsOwnSetting(t *testing.T) {
	if d, err := resolveQuestionTimeout(0); err != nil || d != 0 {
		t.Errorf("an omitted timeout gave (%v, %v), want the broker's default", d, err)
	}
	if _, err := resolveQuestionTimeout(-1); err == nil {
		t.Error("a negative timeout was accepted")
	}
	if _, err := resolveQuestionTimeout(1 << 53); err == nil {
		t.Error("an overflowing timeout was accepted rather than refused")
	}
	if d, err := resolveQuestionTimeout(30_000); err != nil || d != 30*time.Second {
		t.Errorf("a valid timeout gave (%v, %v)", d, err)
	}
}

// A truncated list must say so: a caller told to answer everything shown, while another
// blocker waits unmentioned, cannot make the turn move.
func TestQuestionListingReportsWhatItWithheld(t *testing.T) {
	many := make([]PendingQuestion, MaxPendingApprovals+7)
	for i := range many {
		many[i] = PendingQuestion{ID: "qst_x", Question: "which?", Options: []QuestionOption{{Label: "A", Text: "one"}}}
	}
	out, remaining := boundedQuestions(many, MaxPendingApprovals)
	if len(out) != MaxPendingApprovals || remaining != 7 {
		t.Errorf("boundedQuestions returned %d with %d remaining, want %d and 7",
			len(out), remaining, MaxPendingApprovals)
	}
	// And the option text is bounded like every other externally-sourced string.
	huge := PendingQuestion{Question: strings.Repeat("q", MaxEventTextBytes*3), Options: []QuestionOption{
		{Label: "A", Text: strings.Repeat("o", maxQuestionOptionBytes*3)},
	}}
	got := boundedQuestion(huge)
	if len(got.Question) > MaxEventTextBytes {
		t.Errorf("question text survived at %d bytes", len(got.Question))
	}
	if len(got.Options[0].Text) > maxQuestionOptionBytes {
		t.Errorf("option text survived at %d bytes", len(got.Options[0].Text))
	}
	// The bound must not edit the broker's own retained copy.
	if huge.Options[0].Text == got.Options[0].Text {
		t.Error("boundedQuestion truncated the caller's slice in place")
	}
}
