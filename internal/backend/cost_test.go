package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Cost arrives on the TERMINAL `done` event — the earliest it can be known, since
// OpenRouter reports a stream's usage only in its final chunk. Prove it survives the SSE
// parser and reaches both the result and the OnCost hook.
func TestRespondStreamReportsCostFromTheDoneEvent(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":3,"request_id":"req_1","model":"daintree-assistant","state":"dst1.t"}`,
		``,
		`event: delta`,
		`data: {"content":"hi"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{"prompt_tokens":1000,"cached_tokens":900},` +
			`"cost":{"total":0.0000089,"main":0.000003,"selector":0.0000059,"complete":true}}`,
		``,
	}, "\n")

	var events []CostEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}

	if res.Cost == nil {
		t.Fatal("the turn's cost did not reach the result")
	}
	if res.Cost.Total != 0.0000089 {
		t.Errorf("Total = %v, want 0.0000089", res.Cost.Total)
	}
	if res.Cost.Selector == nil || *res.Cost.Selector != 0.0000059 {
		t.Error("the selector breakdown was lost — it is the part prompt work can move")
	}
	if !res.Cost.IsComplete() {
		t.Error("a turn the backend called complete was read as incomplete")
	}

	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1", len(events))
	}
	ev := events[0]
	if ev.Op != "respond" {
		t.Errorf("Op = %q, want \"respond\"", ev.Op)
	}
	if ev.Amount == nil || *ev.Amount != 0.0000089 {
		t.Errorf("Amount = %v, want the turn TOTAL (not just the main call)", ev.Amount)
	}
	// The cache ratio rides along because it explains the number next to it.
	if ev.PromptTokens != 1000 || ev.CachedTokens != 900 {
		t.Errorf("token counts = %d/%d, want 1000/900", ev.PromptTokens, ev.CachedTokens)
	}
}

// A turn with NO cost block must still produce an event — with a nil Amount. Skipping it
// would let a session total silently omit a turn that definitely spent money, and
// present the remainder as a complete sum.
func TestRespondStreamReportsAnUnknownCost(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":3,"request_id":"req_1","model":"daintree-assistant","state":"dst1.t"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{"prompt_tokens":10}}`,
		``,
	}, "\n")

	var events []CostEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1 — an unreported cost is still a billed call", len(events))
	}
	if events[0].Amount != nil {
		t.Errorf("Amount = %v, want nil (absent means unknown, never zero)", *events[0].Amount)
	}
}

// `complete: false` must survive to the hook: it is what turns a session total into a
// lower bound, and it is the one field a naive decode would flatten to a plain false.
func TestRespondStreamCarriesTheIncompleteFlag(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":3,"request_id":"r","model":"m","state":"dst1.t"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{},"cost":{"total":0.5,"complete":false}}`,
		``,
	}, "\n")

	var events []CostEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if len(events) != 1 || events[0].Complete {
		t.Errorf("events = %+v, want exactly one with Complete=false", events)
	}
}

// An omitted `complete` must default to TRUE. The pointer exists for exactly this: an
// older backend that never sends the field would otherwise mark every single turn
// incomplete and permanently caveat a total that is perfectly accurate.
func TestTurnCostCompleteDefaultsToTrueWhenAbsent(t *testing.T) {
	var tc TurnCost
	if err := json.Unmarshal([]byte(`{"total":1.5}`), &tc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if tc.Complete != nil {
		t.Error("an absent `complete` decoded to a non-nil value — the absence is no longer distinguishable")
	}
	if !tc.IsComplete() {
		t.Error("an absent `complete` must read as complete, matching the backend's default")
	}
	// And a nil TurnCost (no block at all) must not panic on the accessor.
	if !(*TurnCost)(nil).IsComplete() {
		t.Error("IsComplete() on a nil TurnCost should be true, not a panic")
	}
}

// A utility task's usage.cost IS that task's total. These are the calls a user has no
// other way to see: they fire from tools, watchers and compaction, never as a turn, and
// a busy session runs dozens of them.
func TestRunTaskReportsItsCost(t *testing.T) {
	var events []CostEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t","object":"daintree.task.result","task":"terminal_summarize",`+
			`"model":"m","output":{},"finish_reason":"stop",`+
			`"usage":{"prompt_tokens":500,"cached_tokens":400,"cost":0.00012},"prompt_version":"v"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RunTask(context.Background(), TaskRequest{Task: "terminal_summarize"}); err != nil {
		t.Fatalf("RunTask: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1", len(events))
	}
	if events[0].Op != "terminal_summarize" {
		t.Errorf("Op = %q — the task id is what makes the breakdown legible", events[0].Op)
	}
	if events[0].Amount == nil || *events[0].Amount != 0.00012 {
		t.Errorf("Amount = %v, want 0.00012", events[0].Amount)
	}
}

// A task rejected at the backend's own door (a 400 contract error) reports nothing: no
// generation ran, so there is no spend to account for, and inventing an "unknown" would
// caveat a session total that is perfectly accurate. Contrast
// TestFailedTaskThatAlreadyBilledReportsUnknownSpend, which covers the failures that DO
// bill.
func TestFailedTaskReportsNoCost(t *testing.T) {
	var events []CostEvent
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"bad","message":"nope"}}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RunTask(context.Background(), TaskRequest{Task: "checkpoint"}); err == nil {
		t.Fatal("want an error")
	}
	if len(events) != 0 {
		t.Errorf("a failed task reported %d cost event(s), want 0", len(events))
	}
}

// A hook that panics must not fail the call it is narrating. Accounting is a
// side-channel; the same rule the audit and debug-log writers follow.
func TestCostHookPanicDoesNotFailTheCall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t","object":"daintree.task.result","task":"checkpoint",`+
			`"model":"m","output":{},"finish_reason":"stop","usage":{"cost":0.1},"prompt_version":"v"}`)
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(CostEvent) { panic("boom") }})
	if _, err := c.RunTask(context.Background(), TaskRequest{Task: "checkpoint"}); err != nil {
		t.Fatalf("a panicking cost hook failed the task: %v", err)
	}
}

