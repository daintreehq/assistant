package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// An assistant turn's reasoning_content (DeepSeek thinking mode) is captured from the
// backend response and replayed verbatim on the wire — DeepSeek 400s if a tool-call
// turn's reasoning is dropped. A turn without reasoning omits the field entirely so the
// default thinking-off posture is byte-identical to before.
func TestReasoningContent_CaptureAndReplay(t *testing.T) {
	// Capture: a backend response message carries reasoning onto the local message.
	got := backendAssistantMessage(backend.RespondMessage{
		Content:          "the answer",
		ReasoningContent: "step-by-step thinking",
		ToolCalls:        []backend.ToolCall{{ID: "call_1", Type: "function", Function: backend.FunctionCall{Name: "git__status", Arguments: "{}"}}},
	})
	if got.ReasoningContent != "step-by-step thinking" {
		t.Fatalf("capture: reasoning = %q, want %q", got.ReasoningContent, "step-by-step thinking")
	}

	// Replay: the assistant message echoes reasoning_content on the wire; a user message
	// (and an assistant turn without reasoning) carry none.
	out, err := toBackendMessages([]models.ChatMessage{
		models.TextMessage("user", "hi"),
		got,
		{Role: "assistant", StringContent: "plain reply, no thinking"},
	})
	if err != nil {
		t.Fatalf("toBackendMessages: %v", err)
	}
	if out[0].ReasoningContent != "" {
		t.Errorf("user message must not carry reasoning, got %q", out[0].ReasoningContent)
	}
	if out[1].ReasoningContent != "step-by-step thinking" {
		t.Errorf("assistant replay reasoning = %q, want %q", out[1].ReasoningContent, "step-by-step thinking")
	}
	if out[2].ReasoningContent != "" {
		t.Errorf("a no-thinking assistant turn must omit reasoning, got %q", out[2].ReasoningContent)
	}
}

func TestToBackendMessages_RejectsSystemAndDeveloper(t *testing.T) {
	for _, role := range []string{"system", "developer"} {
		_, err := toBackendMessages([]models.ChatMessage{models.TextMessage(role, "x")})
		if err == nil {
			t.Errorf("role %q: expected rejection, got nil", role)
		}
	}
}

func TestToBackendMessages_RolesAndContent(t *testing.T) {
	msgs := []models.ChatMessage{
		models.TextMessage("user", "hello"),
		{Role: "assistant", ContentNull: true, ToolCalls: []models.ToolCallRequest{
			{ID: "call_1", Type: "function", Function: models.ToolCallFunction{Name: "git__status", Arguments: ""}},
		}},
		{Role: "tool", ToolCallID: "call_1", StringContent: `{"ok":true}`},
	}
	out, err := toBackendMessages(msgs)
	if err != nil {
		t.Fatalf("toBackendMessages: %v", err)
	}
	if len(out) != 3 {
		t.Fatalf("len = %d", len(out))
	}
	// user
	if out[0].Role != "user" || string(out[0].Content) != `"hello"` {
		t.Errorf("user msg = %+v", out[0])
	}
	// assistant tool-call turn: explicit null content + coerced empty args
	if out[1].Role != "assistant" || string(out[1].Content) != "null" {
		t.Errorf("assistant content = %q, want null", string(out[1].Content))
	}
	if len(out[1].ToolCalls) != 1 || out[1].ToolCalls[0].Function.Arguments != "{}" {
		t.Errorf("assistant tool call = %+v", out[1].ToolCalls)
	}
	// tool result content is a JSON string
	if out[2].Role != "tool" || out[2].ToolCallID != "call_1" {
		t.Errorf("tool msg = %+v", out[2])
	}
	var s string
	if err := json.Unmarshal(out[2].Content, &s); err != nil || !strings.Contains(s, `"ok":true`) {
		t.Errorf("tool content = %q (err %v)", string(out[2].Content), err)
	}
}

func TestToBackendMessages_Multimodal(t *testing.T) {
	msgs := []models.ChatMessage{
		{Role: "user", Parts: []models.ChatContentPart{
			models.TextPart("look:"),
			models.ImageDataPart("aGk=", "image/png"),
		}},
	}
	out, err := toBackendMessages(msgs)
	if err != nil {
		t.Fatalf("toBackendMessages: %v", err)
	}
	var parts []map[string]any
	if err := json.Unmarshal(out[0].Content, &parts); err != nil {
		t.Fatalf("content is not an array: %v (%s)", err, string(out[0].Content))
	}
	if len(parts) != 2 || parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Errorf("parts = %+v", parts)
	}
}

func TestCoerceToolArgs(t *testing.T) {
	cases := map[string]string{
		"":             "{}",
		"   ":          "{}",
		"not json":     "{}",
		"null":         "{}",
		"[1,2]":        "{}",
		`{"a":1}`:      `{"a":1}`,
		`{"path":"x"}`: `{"path":"x"}`,
	}
	for in, want := range cases {
		if got := coerceToolArgs(in); got != want {
			t.Errorf("coerceToolArgs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestValidateBackendTools(t *testing.T) {
	bad := [][]backend.Tool{
		{{Function: backend.FunctionDef{Name: "skill__find"}}},
		{{Function: backend.FunctionDef{Name: "skill__load"}}},
		{{Function: backend.FunctionDef{Name: "skill.find"}}},
		{{Function: backend.FunctionDef{Name: "daintree_internal__x"}}},
		{{Function: backend.FunctionDef{Name: "fs.read"}}}, // dotted name not wire-safe
		{{Function: backend.FunctionDef{Name: ""}}},
		{{Function: backend.FunctionDef{Name: strings.Repeat("x", 65)}}},
		{{Function: backend.FunctionDef{Name: "fs__réad"}}},
		// Over the backend's 8192-char description max_length (it 422s, never truncates).
		{{Function: backend.FunctionDef{Name: "fs__read", Description: strings.Repeat("x", 8193)}}},
	}
	for _, tools := range bad {
		if err := validateBackendTools(tools); err == nil {
			t.Errorf("expected rejection for %q", tools[0].Function.Name)
		}
	}
	good := []backend.Tool{
		{Function: backend.FunctionDef{Name: "fs__read"}},
		{Function: backend.FunctionDef{Name: "git__status"}},
		{Function: backend.FunctionDef{Name: "skill__step__advance"}},
		{Function: backend.FunctionDef{Name: "terminal__run"}},
	}
	if err := validateBackendTools(good); err != nil {
		t.Errorf("unexpected rejection: %v", err)
	}
}

func TestToBackendTools_Conversion(t *testing.T) {
	in := []models.ChatTool{
		{Type: "function", Function: models.ChatToolFunc{
			Name:        "fs__read",
			Description: "read a file",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
	}
	out, err := toBackendTools(in)
	if err != nil {
		t.Fatalf("toBackendTools: %v", err)
	}
	if len(out) != 1 || out[0].Function.Name != "fs__read" || out[0].Type != "function" {
		t.Errorf("converted = %+v", out)
	}
	if string(out[0].Function.Parameters) == "" {
		t.Errorf("parameters dropped")
	}
}
