package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// truncationSink captures every ToolResultEvent so the tests can assert on the exact
// error envelope runToolBatch produced for each call in a batch.
type truncationSink struct {
	NoopEventSink
	results []ToolResultEvent
}

func (s *truncationSink) ToolResult(ev ToolResultEvent) {
	s.results = append(s.results, ev)
}

// truncatedArgs is a real amputation shape: the stream died mid-string inside the
// final call's arguments (observed 2026-07-05/06 — agentTask.spawnForEdits batches
// whose long taskPrompt blew the output-token cap, finish_reason "length").
const truncatedArgs = `{"agentId": "codex", "taskPrompt": "You are Codex, a contestant in`

// TestTruncatedFinalCallDiagnosedAsOutputCap proves a parse-failed FINAL call in a
// finish_reason=length round returns TOOL_ARGS_TRUNCATED (recoverable, never
// dispatched) while its complete sibling still executes normally.
func TestTruncatedFinalCallDiagnosedAsOutputCap(t *testing.T) {
	sink := &truncationSink{}
	tools := &fakeTools{result: domain.Ok("spawned", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "length",
			ToolCalls: []models.ToolCallRequest{
				toolCall("c1", "agentTask__spawnForEdits", `{"agentId":"grok"}`),
				toolCall("c2", "agentTask__spawnForEdits", truncatedArgs),
			},
		},
		{Content: "recovered"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if tools.dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (complete sibling runs, truncated call never reaches Dispatch)", tools.dispatched)
	}
	if len(sink.results) != 2 {
		t.Fatalf("tool results = %d, want 2 (every call still gets a structurally-valid reply)", len(sink.results))
	}
	if !sink.results[0].Result.Ok {
		t.Fatalf("complete sibling result not ok: %+v", sink.results[0].Result)
	}
	res := sink.results[1].Result
	if res.Ok || res.Error == nil {
		t.Fatalf("truncated call result = %+v, want a failure with an error envelope", res)
	}
	if res.Error.Code != "TOOL_ARGS_TRUNCATED" {
		t.Fatalf("error code = %q, want TOOL_ARGS_TRUNCATED", res.Error.Code)
	}
	if !res.Error.Recoverable {
		t.Fatal("truncation must be recoverable — the model re-issues the amputated work")
	}
	if !strings.Contains(res.Error.Message, "output-token limit") {
		t.Fatalf("message %q does not tell the model WHY the args were cut off", res.Error.Message)
	}
}

// TestEmptyArgsFinalCallInLengthRoundDiagnosedAsTruncation proves the OTHER amputation
// shape: the cap severs the stream after the function name but before the first argument
// byte, so build() materializes empty "{}" args that parse cleanly. Left alone they would
// dispatch on empty input; instead an effectively-empty FINAL call in a length round is
// diagnosed as TOOL_ARGS_TRUNCATED (never dispatched), and the tool.args.truncated gap
// event is emitted since the rejection never reaches Dispatch's audit path.
func TestEmptyArgsFinalCallInLengthRoundDiagnosedAsTruncation(t *testing.T) {
	sink := &truncationSink{}
	cap := &traceCapture{}
	tools := &fakeTools{result: domain.Ok("spawned", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "length",
			ToolCalls: []models.ToolCallRequest{
				toolCall("c1", "agentTask__spawnForEdits", `{"agentId":"grok"}`),
				toolCall("c2", "agentTask__spawnForEdits", `{}`), // cut before any arg byte
			},
		},
		{Content: "recovered"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	deps.Trace = cap.record
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if tools.dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (complete sibling runs, empty-args final call never reaches Dispatch)", tools.dispatched)
	}
	if len(sink.results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(sink.results))
	}
	res := sink.results[1].Result
	if res.Ok || res.Error == nil || res.Error.Code != "TOOL_ARGS_TRUNCATED" {
		t.Fatalf("empty-args final call = %+v, want TOOL_ARGS_TRUNCATED (not a bare {} dispatch)", res)
	}
	ev, ok := cap.first("tool.args.truncated")
	if !ok {
		t.Fatal("expected a tool.args.truncated trace event (the rejection never reaches Dispatch's audit)")
	}
	if ev.fields["toolCallId"] != "c2" {
		t.Errorf("toolCallId = %v, want c2", ev.fields["toolCallId"])
	}
}

// TestValidFinalCallInLengthRoundDispatchesNormally guards the other direction: a
// COMPLETE, non-empty final call must still run even when the round finished with
// "length" (the cap fell after the call, not inside it). No false truncation, and no
// tool.args.truncated trace.
func TestValidFinalCallInLengthRoundDispatchesNormally(t *testing.T) {
	sink := &truncationSink{}
	cap := &traceCapture{}
	tools := &fakeTools{result: domain.Ok("read", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "length",
			ToolCalls:    []models.ToolCallRequest{toolCall("c1", "fs__read", `{"path":"a.txt"}`)},
		},
		{Content: "done"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	deps.Trace = cap.record
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if tools.dispatched != 1 {
		t.Fatalf("dispatched = %d, want 1 (a complete final call runs even in a length round)", tools.dispatched)
	}
	if len(sink.results) != 1 || !sink.results[0].Result.Ok {
		t.Fatalf("result = %+v, want a single ok dispatch", sink.results)
	}
	if _, ok := cap.first("tool.args.truncated"); ok {
		t.Fatal("a complete non-empty final call must not be flagged as truncated")
	}
}

// TestInvalidNonFinalAndTruncatedFinalInOneBatch exercises the two failure shapes
// together: a length round whose non-final call is a genuine syntax slip and whose
// final call is amputated to empty. Each gets its own diagnosis and neither dispatches.
func TestInvalidNonFinalAndTruncatedFinalInOneBatch(t *testing.T) {
	sink := &truncationSink{}
	tools := &fakeTools{result: domain.Ok("unused", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "length",
			ToolCalls: []models.ToolCallRequest{
				toolCall("c1", "fs__read", `{not json`),          // non-final syntax slip
				toolCall("c2", "agentTask__spawnForEdits", `{}`), // final, amputated to empty
			},
		},
		{Content: "recovered"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if tools.dispatched != 0 {
		t.Fatalf("dispatched = %d, want 0 (both calls are rejected before Dispatch)", tools.dispatched)
	}
	if len(sink.results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(sink.results))
	}
	if got := sink.results[0].Result.Error; got == nil || got.Code != "INVALID_TOOL_ARGS_JSON" {
		t.Fatalf("non-final = %+v, want INVALID_TOOL_ARGS_JSON", got)
	}
	if got := sink.results[1].Result.Error; got == nil || got.Code != "TOOL_ARGS_TRUNCATED" {
		t.Fatalf("final = %+v, want TOOL_ARGS_TRUNCATED", got)
	}
}

// TestInvalidJSONWithoutLengthKeepsSyntaxDiagnosis proves a parse failure in a round
// that did NOT hit the output cap stays INVALID_TOOL_ARGS_JSON and now carries the
// decoder's own error detail so the model can fix the actual syntax slip.
func TestInvalidJSONWithoutLengthKeepsSyntaxDiagnosis(t *testing.T) {
	sink := &truncationSink{}
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "tool_calls",
			ToolCalls: []models.ToolCallRequest{
				toolCall("c1", "fs__read", `{"path": 'a.txt'}`), // single quotes — a real syntax slip
			},
		},
		{Content: "recovered"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if tools.dispatched != 0 {
		t.Fatalf("dispatched = %d, want 0", tools.dispatched)
	}
	if len(sink.results) != 1 {
		t.Fatalf("tool results = %d, want 1", len(sink.results))
	}
	res := sink.results[0].Result
	if res.Ok || res.Error == nil || res.Error.Code != "INVALID_TOOL_ARGS_JSON" {
		t.Fatalf("result = %+v, want INVALID_TOOL_ARGS_JSON failure", res)
	}
	if !strings.Contains(res.Error.Message, "invalid character") {
		t.Fatalf("message %q does not surface the decoder's parse error", res.Error.Message)
	}
}

// TestInvalidJSONOnNonFinalCallInLengthRound proves the truncation diagnosis is
// reserved for the batch's FINAL call: the output cap amputates the stream at one
// point, so an earlier unparseable call is a genuine syntax error even when the
// round finished with "length".
func TestInvalidJSONOnNonFinalCallInLengthRound(t *testing.T) {
	sink := &truncationSink{}
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	r := &fakeRouter{results: []models.ChatResult{
		{
			FinishReason: "length",
			ToolCalls: []models.ToolCallRequest{
				toolCall("c1", "fs__read", `{not json`),
				toolCall("c2", "fs__read", `{"path":"b"}`),
			},
		},
		{Content: "recovered"},
	}}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(sink.results) != 2 {
		t.Fatalf("tool results = %d, want 2", len(sink.results))
	}
	if got := sink.results[0].Result.Error; got == nil || got.Code != "INVALID_TOOL_ARGS_JSON" {
		t.Fatalf("non-final parse failure = %+v, want INVALID_TOOL_ARGS_JSON (not truncation)", got)
	}
	if !sink.results[1].Result.Ok {
		t.Fatalf("final complete call result not ok: %+v", sink.results[1].Result)
	}
}
