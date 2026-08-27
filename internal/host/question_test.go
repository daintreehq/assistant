package host

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// collectPosts captures the frames a bridge emits, so a test can assert on the wire
// shape rather than on internal state.
//
// It is mutex-guarded because the bridge posts from the DISPATCH goroutine (inside
// AskChoice) while the test polls from its own — which is exactly the concurrency the
// question channel exists to handle, so an unguarded collector would be a race in the
// harness rather than in the thing under test. The returned reader hands back a copy so a
// caller can range over it while more frames arrive.
func collectPosts() (PostFunc, func() []map[string]any) {
	var (
		mu     sync.Mutex
		frames []map[string]any
	)
	post := func(ev HostEvent) {
		mu.Lock()
		defer mu.Unlock()
		raw, err := encodeSeq(ev, "ses_test", uint64(len(frames)+1))
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			frames = append(frames, m)
		}
	}
	return post, func() []map[string]any {
		mu.Lock()
		defer mu.Unlock()
		out := make([]map[string]any, len(frames))
		copy(out, frames)
		return out
	}
}

func newQuestionBridge() (*Bridge, func() []map[string]any) {
	post, frames := collectPosts()
	return NewBridge(BridgeOptions{SessionID: "ses_test", Post: post}), frames
}

func threeOptions() AskChoiceRequest {
	return AskChoiceRequest{
		ToolCallID: "call_1",
		Question:   "Which worktree did you mean?",
		Options: []AskChoiceOption{
			{Label: "A", Text: "feature/one"},
			{Label: "B", Text: "feature/two"},
			{Label: "C", Text: "main"},
		},
		Default: 0,
	}
}

// The whole point of the channel: the HOST — the surface a user actually runs — can be
// asked a question. Before this existed, user.askMultipleChoice returned
// QUESTION_UNAVAILABLE here and the model had to guess or give up in prose, while the
// developer-only line REPL could ask freely.
func TestAskChoiceEmitsQuestionAndUnblocksOnAnswer(t *testing.T) {
	b, frames := newQuestionBridge()

	type result struct {
		ans AskChoiceAnswer
		err error
	}
	done := make(chan result, 1)
	go func() {
		ans, err := b.AskChoice(context.Background(), threeOptions())
		done <- result{ans, err}
	}()

	qid := waitForQuestionID(t, frames)
	b.ResolveQuestion(qid, 1)

	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("AskChoice returned %v, want the selected option", got.err)
		}
		if got.ans.Index != 1 || got.ans.Label != "B" || got.ans.Text != "feature/two" {
			t.Fatalf("answer = %+v, want index 1 / B / feature/two", got.ans)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AskChoice never returned; the dispatch would be parked forever")
	}

	// The answered frame must be reconcilable against the host's own sheet.
	last := frames()[len(frames())-1]
	if last["type"] != "question:answered" {
		t.Fatalf("last frame = %v, want question:answered", last["type"])
	}
	if last["cancelled"] != false {
		t.Errorf("cancelled = %v, want false", last["cancelled"])
	}
	if last["label"] != "B" {
		t.Errorf("label = %v, want B", last["label"])
	}
}

// A dismissed sheet is a CANCELLATION, not a selection. Answering "the first option" for
// a user who never chose is the one wrong outcome a decision channel must never invent.
func TestAskChoiceTreatsDismissalAsCancellation(t *testing.T) {
	b, frames := newQuestionBridge()

	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		done <- err
	}()

	qid := waitForQuestionID(t, frames)
	b.ResolveQuestion(qid, -1)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a dismissed question must not resolve to an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AskChoice never returned after dismissal")
	}
	last := frames()[len(frames())-1]
	if last["cancelled"] != true {
		t.Errorf("cancelled = %v, want true", last["cancelled"])
	}
	if last["choiceIndex"] != float64(-1) {
		t.Errorf("choiceIndex = %v, want -1", last["choiceIndex"])
	}
}

// An out-of-range index must cancel rather than clamp: clamping answers with an option
// the user never selected.
func TestAskChoiceRejectsOutOfRangeIndex(t *testing.T) {
	b, frames := newQuestionBridge()

	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		done <- err
	}()

	qid := waitForQuestionID(t, frames)
	b.ResolveQuestion(qid, 99)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an out-of-range choice must not be clamped into a real answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AskChoice never returned")
	}
}

// Teardown and interrupt drain questions for the same reason they drain approvals: a
// parked dispatch nobody will answer strands the turn as busy forever.
func TestSettlePendingQuestionsUnparksEveryDispatch(t *testing.T) {
	b, frames := newQuestionBridge()

	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		done <- err
	}()
	waitForQuestionID(t, frames)

	b.SettlePendingQuestions()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a drained question must not resolve to an answer")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SettlePendingQuestions left a dispatch parked")
	}
}

