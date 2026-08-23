package mcpserver

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// A turn that returns cleanly but emits NO terminal event is not an empty success — it is
// exactly the shape a runtime with an unwired sink produces, which this package has
// already shipped once. Reporting it as success is how that bug stayed invisible: the
// caller was told the run completed, so nothing looked wrong except that nothing had
// happened.
func TestARunWithNoTerminalEventFailsClosed(t *testing.T) {
	fake := newFakeRuntime("ses_silent")
	fake.silent = true
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	fake.letFinish()

	run, err := sess.Ask(context.Background(), "do the thing", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()

	if got := run.Status(); got != RunFailed {
		t.Fatalf("status = %q, want error — a run that recorded no terminal event has no trustworthy outcome", got)
	}
	_, _, _, _, errMsg, _, _, _ := run.Snapshot(0, 0)
	if !strings.Contains(errMsg, "RUN_EVENT_STREAM_INCOMPLETE") {
		t.Errorf("error = %q, want a RUN_EVENT_STREAM_INCOMPLETE code the caller can branch on", errMsg)
	}
	// The reply is kept, but as diagnostics rather than an answer.
	_, _, _, content, _, _, _, _ := run.Snapshot(0, 0)
	if content == "" {
		t.Error("the reply was discarded; it may be the only account of what happened")
	}
}

// The recorder's buffer promised that a round interrupted before assistant:end "still
// reports what it had said". That was true only for the paths that happened to call
// flush() — a new round, an interjection, a skill load. A turn cancelled mid-sentence hit
// none of them, so the one case the buffer exists for was the one it did not cover.
func TestPartialProseSurvivesCancellation(t *testing.T) {
	fake := newFakeRuntime("ses_partial")
	fake.script = func(sink agent.EventSink) {
		sink.AssistantStart()
		sink.AssistantToken("I was half way through saying ")
		sink.AssistantToken("something useful")
	}
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	run, err := sess.Ask(context.Background(), "explain", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// Cancel while Send is still blocked, so nothing flushes the buffer on its own.
	if err := sess.Interrupt(run.ID); err != nil {
		t.Fatal(err)
	}
	<-run.Done()

	if got := run.Status(); got != RunCancelled {
		t.Fatalf("status = %q, want cancelled", got)
	}
	_, _, _, content, _, _, _, _ := run.Snapshot(0, 0)
	if !strings.Contains(content, "half way through") {
		t.Errorf("the streamed prose was dropped on cancellation; content = %q", content)
	}
}

// A run that no caller ever interrupts holds the session — and with it the project's
// owner lease — for as long as the process lives. The deadline is what makes the stuck
// case self-clearing, and a caller must be able to tell "it ran too long" from "you
// stopped it".
func TestARunIsBoundedByItsDeadline(t *testing.T) {
	fake := newFakeRuntime("ses_deadline")
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	// Never released: the fake's Send blocks until its context dies.
	run, err := sess.Ask(context.Background(), "wedge", 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-run.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the run outlived its deadline")
	}
	if got := run.Status(); got != RunCancelled {
		t.Fatalf("status = %q, want cancelled", got)
	}
	_, _, _, _, errMsg, _, _, _ := run.Snapshot(0, 0)
	if !strings.Contains(errMsg, "RUN_DEADLINE_EXCEEDED") {
		t.Errorf("error = %q — a caller cannot tell an expired deadline from its own interrupt", errMsg)
	}
	// The session is usable again, which is the whole point.
	if sess.Busy() {
		t.Error("the session is still busy after its run was bounded")
	}
}

// timeoutMs is validated as an INTEGER before it becomes a Duration. time.Duration is
// int64 nanoseconds, so a large millisecond count overflows and wraps negative, which
// then reads as "non-positive" and silently becomes the default — a caller asking for an
// enormous timeout would get 30 minutes with no indication its number was discarded.
func TestRunDeadlineValidatesBeforeConverting(t *testing.T) {
	if d, err := resolveRunDeadline(0); err != nil || d != DefaultRunDeadline {
		t.Errorf("an omitted timeout gave (%v, %v), want the default", d, err)
	}
	if _, err := resolveRunDeadline(-1); err == nil {
		t.Error("a negative timeout was accepted")
	}
	// The overflow case: 1<<53 ms is ~285,000 years and wraps int64 nanoseconds.
	if _, err := resolveRunDeadline(1 << 53); err == nil {
		t.Error("an overflowing timeout was accepted rather than refused")
	}
	// And it is refused for exceeding the cap, not silently clamped.
	over := int(MaxRunDeadline/time.Millisecond) + 1
	if _, err := resolveRunDeadline(over); err == nil {
		t.Error("a timeout above the server maximum was accepted")
	}
	if d, err := resolveRunDeadline(60_000); err != nil || d != time.Minute {
		t.Errorf("a valid timeout gave (%v, %v)", d, err)
	}
}

// Closing an already-closed session is the one call a caller is told to ALWAYS make, and
// it was the one that looked like it failed. A lost response then left a harness choosing
// between ignoring every close error and retrying forever.
func TestCloseIsSafeToRetry(t *testing.T) {
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_close"), nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := reg.Close(context.Background(), sess.ID)
	if err != nil || !res.Acted || res.State != "closed" {
		t.Fatalf("first close: %+v err=%v", res, err)
	}
	// The retry is a report, not a failure.
	res, err = reg.Close(context.Background(), sess.ID)
	if err != nil {
		t.Fatalf("retrying a successful close returned an error: %v", err)
	}
	if res.Acted {
		t.Error("the retry claimed to have done the teardown")
	}
	if res.State != "already-closed" {
		t.Errorf("state = %q, want already-closed", res.State)
	}
	// A session that never existed is the same answer: the caller asked for it to be
	// closed, and it is.
	if res, err := reg.Close(context.Background(), "ses_never"); err != nil || res.Acted {
		t.Errorf("closing an unknown session: %+v err=%v", res, err)
	}
}

// A teardown that FAILS must stay visible. Deleting the session up front meant a failed
// close took it out of session.list while its runtime might still hold the project lease
// — the caller could not retry it, could not see it, and had no way to learn the project
// was stuck.
func TestAFailedCloseStaysVisibleAndNamesTheStuckLease(t *testing.T) {
	fake := newFakeRuntime("ses_stuck")
	fake.closeErr = errors.New("lease release timed out")
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	res, err := reg.Close(context.Background(), sess.ID)
	if err == nil {
		t.Fatal("a failed teardown reported success")
	}
	if res.State != string(StateCloseFailed) {
		t.Errorf("state = %q, want close-failed", res.State)
	}
	if !strings.Contains(err.Error(), "lease") {
		t.Errorf("the error does not mention the lease: %v", err)
	}

	// Still listed, so a caller can see the project is stuck.
	found := false
	for _, s := range reg.List() {
		if s.ID == sess.ID {
			found = true
			if got := s.State(); got != StateCloseFailed {
				t.Errorf("listed state = %q, want close-failed", got)
			}
		}
	}
	if !found {
		t.Error("a session whose close failed vanished from the registry while its lease may still be held")
	}
}

// A caller that loses an ask response knows only that the session is busy, and retrying
// ask says the same unhelpful thing. The live run id is the fact that turns that back
// into a handle.
func TestBusyNamesTheLiveRun(t *testing.T) {
	fake := newFakeRuntime("ses_busy")
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	run, err := sess.Ask(context.Background(), "first", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { fake.letFinish(); <-run.Done() }()

	if got := sess.CurrentRunID(); got != run.ID {
		t.Errorf("CurrentRunID = %q, want %q", got, run.ID)
	}

	_, err = sess.Ask(context.Background(), "second", time.Minute)
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("a second ask returned %v, want ErrBusy", err)
	}
	var busy *BusyError
	if !errors.As(err, &busy) {
		t.Fatalf("the refusal does not carry the live run: %v", err)
	}
	if busy.CurrentRunID != run.ID {
		t.Errorf("BusyError names run %q, want %q", busy.CurrentRunID, run.ID)
	}
	if !strings.Contains(err.Error(), run.ID) {
		t.Errorf("the prose does not name the run to poll: %v", err)
	}
}

// A close that hangs must not hold the MCP request handler open. The SDK waits for
// in-flight handlers before Run returns, so a wedged handler stopped the server's own
// CloseAll from ever running — the process stayed alive holding every project's flock,
// which is the exact opposite of what closing a session is for.
func TestAHungCloseReportsAndReleasesTheCaller(t *testing.T) {
	fake := newFakeRuntime("ses_hung")
	blockClose := make(chan struct{})
	fake.closeBlock = blockClose
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	// A caller that gives up quickly gets an answer quickly.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	started := time.Now()
	res, err := reg.Close(ctx, sess.ID)
	if err != nil {
		t.Fatalf("a slow close was reported as a failure: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("close blocked the caller for %v", elapsed)
	}
	if res.State != string(StateClosing) {
		t.Errorf("state = %q, want closing", res.State)
	}

	// And it stays VISIBLE while it closes, so the capacity it is consuming is
	// accounted for rather than simply missing.
	found := false
	for _, s := range reg.List() {
		if s.ID == sess.ID {
			found = true
			if got := s.State(); got != StateClosing {
				t.Errorf("listed state = %q, want closing", got)
			}
		}
	}
	if !found {
		t.Error("a session mid-teardown vanished from session.list while still holding its lease")
	}

	close(blockClose)
}

// CloseAll must cover teardowns ALREADY in flight, not just registered sessions. Left
// out, a close that was running when the server stopped got no bound at all — and its
// lease was exactly the one shutdown exists to release.
func TestCloseAllWaitsForATeardownAlreadyInFlight(t *testing.T) {
	fake := newFakeRuntime("ses_inflight")
	blockClose := make(chan struct{})
	fake.closeBlock = blockClose
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := reg.Close(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	allDone := make(chan struct{})
	go func() { reg.CloseAll(); close(allDone) }()

	select {
	case <-allDone:
		t.Fatal("CloseAll returned while a teardown it should be waiting on was still running")
	case <-time.After(200 * time.Millisecond):
	}

	close(blockClose)
	select {
	case <-allDone:
	case <-time.After(5 * time.Second):
		t.Fatal("CloseAll did not finish after the teardown completed")
	}
}

// The real agent emits AssistantCancelled("") — it has no final answer to give — and
// clearing the buffer there threw away the one account of what the turn had been saying.
// The fake in TestPartialProseSurvivesCancellation did NOT emit that terminal event, so
// it exercised a path production never takes.
func TestPartialProseSurvivesTheRealCancellationShape(t *testing.T) {
	fake := newFakeRuntime("ses_realcancel")
	fake.script = func(sink agent.EventSink) {
		sink.AssistantStart()
		sink.AssistantToken("I was drafting ")
		sink.AssistantToken("the answer")
		// Exactly what internal/agent does on cancellation: a terminal event with NO
		// content, followed by a sentinel reply.
		sink.AssistantCancelled("")
	}
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	fake.letFinish()

	run, err := sess.Ask(context.Background(), "explain", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()

	_, _, _, content, _, _, _, _ := run.Snapshot(0, 0)
	if !strings.Contains(content, "drafting") {
		t.Errorf("the streamed prose was dropped by a content-less cancellation; content = %q", content)
	}
}

// A failed teardown is TERMINAL. Runtime.Close tears down an App — store, MCP client,
// scheduler, lease — and running it again over a half-closed one is not a retry. The
// retry must therefore not claim to have acted.
func TestRetryingAFailedCloseDoesNotClaimToHaveActed(t *testing.T) {
	fake := newFakeRuntime("ses_failclose")
	fake.closeErr = errors.New("lease release timed out")
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := reg.Close(context.Background(), sess.ID); err == nil {
		t.Fatal("a failed teardown reported success")
	}
	res, err := reg.Close(context.Background(), sess.ID)
	if err == nil {
		t.Fatal("retrying a failed close reported success")
	}
	if res.Acted {
		t.Error("the retry claimed to have torn the runtime down again; it did not")
	}
	if !strings.Contains(err.Error(), "restart") {
		t.Errorf("the error does not say what actually recovers this: %v", err)
	}
	if got := fake.closes(); got != 1 {
		t.Errorf("Runtime.Close ran %d times; a failed teardown must not be re-run over a half-closed App", got)
	}
}

// A caller whose ask response was lost needs the handle back even when the run has
// already finished — which is the case a FAST run lands in, and the one it cannot
// otherwise get out of, since a retried ask on an idle session is accepted and simply
// does the work twice.
func TestRecentRunsRecoverAHandleAfterTheRunFinished(t *testing.T) {
	fake := newFakeRuntime("ses_recent")
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	fake.letFinish()

	run, err := sess.Ask(context.Background(), "the prompt whose response I lost", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	<-run.Done()

	live := sess.Live()
	if live.Busy || live.CurrentRunID != "" {
		t.Fatalf("the session still reports a live run: %+v", live)
	}
	if len(live.Recent) == 0 {
		t.Fatal("a finished run left no way to recover its handle")
	}
	if live.Recent[0].RunID != run.ID {
		t.Errorf("newest recent run = %q, want %q", live.Recent[0].RunID, run.ID)
	}
	if !strings.Contains(live.Recent[0].Prompt, "response I lost") {
		t.Errorf("the summary does not echo the prompt, so a caller cannot recognize its own run: %+v", live.Recent[0])
	}
	if live.Recent[0].Status != string(RunSucceeded) {
		t.Errorf("status = %q, want success", live.Recent[0].Status)
	}
}

// The cap must admit exactly N concurrent opens, not N-1. Releasing the reservation in a
// defer that runs AFTER registration left an opener counted in both `sessions` and
// `opening`, so a second opener under a cap of 2 computed 1+2-1 == 2 and refused itself
// even though the two of them exactly filled the cap.
func TestTheCapAdmitsExactlyItsLimitUnderConcurrentOpens(t *testing.T) {
	const limit = 2
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		entered <- struct{}{}
		<-release
		return newFakeRuntime(domain.NewID("ses_")), nil
	})
	reg.SetPolicy(ServerPolicy{MaxSessions: limit})

	var wg sync.WaitGroup
	var mu sync.Mutex
	opened := 0
	for i := 0; i < limit; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := reg.Open(context.Background(), OpenParams{}); err == nil {
				mu.Lock()
				opened++
				mu.Unlock()
			}
		}()
	}
	// Both must reach the factory: the cap is 2 and there are 2 of them.
	for i := 0; i < limit; i++ {
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("the cap refused an opener that fitted inside it")
		}
	}
	close(release)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if opened != limit {
		t.Fatalf("%d of %d concurrent opens succeeded under a cap of %d", opened, limit, limit)
	}
}

// A session handed out by Get can begin closing before the call reaches the runtime.
// Acting then reports success for work folded into a turn that is being cancelled, and
// reads a store that teardown is closing.
func TestRuntimeCallsAreRefusedOnceCloseHasBegun(t *testing.T) {
	fake := newFakeRuntime("ses_gate")
	blockClose := make(chan struct{})
	fake.closeBlock = blockClose
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return fake, nil
	})
	sess, err := reg.Open(context.Background(), OpenParams{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := reg.Close(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}

	if err := sess.Inject("", "steer this"); !errors.Is(err, ErrNoSession) {
		t.Errorf("inject on a closing session returned %v, want ErrNoSession", err)
	}
	if _, _, err := sess.Attention(context.Background(), 0, false); !errors.Is(err, ErrNoSession) {
		t.Errorf("attention on a closing session returned %v, want ErrNoSession", err)
	}
	if _, _, err := sess.AcknowledgeAttention(context.Background(), []string{"q_1"}); !errors.Is(err, ErrNoSession) {
		t.Errorf("attention ack on a closing session returned %v, want ErrNoSession", err)
	}

	close(blockClose)
}
