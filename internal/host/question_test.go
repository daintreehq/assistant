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