// Cancelling the turn must free the dispatch too — otherwise an interrupt would leave the
// tool goroutine blocked on a sheet the user has already navigated away from.
func TestAskChoiceHonoursTurnCancellation(t *testing.T) {
	b, frames := newQuestionBridge()
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(ctx, threeOptions())
		done <- err
	}()
	waitForQuestionID(t, frames)

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a cancelled turn left the dispatch parked on a question")
	}
}

// A stale or duplicate answer — a host that retried — must be a harmless no-op, not a
// panic or a second decision.
func TestResolveQuestionIsIdempotent(t *testing.T) {
	b, frames := newQuestionBridge()

	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		done <- err
	}()
	qid := waitForQuestionID(t, frames)

	b.ResolveQuestion(qid, 0)
	<-done
	before := len(frames())
	b.ResolveQuestion(qid, 2)
	b.ResolveQuestion("qst_never_existed", 1)
	if got := len(frames()); got != before {
		t.Errorf("a repeated answer emitted %d extra frame(s); it must be a no-op", got-before)
	}
}

// waitForQuestionID blocks until the question:requested frame appears and returns its id.
func waitForQuestionID(t *testing.T, frames func() []map[string]any) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range frames() {
			if f["type"] == "question:requested" {
				id, _ := f["questionId"].(string)
				if id == "" {
					t.Fatal("question:requested carried no questionId")
				}
				// The options must reach the host: a sheet cannot be drawn without them.
				opts, _ := f["options"].([]any)
				if len(opts) != 3 {
					t.Fatalf("options = %v, want the three offered choices", f["options"])
				}
				return id
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no question:requested frame was emitted")
	return ""
}

// A dismissal and an abandonment are both "no answer" and they mean opposite things to
// the model: one is a decision not to decide (stop asking), the other is the turn going
// away underneath the sheet (the question was never reached). Before this the two were
// the same `context.Canceled`, so QUESTION_DISMISSED existed as a documented outcome
// that nothing could ever produce, and a user who closed a sheet was reported to the
// model as having cancelled the turn.
func TestAskChoiceSeparatesDismissalFromAbandonment(t *testing.T) {
	t.Run("the host answering with a negative index is a dismissal", func(t *testing.T) {
		b, frames := newQuestionBridge()
		done := make(chan error, 1)
		go func() {
			_, err := b.AskChoice(context.Background(), threeOptions())
			done <- err
		}()

		b.ResolveQuestion(waitForQuestionID(t, frames), -1)

		select {
		case err := <-done:
			if !errors.Is(err, ErrQuestionDismissed) {
				t.Fatalf("err = %v, want ErrQuestionDismissed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("AskChoice never returned after dismissal")
		}
	})

	t.Run("a drained question is an abandonment, not a dismissal", func(t *testing.T) {
		// SettlePendingQuestions runs on interrupt and teardown. Nobody declined
		// anything there — the turn is being taken away — so reporting a dismissal
		// would tell the model the user made a decision they were never shown.
		b, frames := newQuestionBridge()
		done := make(chan error, 1)
		go func() {
			_, err := b.AskChoice(context.Background(), threeOptions())
			done <- err
		}()

		waitForQuestionID(t, frames)
		b.SettlePendingQuestions()

		select {
		case err := <-done:
			if errors.Is(err, ErrQuestionDismissed) {
				t.Fatal("a drained question was reported as a dismissal")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want a cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("AskChoice never returned after the drain")
		}
	})

	t.Run("an out-of-range index is an abandonment, not a dismissal", func(t *testing.T) {
		// A non-negative index past the end is a HOST bug, not a user declining. It is
		// already refused rather than clamped; this pins that it is not quietly
		// promoted into a decision the user is then told to have made.
		b, frames := newQuestionBridge()
		done := make(chan error, 1)
		go func() {
			_, err := b.AskChoice(context.Background(), threeOptions())
			done <- err
		}()

		b.ResolveQuestion(waitForQuestionID(t, frames), 99)

		select {
		case err := <-done:
			if errors.Is(err, ErrQuestionDismissed) {
				t.Fatal("an out-of-range index was reported as a dismissal")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want a cancellation", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("AskChoice never returned")
		}
	})
}

// waitForQuestionIDAfter is waitForQuestionID for a SECOND question: it skips the id
// already seen, which the shared helper would return forever because it scans from the
// front of the frame log.
func waitForQuestionIDAfter(t *testing.T, frames func() []map[string]any, seen string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, f := range frames() {
			if f["type"] != "question:requested" {
				continue
			}
			if id, _ := f["questionId"].(string); id != "" && id != seen {
				return id
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no second question:requested frame was emitted")
	return ""
}

// ONE sheet, so one outstanding question. The host draws a single sheet above a single
// composer, so a second request would REPLACE the first rather than joining it — and the
// first asker would sit parked on a question nobody can see until its timeout. Reachable
// since `/backend` began asking on its own account beside a running turn.
func TestAskChoiceRefusesASecondOutstandingQuestion(t *testing.T) {
	b, frames := newQuestionBridge()

	first := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		first <- err
	}()
	qid := waitForQuestionID(t, frames)

	if _, err := b.AskChoice(context.Background(), threeOptions()); !errors.Is(err, ErrQuestionBusy) {
		t.Fatalf("the second question returned %v, want ErrQuestionBusy", err)
	}
	// Refused WITHOUT disturbing the first: it is still answerable, and still the one on
	// screen. A refusal that settled the live sheet would be the bug with extra steps.
	b.ResolveQuestion(qid, 1)
	select {
	case err := <-first:
		if err != nil {
			t.Fatalf("the first question was disturbed by the refusal: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first question never returned")
	}

	// …and the sheet frees up again once it settles.
	done := make(chan error, 1)
	go func() {
		_, err := b.AskChoice(context.Background(), threeOptions())
		done <- err
	}()
	b.ResolveQuestion(waitForQuestionIDAfter(t, frames, qid), 0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a question after the first settled returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the channel never reopened")
	}
}

// A LOCAL question belongs to no turn. Stamping the running one would record the answer
// inside a turn that never asked — a transcript claiming the model was told something it
// was not.
func TestAskChoiceLeavesALocalQuestionOutsideTheRunningTurn(t *testing.T) {
	for _, tc := range []struct {
		name    string
		local   bool
		wantSet bool
	}{
		{name: "the model asking mid-turn carries its turn", local: false, wantSet: true},
		{name: "the CLI asking on its own account carries none", local: true, wantSet: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, frames := newQuestionBridge()
			b.mu.Lock()
			b.activeTurnID = "trn_live"
			b.mu.Unlock()

			done := make(chan struct{})
			go func() {
				req := threeOptions()
				req.Local = tc.local
				_, _ = b.AskChoice(context.Background(), req)
				close(done)
			}()
			qid := waitForQuestionID(t, frames)

			var requested map[string]any
			for _, f := range frames() {
				if f["type"] == "question:requested" {
					requested = f
				}
			}
			if requested == nil {
				t.Fatal("no question:requested frame was emitted")
			}
			got, present := requested["turnId"]
			if tc.wantSet {
				if !present || got != "trn_live" {
					t.Errorf("turnId = %v (present=%v), want trn_live", got, present)
				}
			} else if present {
				t.Errorf("a local question carried turnId %v; it belongs to no turn", got)
			}

			b.ResolveQuestion(qid, 0)
			<-done
		})
	}
}

// A question is never announced AFTER it has been settled.
//
// Registering under the lock and posting after it leaves a window in which the question
// is settleable but has never been announced. An interrupt landing there drains it and
// posts question:answered FIRST; a host with no record of the question discards that,
// and the question:requested arriving next opens a sheet whose pending entry is already
// gone — so every click on it reaches a ResolveQuestion that no-ops. An immortal sheet,
// for a turn cancelled before it was ever drawn.
//
// The Post hook BLOCKS inside the request frame and a second goroutine calls
// SettlePendingQuestions while it is stuck there, which is exactly the interleaving.
//
// NOT deterministic, and the difference matters. The sleep before releasing is a window,
// not a synchronisation point: nothing reports "the drainer has reached the mutex", so a
// pass means the ordering held across a generous window rather than that the drain was
// ever really in contention. It is a strong smoke test for the ordering and no more; the
// guarantee itself is structural — registration, arming and announcement share one lock
// hold, so there is no point at which a drain can observe an unannounced question.
func TestAskChoiceNeverAnnouncesAQuestionItHasAlreadySettled(t *testing.T) {
	var (
		mu       sync.Mutex
		order    []string
		inPost   = make(chan struct{})
		release  = make(chan struct{})
		postOnce sync.Once
	)
	post := func(ev HostEvent) {
		name := ""
		switch ev.(type) {
		case EvQuestionRequested:
			name = "requested"
		case EvQuestionAnswered:
			name = "answered"
		}
		if name == "" {
			return
		}
		if name == "requested" {
			// Park INSIDE the announcement, and let the drainer run while we are here.
			postOnce.Do(func() {
				close(inPost)
				<-release
			})
		}
		mu.Lock()
		order = append(order, name)
		mu.Unlock()
	}

	b := NewBridge(BridgeOptions{SessionID: "ses_test", Post: post})
	done := make(chan struct{})
	go func() {
		_, _ = b.AskChoice(context.Background(), threeOptions())
		close(done)
	}()

	<-inPost
	drained := make(chan struct{})
	go func() {
		// Blocks until AskChoice releases the bridge lock, which is the point: the drain
		// cannot observe a question that has not finished being announced.
		b.SettlePendingQuestions()
		close(drained)
	}()
	// Give the drainer a real chance to get in front of the announcement.
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("AskChoice never returned")
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("SettlePendingQuestions never returned")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) == 0 || order[0] != "requested" {
		t.Fatalf("frames came out as %v; the question was settled before it was announced", order)
	}
}
