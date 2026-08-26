package backend

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A code that means the model ALREADY RAN must survive any status the envelope wears.
//
// This is the direction that costs a user real money. `task_output_invalid` is raised only
// after a billed completion, and every status-shaped arm in taskMayHaveBilled answers
// "free" — so for as long as those arms could be reached first, the same billed verdict
// arriving as a 400, 401, 403 or 426 was reported as costing nothing. Over-caveating an
// accurate total is a cosmetic annoyance; reporting a real charge as free is a number the
// user has no way to recover.
func TestABilledVerdictIsNeverOverriddenByItsStatus(t *testing.T) {
	// Every status whose arm returns false: the two auth-shaped ones, the contract one,
	// the protocol-mismatch one, and none at all (a terminal SSE error carries no status).
	for _, status := range []int{
		0,
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusUpgradeRequired,
		http.StatusBadGateway,
	} {
		err := &Error{HTTPStatus: status, Code: "task_output_invalid"}
		if !taskMayHaveBilled(err) {
			t.Errorf("task_output_invalid at status %d: taskMayHaveBilled() = false — a completion that provably ran is being reported as free", status)
		}
	}
}

// The two account 429s are account verdicts like any other: the backend answers them at
// its own door and no model call is reached. They were the two members of the account
// union that taskMayHaveBilled's hand-written enumeration left out.
func TestTheAccountRateLimitsAreProvablyFree(t *testing.T) {
	for _, code := range []string{CodeUsageLimitReached, CodeAccountRateLimited} {
		for _, status := range []int{0, http.StatusTooManyRequests} {
			err := &Error{HTTPStatus: status, Code: code}
			if taskMayHaveBilled(err) {
				t.Errorf("%s at status %d: taskMayHaveBilled() = true — caveats the total over a refusal that never reached a provider", code, status)
			}
		}
	}
}

// The cost of that omission, once a replaced attempt could poison a total: a rate-limited
// first attempt followed by a perfectly good answer reported that answer's exact figure as
// a floor, over spend that provably never happened. The account contract is explicit that
// an account verdict stops before the model call.
func TestARetriedRateLimitStillReportsAnExactTotal(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"type":"rate_limit_error","code":"account_rate_limited","message":"x"}}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"task_1","object":"daintree.task.result","task":"terminal_extract_json","model":"m","output":{},"finish_reason":"stop","usage":{"total_tokens":5,"cost":0.25}}`)
	}))
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond},
		OnCost:  func(ev CostEvent) { events = append(events, ev) },
	})
	if _, err := c.RunTask(context.Background(), TaskRequest{Task: "terminal_extract_json"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if got := attempts.Load(); got != 2 {
		t.Fatalf("attempts = %d, want 2 — nothing was retried, so the test proves nothing", got)
	}
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1", len(events))
	}
	if !events[0].Complete {
		t.Error("a retried call whose replaced attempt was a rate limit reported its total as a floor — the refused attempt spent nothing")
	}
	if events[0].Amount == nil || *events[0].Amount != 0.25 {
		t.Errorf("cost event = %+v, want the exact 0.25 total", events[0])
	}
}

// One transport spending one credential must not keep two accounting rules. The
// non-streaming Respond path asked only whether an earlier attempt was replaced, so a
// SINGLE terminal `upstream_unavailable` — a turn the backend may well have paid for and
// then failed to deliver — reported nothing at all, while the identical error on a task
// correctly caveated the total.
func TestNonStreamingRespondCaveatsAPossiblyBilledFailure(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"type":"api_error","code":"upstream_unavailable","message":"x"}}`)
	}))
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{MaxAttempts: 1},
		OnCost:  func(ev CostEvent) { events = append(events, ev) },
	})
	if _, err := c.Respond(context.Background(), RespondRequest{}); err == nil {
		t.Fatal("want an error")
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempts = %d, want 1 — this case is about a single terminal failure", got)
	}
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1 — a turn the backend may have paid for was reported by nothing", len(events))
	}
	if events[0].Complete || events[0].Amount != nil {
		t.Errorf("cost event = %+v, want an incomplete event with no amount", events[0])
	}
}

// The other half of that rule: a refusal at the backend's own door reached no provider, so
// the non-streaming path must stay silent about it exactly as the task path does.
func TestNonStreamingRespondStaysSilentOnAProvablyFreeRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPaymentRequired)
		_, _ = io.WriteString(w, `{"error":{"type":"billing_error","code":"subscription_required","message":"x"}}`)
	}))
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{MaxAttempts: 1},
		OnCost:  func(ev CostEvent) { events = append(events, ev) },
	})
	if _, err := c.Respond(context.Background(), RespondRequest{}); err == nil {
		t.Fatal("want an error")
	}
	if len(events) != 0 {
		t.Fatalf("OnCost fired %d times for a refusal that never reached a provider, want 0: %+v", len(events), events)
	}
}
