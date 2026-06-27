package mcp

import (
	"context"
	"testing"
	"time"
)

// Daintree returns a per-tool READ throttle as a tool RESULT (IsError=true), not a
// transport error:
//
//	{"code":"MCP_RATE_LIMITED","message":"Rate limit exceeded for 'terminal.getOutput'. Retry after 1s.","retriable":true,"details":{"retryAfter":1}}
//
// CallTool must absorb that below the model (for read-only callers with a budget) so
// it never surfaces as a failed read that costs a whole LLM round to re-decide.

// throttleResult builds the real-shape throttle result text the server emits.
func throttleResult(retryAfter string) rawResult {
	body := `{"code":"MCP_RATE_LIMITED","message":"Rate limit exceeded for 'terminal.getOutput'. Retry after 1s.","retriable":true`
	if retryAfter != "" {
		body += `,"details":{"retryAfter":` + retryAfter + `}`
	}
	body += `}`
	return rawResult{Text: body, IsError: true}
}

// shrinkThrottleDelays makes the throttle backoff effectively instant so the retry
// tests don't sleep; restored on cleanup. Mirrors how the transport-retry policy
// (mcpReadRetryPolicy) is a tunable var.
func shrinkThrottleDelays(t *testing.T) {
	t.Helper()
	base, max := throttleBaseDelay, maxThrottleRetryAfter
	throttleBaseDelay = time.Millisecond
	maxThrottleRetryAfter = 2 * time.Millisecond
	t.Cleanup(func() { throttleBaseDelay, maxThrottleRetryAfter = base, max })
}

func TestThrottleRetryAfter(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		wantOK    bool
		wantDelay time.Duration
	}{
		{"honours retryAfter seconds", throttleResult("3").Text, true, 3 * time.Second},
		{"default delay when no retryAfter", throttleResult("").Text, true, throttleBaseDelay},
		{"caps a pathological retryAfter", throttleResult("99999").Text, true, maxThrottleRetryAfter},
		{"retryAfter 0 falls back to base", throttleResult("0").Text, true, throttleBaseDelay},
		{"non-throttle error is not a throttle", `{"code":"TERMINAL_OUTPUT","message":"boom"}`, false, 0},
		{"empty text is not a throttle", "", false, 0},
		{"legit output mentioning rate limit is not a throttle", "the API rate limit is 60/min", false, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			delay, ok := throttleRetryAfter(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && delay != tc.wantDelay {
				t.Errorf("delay = %v, want %v", delay, tc.wantDelay)
			}
		})
	}
}

// TestCallToolAbsorbsReadThrottle: a read-only tool that returns a throttle RESULT
// then succeeds is retried within budget and the success surfaces (no IsError).
func TestCallToolAbsorbsReadThrottle(t *testing.T) {
	shrinkThrottleDelays(t)
	low := &fakeLow{callResults: []rawResult{
		throttleResult("1"),
		{Text: "ok"},
	}}
	c := newInjected(low)
	c.Connect(context.Background())

	res, err := c.CallTool(context.Background(), "terminal.getOutput", nil, CallOptions{Retries: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.IsError {
		t.Error("throttle should have been absorbed; got an IsError result")
	}
	if res.Text != "ok" {
		t.Errorf("expected the post-retry success body, got %q", res.Text)
	}
	if low.callCalls != 2 {
		t.Errorf("expected 1 throttle + 1 success = 2 attempts, got %d", low.callCalls)
	}
}

// TestCallToolThrottleNotRetriedForMutation: a mutation that returns a throttle
// RESULT is never retried (read-only guard forces Retries to 0) — the throttle
// surfaces verbatim so the caller can decide, with no double-apply risk.
func TestCallToolThrottleNotRetriedForMutation(t *testing.T) {
	shrinkThrottleDelays(t)
	low := &fakeLow{callResults: []rawResult{throttleResult("1"), {Text: "ok"}}}
	c := newInjected(low)
	c.Connect(context.Background())

	res, err := c.CallTool(context.Background(), "terminal.sendCommand", nil, CallOptions{Retries: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("a mutation throttle must surface as IsError, not be retried")
	}
	if low.callCalls != 1 {
		t.Errorf("a mutation must not retry on a throttle result, got %d attempts", low.callCalls)
	}
}

// TestCallToolNonThrottleIsErrorNotRetried: a non-throttle IsError result (a genuine
// tool error) is returned immediately even for a read-only caller with a budget —
// only a throttle signature triggers the absorb.
func TestCallToolNonThrottleIsErrorNotRetried(t *testing.T) {
	shrinkThrottleDelays(t)
	low := &fakeLow{callResults: []rawResult{
		{Text: `{"code":"TERMINAL_OUTPUT","message":"boom"}`, IsError: true},
		{Text: "ok"},
	}}
	c := newInjected(low)
	c.Connect(context.Background())

	res, err := c.CallTool(context.Background(), "terminal.getOutput", nil, CallOptions{Retries: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("a non-throttle tool error must surface, not be retried")
	}
	if low.callCalls != 1 {
		t.Errorf("a non-throttle IsError must not retry, got %d attempts", low.callCalls)
	}
}

// TestCallToolThrottleExhaustsBudget: a throttle on every attempt makes exactly
// 1+Retries attempts, then surfaces the throttle result (no infinite loop).
func TestCallToolThrottleExhaustsBudget(t *testing.T) {
	shrinkThrottleDelays(t)
	low := &fakeLow{callResults: []rawResult{
		throttleResult("1"), throttleResult("1"), throttleResult("1"),
	}}
	c := newInjected(low)
	c.Connect(context.Background())

	res, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.IsError {
		t.Error("after the budget is spent the throttle result should surface")
	}
	if low.callCalls != 3 {
		t.Errorf("expected 1 initial + 2 retries = 3 attempts, got %d", low.callCalls)
	}
}

// TestCallToolThrottleAbortMidBackoff: a ctx cancelled during the throttle backoff
// propagates the abort and does NOT degrade the connection (same contract as a
// cancelled transport retry).
func TestCallToolThrottleAbortMidBackoff(t *testing.T) {
	// A real (not shrunk) base delay so the cancel lands during the sleep.
	low := &fakeLow{callResults: []rawResult{throttleResult(""), {Text: "ok"}}}
	c := newInjected(low)
	c.Connect(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(5 * time.Millisecond); cancel() }()

	_, err := c.CallTool(ctx, "terminal.getOutput", nil, CallOptions{Retries: 2})
	if err == nil {
		t.Fatal("expected the aborted backoff to surface an error")
	}
	if !c.IsConnected() {
		t.Error("an aborted throttle backoff must NOT degrade the connection")
	}
}
