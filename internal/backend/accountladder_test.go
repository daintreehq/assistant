package backend

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
)

// accountladder_test.go covers the STREAMED turn's account ladder. The JSON path is
// exercised end to end from internal/auth against a real Manager; this side pins the
// client's own rules, which is where the "at most once, before anything visible"
// boundary actually lives.

// fakeObserver is a TokenSource that also observes outcomes, so a test can see exactly
// what the client reported and what it presented.
type fakeObserver struct {
	mu sync.Mutex
	// tokens are handed out in order; the last repeats.
	tokens    []string
	gen       uint64
	active    []uint64
	verdicts  []error
	takenGens []uint64
}

func (f *fakeObserver) AccessToken(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens) == 0 {
		return "", nil
	}
	tok := f.tokens[0]
	if len(f.tokens) > 1 {
		f.tokens = f.tokens[1:]
	}
	return tok, nil
}

func (f *fakeObserver) Invalidate(string) {}

// Secrets makes the fake a TokenScrubber, exactly as the real *auth.Manager is.
//
// Without it the fake silently disables every scrub in the client, so a test that
// checked "the bearer did not survive into this field" would pass against a build that
// scrubbed nothing at all — the one shape of green that is worse than red.
func (f *fakeObserver) Secrets() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tokens) == 0 {
		return nil
	}
	return append([]string(nil), f.tokens...)
}

func (f *fakeObserver) Generation() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takenGens = append(f.takenGens, f.gen)
	return f.gen
}

func (f *fakeObserver) MarkActive(gen uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.active = append(f.active, gen)
}

func (f *fakeObserver) ApplyBackendVerdict(_ context.Context, _ uint64, _ string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.verdicts = append(f.verdicts, err)
}

// verdictCodes reports the account code of each verdict observed, so a test can pin
// WHICH failure drove a refresh rather than merely that one happened.
func (f *fakeObserver) verdictCodes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	codes := make([]string, 0, len(f.verdicts))
	for _, err := range f.verdicts {
		var be *Error
		if errors.As(err, &be) {
			codes = append(codes, be.Code)
			continue
		}
		codes = append(codes, "")
	}
	return codes
}

func (f *fakeObserver) counts() (active, verdicts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.active), len(f.verdicts)
}

const expiredMidStream = "event: error\ndata: " +
	`{"error":{"type":"authentication_error","code":"auth_token_expired","message":"no"}}` + "\n\n"

// The ladder on the streamed path: an expired credential before anything is emitted is
// renewed and the turn replayed, exactly once.
func TestAStreamedTurnRefreshesAndReplaysOnce(t *testing.T) {
	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, expiredMidStream
		}
		return http.StatusOK, preambleStream
	})
	defer srv.Close()

	obs := &fakeObserver{tokens: []string{"token-a", "token-b"}}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs, Retry: RetryPolicy{MaxAttempts: 1}})

	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("the turn was not replayed after a refresh: %v", err)
	}
	if got := hits(); got != 2 {
		t.Errorf("requests = %d, want exactly 2", got)
	}
	active, verdicts := obs.counts()
	if verdicts != 1 {
		t.Errorf("verdicts = %d, want 1 for the expired attempt", verdicts)
	}
	if active != 1 {
		t.Errorf("MarkActive calls = %d, want 1 for the successful replay", active)
	}
}

// "Only before visible stream output" — and a PREAMBLE is visible output. It is painted
// on screen before any executor prose arrives, so replaying after one duplicates text
// the user has already read.
//
// This is deliberately STRICTER than the transient retry, which does replay after a
// preamble (see TestPreambleIsNotTheRetryBoundary). The two are different questions: a
// transient failure means the work probably did not happen, while an attempt that got
// far enough to emit a preamble has already been accepted and paid for. Nobody should
// "fix" the inconsistency by loosening this one.
func TestAStreamedTurnDoesNotReplayAfterAPreamble(t *testing.T) {
	const expiredAfterPreamble = "event: meta\ndata: {}\n\n" +
		"event: preamble\ndata: {\"id\":\"pre_1\",\"content\":\"On it.\",\"provisional\":true,\"commit_on\":\"done\"}\n\n" +
		expiredMidStream

	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, expiredAfterPreamble
		}
		return http.StatusOK, preambleStream
	})
	defer srv.Close()

	obs := &fakeObserver{tokens: []string{"token-a", "token-b"}}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs, Retry: RetryPolicy{MaxAttempts: 1}})

	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("the turn reported success after being abandoned")
	}
	if got := hits(); got != 1 {
		t.Errorf("requests = %d, want 1 — the turn was replayed after showing text", got)
	}
	// The verdict still reaches the observer, so the NEXT turn starts with a fresh
	// credential. Ending the turn must not also mean forgetting why.
	if _, verdicts := obs.counts(); verdicts != 1 {
		t.Errorf("verdicts = %d, want 1 — the credential problem was swallowed", verdicts)
	}
}

// A settled refusal is never replayed on the streamed path either.
func TestAStreamedSettledRefusalIsNotReplayed(t *testing.T) {
	const refused = "event: error\ndata: " +
		`{"error":{"type":"authentication_error","code":"auth_permission_denied","message":"no"}}` + "\n\n"

	srv, hits := countingServer(t, func(int) (int, string) { return http.StatusOK, refused })
	defer srv.Close()

	obs := &fakeObserver{tokens: []string{"token-a"}}
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs, Retry: RetryPolicy{MaxAttempts: 1}})

	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("a refused credential reported success")
	}
	if got := hits(); got != 1 {
		t.Errorf("requests = %d, want 1 — a settled 403 was replayed", got)
	}
}

// An anonymous turn observes nothing. There is no session to confirm, and reporting one
// would show an account on an install that has never signed in.
func TestAnAnonymousStreamedTurnObservesNothing(t *testing.T) {
	srv, _ := countingServer(t, func(int) (int, string) { return http.StatusOK, preambleStream })
	defer srv.Close()

	obs := &fakeObserver{} // no tokens: every request goes out bare
	c := NewClient(ClientConfig{BaseURL: srv.URL, TokenSource: obs})

	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if active, verdicts := obs.counts(); active != 0 || verdicts != 0 {
		t.Errorf("observed %d successes and %d verdicts for an anonymous turn", active, verdicts)
	}
}
