package jsonout

import (
	"bytes"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Additional sink behavior: a failed tool call still exits 0 (recoverable
// model context), a cancelled turn drops its streamed buffer, an error event flushes
// buffered prose first, an unserializable tool error degrades the line (not throw),
// and events emitted after finish are dropped.

func types(lines []map[string]any) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if s, ok := l["type"].(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestFailedToolCallStillExitsZero: a tool:result with ok=false carries the
// structured error but the turn ends via assistant:end, so the run exits 0.
func TestFailedToolCallStillExitsZero(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantStart()
	s.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "git.push"})
	s.ToolResult(agent.ToolResultEvent{
		ID: "c1", Name: "git.push",
		Result: domain.Fail("git_rejected", "non-fast-forward"),
	})
	s.AssistantStart()
	s.AssistantEnd("could not push, will retry", "")
	code := s.Finish()

	if code != domain.OneShotExitCode.Success {
		t.Fatalf("failed-tool exit = %d, want 0", code)
	}
	lines := decodeLines(t, &buf)
	var tr map[string]any
	for _, l := range lines {
		if l["type"] == "tool:result" {
			tr = l
		}
	}
	if tr == nil {
		t.Fatal("no tool:result line")
	}
	if ok, _ := tr["ok"].(bool); ok {
		t.Fatal("failed tool:result must carry ok=false")
	}
	if tr["error"] == nil {
		t.Fatal("failed tool:result must carry a structured error")
	}
	if last := lines[len(lines)-1]; last["status"] != string(domain.JSONStatusSuccess) {
		t.Fatalf("result status = %v, want success", last["status"])
	}
}

// TestCancelledDropsStreamedBuffer: streamed tokens before a cancel must NOT surface
// as an assistant:content line (content is authoritative; the aborted round is dropped).
func TestCancelledDropsStreamedBuffer(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantStart()
	s.AssistantToken("interrupt")
	s.AssistantCancelled("")
	s.Finish()

	got := types(decodeLines(t, &buf))
	want := []string{"assistant:start", "assistant:cancelled", "result"}
	if len(got) != len(want) {
		t.Fatalf("cancelled lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("cancelled lines = %v, want %v", got, want)
		}
	}
}

// TestErrorFlushesBufferedProse: a partial streamed thought is flushed as
// assistant:content BEFORE the error line (don't lose it on a stream death).
func TestErrorFlushesBufferedProse(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantStart()
	s.AssistantToken("partial thought")
	s.Error("stream died")
	s.Finish()

	lines := decodeLines(t, &buf)
	got := types(lines)
	want := []string{"assistant:start", "assistant:content", "error", "result"}
	if len(got) != len(want) {
		t.Fatalf("lines = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("lines = %v, want %v", got, want)
		}
	}
	if lines[1]["content"] != "partial thought" {
		t.Fatalf("flushed content = %v", lines[1]["content"])
	}
	if last := lines[len(lines)-1]; last["status"] != string(domain.JSONStatusError) {
		t.Fatalf("error result status = %v", last["status"])
	}
}

// TestUnserializableToolErrorDegrades: a tool error detail that can't be JSON-
// encoded must degrade the line (serializationError:true) keeping the seq, never
// throw or drop, and the run still finishes success.
func TestUnserializableToolErrorDegrades(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantStart()
	s.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "t"})
	// A channel value is not JSON-serializable; the emit must degrade, not panic.
	s.ToolResult(agent.ToolResultEvent{
		ID: "c1", Name: "t",
		Result: domain.Fail("x", "m", domain.WithDetails(map[string]any{"bad": make(chan int)})),
	})
	s.AssistantStart()
	s.AssistantEnd("ok", "")
	s.Finish()

	lines := decodeLines(t, &buf)
	// Contiguous seq across all lines (no gap from the degraded line).
	for i, l := range lines {
		if int(l["seq"].(float64)) != i {
			t.Fatalf("seq gap at line %d: %v", i, l["seq"])
		}
	}
	var degraded map[string]any
	for _, l := range lines {
		if l["type"] == "tool:result" {
			degraded = l
		}
	}
	if degraded == nil {
		t.Fatal("degraded tool:result line missing entirely")
	}
	if se, _ := degraded["serializationError"].(bool); !se {
		t.Fatalf("degraded line not flagged serializationError: %v", degraded)
	}
	if last := lines[len(lines)-1]; last["status"] != string(domain.JSONStatusSuccess) {
		t.Fatalf("result status = %v, want success", last["status"])
	}
}

// TestEventsAfterFinishDropped: nothing may follow the terminal result line.
func TestEventsAfterFinishDropped(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantStart()
	s.AssistantEnd("hi", "")
	s.Finish()
	s.Info("late")
	s.ToolCall(agent.ToolCallEvent{ID: "c9", Name: "t"})

	lines := decodeLines(t, &buf)
	if last := lines[len(lines)-1]; last["type"] != "result" {
		t.Fatalf("last line = %v, want result", last["type"])
	}
	resultCount := 0
	for _, l := range lines {
		if l["type"] == "result" {
			resultCount++
		}
		if l["type"] == "info" {
			t.Fatal("info line emitted after finish")
		}
	}
	if resultCount != 1 {
		t.Fatalf("result lines = %d, want 1", resultCount)
	}
}
