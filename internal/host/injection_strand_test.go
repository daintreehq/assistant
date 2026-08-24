package host

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// The injection-strand race: handlePrompt observes busy, unlocks, and calls
// InjectPrompt — but injections are only BUFFERED; the running turn folds them in
// at its next iteration boundary, and a turn already past its FINAL fold check
// can complete before (or just after) the injection lands. The host used to
// report "prompt-folded" and move on, silently stranding the prompt in the
// session buffer with no new turn scheduled. Two complementary closes now
// guarantee delivery: finishPromptTurn reclaims unconsumed injections while busy
// is still held, and handlePrompt re-checks busy after injecting. These tests
// drive the REAL Host wiring with the cooperative wakeSession fake, whose
// InjectPrompt never folds — modeling a turn past its final drain. Run with
// -race.

// A prompt folded into a turn that then completes WITHOUT consuming it must be
// reclaimed and dispatched as a fresh command turn — never stranded.
func TestStrandedInjectionRedispatchedAsFreshTurn(t *testing.T) {
	sess := newWakeSession()
	h, _, _ := newWakeHost(t, sess)

	go h.handlePrompt("first")
	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}

	// Folds via InjectPrompt (the fake never consumes it — final drain passed).
	h.handlePrompt("second")
	if got := sess.injectedPrompts(); len(got) != 1 || got[0] != "second" {
		t.Fatalf("prompt while busy was not buffered via InjectPrompt: %v", got)
	}

	// The turn completes without ever folding the injection. finishPromptTurn
	// must reclaim it and dispatch it as a fresh turn.
	close(sess.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		texts := sess.sentTexts()
		if len(texts) >= 2 {
			if texts[1] != "second" {
				t.Fatalf("redispatched turn text = %q, want the stranded injection", texts[1])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stranded injection was never redispatched as a fresh turn (sends: %v)", sess.sentTexts())
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := sess.injectedPrompts(); len(got) != 0 {
		t.Fatalf("injection buffer not drained after redispatch: %v", got)
	}
	waitNotBusy(t, h)
}

// Multiple stranded injections are reclaimed in arrival order (retraction is
// LIFO — the reclaim must restore FIFO) and joined into one fresh turn.
func TestMultipleStrandedInjectionsKeepArrivalOrder(t *testing.T) {
	sess := newWakeSession()
	h, _, _ := newWakeHost(t, sess)

	go h.handlePrompt("first")
	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}
	h.handlePrompt("second")
	h.handlePrompt("third")

	close(sess.release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		texts := sess.sentTexts()
		if len(texts) >= 2 {
			want := "second\n\nthird"
			if texts[1] != want {
				t.Fatalf("reclaimed prompt = %q, want arrival-ordered %q", texts[1], want)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stranded injections never redispatched")
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Race hammer: a prompt arriving exactly as the running turn completes must be
// delivered to the session EXACTLY once whichever interleaving wins — as its own
// fresh turn (non-busy path), via handlePrompt's post-inject re-check, or via
// finishPromptTurn's reclaim. The fake session never folds injections, so any
// delivery must surface as a Send containing the text.
func TestPromptRacingTurnCompletionIsNeverStranded(t *testing.T) {
	for i := 0; i < 50; i++ {
		sess := newWakeSession()
		h, _, _ := newWakeHost(t, sess)

		go h.handlePrompt("first")
		select {
		case <-sess.started:
		case <-time.After(2 * time.Second):
			t.Fatal("first turn never started")
		}

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.handlePrompt("racing prompt")
		}()
		close(sess.release) // the first turn completes concurrently
		wg.Wait()

		deadline := time.Now().Add(2 * time.Second)
		for {
			delivered := 0
			for _, text := range sess.sentTexts() {
				if strings.Contains(text, "racing prompt") {
					delivered++
				}
			}
			if delivered == 1 && len(sess.injectedPrompts()) == 0 {
				break
			}
			if delivered > 1 {
				t.Fatalf("iter %d: racing prompt delivered %d times, want exactly once", i, delivered)
			}
			if time.Now().After(deadline) {
				t.Fatalf("iter %d: racing prompt stranded (sends=%v buffered=%v)",
					i, sess.sentTexts(), sess.injectedPrompts())
			}
			time.Sleep(time.Millisecond)
		}
		waitNotBusy(t, h)
	}
}

// Interrupt semantics: aborting a turn discards its buffered-but-unfolded
// injections (the host's Ctrl+C discard) — a redirect typed into abandoned
// work must NOT resurrect as a fresh turn.
func TestInterruptDiscardsUnfoldedInjection(t *testing.T) {
	sess := newWakeSession()
	h, _, _ := newWakeHost(t, sess)

	go h.handlePrompt("first")
	select {
	case <-sess.started:
	case <-time.After(2 * time.Second):
		t.Fatal("first turn never started")
	}
	h.handlePrompt("redirect") // buffered into the running turn
	h.handleInterrupt()        // abort: the redirect must die with the turn

	waitNotBusy(t, h)
	time.Sleep(30 * time.Millisecond) // give a wrong redispatch a moment to appear
	if got := sess.sendCount(); got != 1 {
		t.Fatalf("interrupted turn's injection resurrected as a fresh turn (%d sends)", got)
	}
	if got := sess.injectedPrompts(); len(got) != 0 {
		t.Fatalf("interrupt did not discard the buffered injection: %v", got)
	}
}
