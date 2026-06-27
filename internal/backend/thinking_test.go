package backend

import (
	"strings"
	"testing"
)

// A DeepSeek thinking-mode stream interleaves a one-shot `status` event, then
// reasoning_content deltas (chain-of-thought), then content deltas (the answer).
// The parser must accumulate reasoning into the final message (for verbatim replay)
// and surface the status + reasoning callbacks for UX.
func TestParseRespondStreamThinking(t *testing.T) {
	stream := strings.Join([]string{
		"event: meta",
		"data: {}",
		"",
		"event: status",
		`data: {"phase":"thinking"}`,
		"",
		"event: delta",
		`data: {"reasoning_content":"let me "}`,
		"",
		"event: delta",
		`data: {"reasoning_content":"think"}`,
		"",
		"event: delta",
		`data: {"content":"answer"}`,
		"",
		"event: done",
		`data: {"finish_reason":"stop","usage":{}}`,
		"",
		"",
	}, "\n")

	var reasoning, content strings.Builder
	var phases []string
	res, err := parseRespondStream(strings.NewReader(stream), StreamCallbacks{
		OnContent:   func(s string) { content.WriteString(s) },
		OnReasoning: func(s string) { reasoning.WriteString(s) },
		OnStatus:    func(st StreamStatus) { phases = append(phases, st.Phase) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Message.ReasoningContent != "let me think" {
		t.Errorf("final reasoning = %q, want %q", res.Message.ReasoningContent, "let me think")
	}
	if res.Message.Content != "answer" {
		t.Errorf("final content = %q, want %q", res.Message.Content, "answer")
	}
	if reasoning.String() != "let me think" {
		t.Errorf("OnReasoning accumulated %q, want %q", reasoning.String(), "let me think")
	}
	if len(phases) != 1 || phases[0] != "thinking" {
		t.Errorf("OnStatus phases = %v, want [thinking]", phases)
	}
	if res.FinishReason != "stop" {
		t.Errorf("finish = %q, want stop", res.FinishReason)
	}
}

// A non-thinking stream (the default posture) carries no status/reasoning, decodes
// exactly as before, and yields an empty ReasoningContent — proving the additions
// are inert when thinking is off.
func TestParseRespondStreamNoThinkingIsInert(t *testing.T) {
	stream := strings.Join([]string{
		"event: meta", "data: {}", "",
		"event: delta", `data: {"content":"hi"}`, "",
		"event: done", `data: {"finish_reason":"stop","usage":{}}`, "", "",
	}, "\n")

	res, err := parseRespondStream(strings.NewReader(stream), StreamCallbacks{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Message.ReasoningContent != "" {
		t.Errorf("reasoning should be empty with no thinking, got %q", res.Message.ReasoningContent)
	}
	if res.Message.Content != "hi" {
		t.Errorf("content = %q, want hi", res.Message.Content)
	}
}
