package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The two done payloads below are VERBATIM output from the backend's own pydantic
// models (`StreamDone(...).model_dump(exclude_none=True)`, ../assistant-backend
// contracts/daintree_api.py), not a hand-written approximation of them. That is the
// point: this is a cross-repo contract, and a test that decodes bytes we invented
// ourselves proves only that we are self-consistent.
const (
	doneWithFullTimings = `{"finish_reason": "stop", "usage": {"prompt_tokens": 0, "completion_tokens": 0, ` +
		`"total_tokens": 0, "cached_tokens": 0}, "timings": {"selection_ms": 863, "docs_ms": 210, ` +
		`"preparation_ms": 1104, "upstream_open_ms": 1290, "thinking_ms": 42, "first_output_ms": 1655, ` +
		`"generation_ms": 4120, "total_ms": 5775}, "warnings": []}`

	// An ordinary tool-continuation round: no selector, no docs lookup, thinking off.
	// exclude_none drops all three KEYS — this is what "absent" looks like on the wire.
	doneWithSparseTimings = `{"finish_reason": "stop", "usage": {"prompt_tokens": 0, "completion_tokens": 0, ` +
		`"total_tokens": 0, "cached_tokens": 0}, "timings": {"preparation_ms": 12, "upstream_open_ms": 300, ` +
		`"first_output_ms": 420, "generation_ms": 900, "total_ms": 1320}, "warnings": []}`

	// A backend that predates the timings block: no `timings` key at all.
	doneWithoutTimings = `{"finish_reason": "stop", "usage": {}}`
)

// timingsSSE builds a stream whose terminal `done` event carries the given payload.
func timingsSSE(done string) string {
	return strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_1","model":"daintree-assistant","state":"dst1.t"}`,
		``,
		`event: delta`,
		`data: {"content":"hi"}`,
		``,
		`event: done`,
		`data: ` + done,
		``,
	}, "\n")
}

// streamOnce serves one canned SSE body and returns the parsed result.
func streamOnce(t *testing.T, body string) RespondResult {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	res, err := NewClient(ClientConfig{BaseURL: srv.URL}).
		RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	return res
}

// Timings ride the TERMINAL event for the same reason cost does — `meta` is emitted
// before the model is even opened, so it cannot know generation or total. Prove a full
// block survives the SSE parser onto the result.
func TestRespondStreamCarriesTimingsFromTheDoneEvent(t *testing.T) {
	res := streamOnce(t, timingsSSE(doneWithFullTimings))

	tm := res.Timings
	if tm == nil {
		t.Fatal("the turn's phase timings did not reach the result")
	}
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"selection_ms", tm.SelectionMs, 863},
		{"docs_ms", tm.DocsMs, 210},
		{"preparation_ms", tm.PreparationMs, 1104},
		{"upstream_open_ms", tm.UpstreamOpenMs, 1290},
		{"thinking_ms", tm.ThinkingMs, 42},
		{"first_output_ms", tm.FirstOutputMs, 1655},
		{"generation_ms", tm.GenerationMs, 4120},
		{"total_ms", tm.TotalMs, 5775},
	} {
		if c.got == nil {
			t.Errorf("%s was dropped", c.name)
			continue
		}
		if *c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, *c.got, c.want)
		}
	}
	if !tm.Any() {
		t.Error("Any() = false on a fully populated block")
	}
}

// The one rule that makes these numbers usable: the backend serializes with
// exclude_none, so a phase that DID NOT HAPPEN is a missing key. A selector that never
// ran and a selector that answered instantly are different facts, and decoding the
// first as 0 merges them into a measurement that was never taken.
func TestTimingsAbsentPhasesStayNilNotZero(t *testing.T) {
	res := streamOnce(t, timingsSSE(doneWithSparseTimings))

	tm := res.Timings
	if tm == nil {
		t.Fatal("timings block was dropped")
	}
	if tm.SelectionMs != nil {
		t.Errorf("selection_ms = %d — a skipped selector must stay ABSENT, never 0", *tm.SelectionMs)
	}
	if tm.DocsMs != nil {
		t.Errorf("docs_ms = %d — no lookup happened", *tm.DocsMs)
	}
	if tm.ThinkingMs != nil {
		t.Errorf("thinking_ms = %d — the interactive surface is non-thinking", *tm.ThinkingMs)
	}
	if tm.TotalMs == nil || *tm.TotalMs != 1320 {
		t.Errorf("total_ms = %v, want 1320", tm.TotalMs)
	}
}

// A backend that does not report timings at all (the deployed one, until this ships)
// must leave the block nil rather than synthesize an all-zero turn.
func TestTimingsAbsentBlockIsNil(t *testing.T) {
	res := streamOnce(t, timingsSSE(doneWithoutTimings))
	if res.Timings != nil {
		t.Fatalf("timings = %+v, want nil for a backend that reports none", res.Timings)
	}
	if res.Timings.Any() {
		t.Error("Any() = true on a nil block")
	}
	// An EMPTY block is the same fact as no block: nothing was measured.
	if (&TurnTimings{}).Any() {
		t.Error("Any() = true on a block with no measured phase")
	}
}

