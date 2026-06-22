package jsonout

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// fixedClock returns a constant ts for deterministic lines.
func fixedClock() int64 { return 1234 }

// decodeLines parses each JSONL line into a generic map.
func decodeLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	sc := bufio.NewScanner(buf)
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("line is not valid JSON: %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

func TestSeqMonotonicAndTerminalEnvelope(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)

	s.AssistantStart()
	s.AssistantToken("hel")
	s.AssistantToken("lo")
	s.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "fs.read", Args: `{"path":"x"}`})
	s.ToolResult(agent.ToolResultEvent{ID: "c1", Name: "fs.read", Result: domain.Ok("ok", nil)})
	s.AssistantEnd("final answer", "")
	code := s.Finish()

	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit code = %d, want %d", code, domain.OneShotExitCode.Success)
	}

	lines := decodeLines(t, &buf)
	// seq must be 0,1,2,... with no gaps.
	for i, l := range lines {
		seq, ok := l["seq"].(float64)
		if !ok {
			t.Fatalf("line %d missing seq", i)
		}
		if int(seq) != i {
			t.Fatalf("seq gap: line %d has seq %v", i, seq)
		}
		if int64(l["ts"].(float64)) != fixedClock() {
			t.Fatalf("line %d ts not from injected clock", i)
		}
	}

	// The buffered tokens must surface as an assistant:content line before tool:call.
	gotContent := false
	for _, l := range lines {
		if l["type"] == "assistant:content" && l["content"] == "hello" {
			gotContent = true
		}
	}
	if !gotContent {
		t.Fatalf("missing flushed assistant:content=hello; lines=%v", lines)
	}

	// Last line is the terminal result envelope.
	last := lines[len(lines)-1]
	if last["type"] != "result" {
		t.Fatalf("last line type = %v, want result", last["type"])
	}
	if last["status"] != string(domain.JSONStatusSuccess) {
		t.Fatalf("result status = %v, want success", last["status"])
	}
	if int(last["schemaVersion"].(float64)) != domain.JSONOutputSchemaVersion {
		t.Fatalf("schemaVersion = %v", last["schemaVersion"])
	}
	if last["error"] != nil {
		t.Fatalf("success result error must be null, got %v", last["error"])
	}
}

func TestErrorExitCodeAndNoLineAfterFinish(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Error("boom")
	code := s.Finish()
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit = %d, want %d", code, domain.OneShotExitCode.Error)
	}
	before := buf.Len()
	// Emissions after finish are dropped.
	s.Info("late")
	if buf.Len() != before {
		t.Fatalf("a line was emitted after Finish")
	}
	// Finish is idempotent.
	if s.Finish() != domain.OneShotExitCode.Error {
		t.Fatalf("Finish not idempotent")
	}
}

func TestCancelledExitCode(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.AssistantCancelled("partial")
	if code := s.Finish(); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
}

func TestDefaultStatusIsErrorWithNoTerminalEvent(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	// No terminal event at all → status error, exit 1.
	if code := s.Finish(); code != domain.OneShotExitCode.Error {
		t.Fatalf("default exit = %d, want %d", code, domain.OneShotExitCode.Error)
	}
}