// A RETRIED turn under-reports unless the client says so. Each attempt bills
// independently — the backend aggregates re-rolls WITHIN a request, never across HTTP
// attempts — so the succeeding attempt's `cost.total` omits everything the failed one
// spent. The failed attempt got a meta event, which means its runbook selector already ran
// and charged. Reporting that total as exact would show a number below the real bill.
func TestRetriedTurnIsReportedAsIncomplete(t *testing.T) {
	fail := "event: meta\ndata: {\"protocol_version\":2,\"request_id\":\"r1\",\"model\":\"m\",\"state\":\"dst1.a\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_unavailable\",\"message\":\"down\"}}\n\n"
	succeed := "event: meta\ndata: {\"protocol_version\":2,\"request_id\":\"r2\",\"model\":\"m\",\"state\":\"dst1.b\"}\n\n" +
		"event: delta\ndata: {\"content\":\"ok\"}\n\n" +
		"event: done\ndata: {\"finish_reason\":\"stop\",\"usage\":{},\"cost\":{\"total\":0.002,\"complete\":true}}\n\n"

	srv, hits := countingServer(t, func(n int) (int, string) {
		if n == 0 {
			return http.StatusOK, fail
		}
		return http.StatusOK, succeed
	})
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: fastRetry(3), OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if n := hits(); n != 2 {
		t.Fatalf("server hit %d times, want 2", n)
	}
	// ONE event for the whole logical call, not one per attempt — the alternative bug is
	// double-counting.
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1 per logical call", len(events))
	}
	if events[0].Amount == nil || *events[0].Amount != 0.002 {
		t.Errorf("Amount = %v, want the succeeding attempt's reported total", events[0].Amount)
	}
	if events[0].Complete {
		t.Error("a retried turn was reported as a complete sum — the failed attempt's selector spend is missing from it")
	}
}

// A turn that fails for good after the selector already ran must still report — as
// UNKNOWN spend. Reporting nothing would leave the session total looking exact while
// omitting money that was definitely charged.
func TestFailedTurnAfterMetaReportsUnknownSpend(t *testing.T) {
	fail := "event: meta\ndata: {\"protocol_version\":2,\"request_id\":\"r1\",\"model\":\"m\",\"state\":\"dst1.a\"}\n\n" +
		"event: error\ndata: {\"error\":{\"code\":\"upstream_no_compliant_provider\",\"message\":\"none\"}}\n\n"
	srv, _ := countingServer(t, func(int) (int, string) { return http.StatusOK, fail })
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("want an error")
	}
	if len(events) != 1 {
		t.Fatalf("OnCost fired %d times, want 1 — the selector already billed", len(events))
	}
	if events[0].Amount != nil {
		t.Errorf("Amount = %v, want nil (spent, unmeasurable)", *events[0].Amount)
	}
}

// A turn that never reached the point of billing must report NOTHING. Caveating a total
// for a refused socket would make every offline moment look like unaccounted spend.
func TestFailedTurnBeforeMetaReportsNothing(t *testing.T) {
	srv, _ := countingServer(t, func(int) (int, string) {
		return http.StatusBadRequest, `{"error":{"type":"invalid_request_error","code":"bad","message":"nope"}}`
	})
	defer srv.Close()

	var events []CostEvent
	c := NewClient(ClientConfig{BaseURL: srv.URL, OnCost: func(ev CostEvent) { events = append(events, ev) }})
	if _, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{}); err == nil {
		t.Fatal("want an error")
	}
	if len(events) != 0 {
		t.Errorf("a request rejected before any generation reported %d cost event(s), want 0", len(events))
	}
}

// `task_output_invalid` is raised only AFTER a billed completion, usually after a second
// billed repair pass. Treating it as free would let a session run dozens of paid
// extractions and report a total omitting every one.
func TestFailedTaskThatAlreadyBilledReportsUnknownSpend(t *testing.T) {
	cases := []struct {
		name       string
		status     int
		code       string
		wantEvents int
	}{
		{"output invalid — the model already ran", http.StatusBadGateway, "task_output_invalid", 1},
		{"provider outage — may have generated", http.StatusServiceUnavailable, "upstream_unavailable", 1},
		// No generation possible: refused at our door, refused by the provider, or no
		// endpoint to route to.
		{"contract bug", http.StatusBadRequest, "system_messages_not_allowed", 0},
		{"our door rejected the key", http.StatusUnauthorized, "invalid_api_key", 0},
		{"no credit", http.StatusPaymentRequired, "provider_insufficient_credits", 0},
		{"no compliant endpoint", http.StatusServiceUnavailable, "upstream_no_compliant_provider", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, `{"error":{"type":"api_error","code":"`+tc.code+`","message":"x"}}`)
			}))
			t.Cleanup(srv.Close)

			var events []CostEvent
			c := NewClient(ClientConfig{
				BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 1},
				OnCost: func(ev CostEvent) { events = append(events, ev) },
			})
			if _, err := c.RunTask(context.Background(), TaskRequest{Task: "terminal_extract_json"}); err == nil {
				t.Fatal("want an error")
			}
			if len(events) != tc.wantEvents {
				t.Fatalf("OnCost fired %d times, want %d", len(events), tc.wantEvents)
			}
			if tc.wantEvents > 0 && events[0].Amount != nil {
				t.Errorf("Amount = %v, want nil — the spend happened but was never reported", *events[0].Amount)
			}
		})
	}
}