// The severe case: these numbers are TELEMETRY, and the terminal `done` event is
// decoded strictly — one Unmarshal failure aborts the stream and fails a turn that was
// already generated, already streamed to the user, and already billed to their key. A
// diagnostic field must never be able to do that.
//
// The trigger is not hypothetical. Every phase is a `*int`, so the backend dropping a
// single `round()` and reporting `5775.3` would have killed every turn.
func TestMalformedTimingsNeverFailTheTurn(t *testing.T) {
	cases := []struct {
		name, timings string
		wantTotal     *int
	}{
		{"float where an int was promised", `{"total_ms": 5775.3}`, ptr(5775)},
		{"a string", `{"total_ms": "fast"}`, nil},
		{"null", `{"total_ms": null}`, nil},
		{"not an object at all", `"soon"`, nil},
		{"an array", `[1,2,3]`, nil},
		{"absurd magnitude", `{"total_ms": 1e30}`, nil},
		{"a negative duration", `{"total_ms": -5}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			body := timingsSSE(`{"finish_reason":"stop","usage":{},"timings":` + c.timings + `}`)
			res := streamOnce(t, body) // fatals if the stream errored
			if res.FinishReason != "stop" {
				t.Fatalf("finish reason = %q — the turn was lost to a telemetry field", res.FinishReason)
			}
			// The ANSWER is what must survive, not merely the envelope: this whole
			// tolerance exists so a diagnostic field cannot discard generated content
			// the user already saw and already paid for.
			if res.Message.Content != "hi" {
				t.Fatalf("content = %q, want the streamed answer intact", res.Message.Content)
			}
			got := res.Timings.TotalMs
			switch {
			case c.wantTotal == nil && got != nil:
				t.Errorf("total_ms = %d, want it treated as unmeasured", *got)
			case c.wantTotal != nil && (got == nil || *got != *c.wantTotal):
				t.Errorf("total_ms = %v, want %d", got, *c.wantTotal)
			}
		})
	}
}

// One unparseable phase must cost only its own phase. Failing the whole block would
// throw away seven good measurements to punish one bad one.
func TestMalformedTimingFieldIsIsolated(t *testing.T) {
	res := streamOnce(t, timingsSSE(`{"finish_reason":"stop","usage":{},`+
		`"timings":{"selection_ms":"whoops","preparation_ms":1104,"total_ms":5775}}`))

	tm := res.Timings
	if tm == nil {
		t.Fatal("the whole block was discarded over one bad field")
	}
	if tm.SelectionMs != nil {
		t.Errorf("selection_ms = %d, want absent", *tm.SelectionMs)
	}
	if tm.PreparationMs == nil || *tm.PreparationMs != 1104 {
		t.Errorf("preparation_ms = %v, want 1104", tm.PreparationMs)
	}
	if tm.TotalMs == nil || *tm.TotalMs != 5775 {
		t.Errorf("total_ms = %v, want 5775", tm.TotalMs)
	}
}

// ptr is a local helper for the pointer-valued expectations above.
func ptr(v int) *int { return &v }

// A retried call makes N separate backend requests, each with its own clock. The
// result must carry the WINNING attempt's telemetry — reporting a dead attempt's
// numbers next to the answer the user actually got would be worse than reporting none.
// This exercises the real retry loop rather than a fake invoking OnRetry.
func TestRespondStreamReturnsTheWinningAttemptsTelemetry(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			// Retriable, and terminal before any content — so the loop replays it.
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":"upstream_unavailable","message":"try again"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, timingsSSE(`{"finish_reason":"stop","usage":{},`+
			`"timings":{"preparation_ms":7,"total_ms":4242}}`))
	}))
	t.Cleanup(srv.Close)

	c := NewClient(ClientConfig{BaseURL: srv.URL, Retry: RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond}})
	res, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2 (the first must have been retried)", attempts)
	}
	if res.Timings == nil || res.Timings.TotalMs == nil || *res.Timings.TotalMs != 4242 {
		t.Fatalf("timings = %+v, want the SECOND attempt's total_ms 4242", res.Timings)
	}
	// The 503 attempt reached the wire too, so the marks must describe the attempt that
	// actually answered — the second one, on a pooled connection.
	if res.Transport == nil || res.Transport.FirstByteMs == nil {
		t.Fatalf("transport = %+v, want the winning attempt's marks", res.Transport)
	}
	if res.Transport.Reused == nil || !*res.Transport.Reused {
		t.Error("the winning attempt reused the pooled connection; the marks say otherwise")
	}
}

// An empty block and an explicit null are the same fact as no block at all: nothing was
// measured. Neither may produce a timing key.
func TestTimingsEmptyAndNullBlocksAreUnmeasured(t *testing.T) {
	for name, timings := range map[string]string{"empty": `{}`, "null": `null`} {
		t.Run(name, func(t *testing.T) {
			res := streamOnce(t, timingsSSE(`{"finish_reason":"stop","usage":{},"timings":`+timings+`}`))
			if res.Timings.Any() {
				t.Fatalf("timings = %+v, want nothing measured", res.Timings)
			}
		})
	}
}

// The non-streaming body carries the same block. It is not the CLI's normal path, but a
// contract that decodes on one path and silently drops on the other is the kind of gap
// that only shows up when someone is already debugging something else.
func TestRespondResponseDecodesTimings(t *testing.T) {
	var out RespondResponse
	body := `{"protocol_version":2,"request_id":"r","model":"m","message":{"role":"assistant","content":"hi"},` +
		`"finish_reason":"stop","usage":{},"timings":{"preparation_ms":10,"total_ms":99}}`
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Timings == nil || out.Timings.TotalMs == nil || *out.Timings.TotalMs != 99 {
		t.Fatalf("timings = %+v, want total_ms 99", out.Timings)
	}
	// Non-streaming reports no upstream open: one await covers opening AND generating,
	// so splitting it would be a guess presented as a measurement.
	if out.Timings.UpstreamOpenMs != nil {
		t.Errorf("upstream_open_ms = %d on a non-streamed call", *out.Timings.UpstreamOpenMs)
	}
}
