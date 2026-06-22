package models

import (
	"strings"
	"testing"
)

// parseSSE must accumulate fragmented tool-call argument deltas by index, tolerate
// CRLF line endings, capture the usage-only final chunk, and stop on [DONE].
func TestParseSSEFragmentedToolCalls(t *testing.T) {
	// Tool call arguments arrive in fragments across chunks; the id+name land once.
	// Usage rides the final usage-only chunk (empty choices). CRLF line endings.
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"Hel"}}]}`,
		`data: {"choices":[{"delta":{"content":"lo"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x","function":{"name":"doit","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`,
		`data: [DONE]`,
		``,
	}, "\r\n")

	filter := &ThinkFilter{}
	toolAcc := map[int]*toolAccEntry{}
	finishReason := "stop"
	var usage *Usage
	var emitted []string

	err := parseSSE(strings.NewReader(stream), func(chunk *streamChunk) {
		if chunk.Usage != nil {
			usage = chunk.Usage.toUsage()
		}
		if len(chunk.Choices) == 0 {
			return
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != nil && *ch.Delta.Content != "" {
			if v := filter.Push(*ch.Delta.Content); v != "" {
				emitted = append(emitted, v)
			}
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := 0
			if tc.Index != nil {
				idx = *tc.Index
			}
			cur := toolAcc[idx]
			if cur == nil {
				cur = &toolAccEntry{}
				toolAcc[idx] = cur
			}
			if tc.ID != "" {
				cur.id = tc.ID
			}
			if tc.Function != nil {
				if tc.Function.Name != "" {
					cur.name = tc.Function.Name
				}
				cur.args += tc.Function.Arguments
			}
		}
		if ch.FinishReason != nil && *ch.FinishReason != "" {
			finishReason = *ch.FinishReason
		}
	})
	if err != nil {
		t.Fatalf("parseSSE error: %v", err)
	}
	filter.End()

	if got := filter.Visible(); got != "Hello" {
		t.Fatalf("visible = %q", got)
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finishReason = %q", finishReason)
	}
	calls := buildStreamToolCalls(toolAcc)
	if len(calls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(calls))
	}
	if calls[0].ID != "call_x" || calls[0].Function.Name != "doit" || calls[0].Function.Arguments != `{"a":1}` {
		t.Fatalf("tool call = %+v", calls[0])
	}
	if usage == nil || usage.PromptTokens == nil || *usage.PromptTokens != 10 ||
		usage.CachedTokens == nil || *usage.CachedTokens != 4 {
		t.Fatalf("usage = %+v", usage)
	}
}

// A tool call with no id synthesizes one from hashString(name+args).
func TestBuildStreamToolCallsSyntheticID(t *testing.T) {
	acc := map[int]*toolAccEntry{0: {name: "tool", args: "{}"}}
	calls := buildStreamToolCalls(acc)
	if len(calls) != 1 {
		t.Fatalf("want 1 call")
	}
	// hashString("tool{}") = -868060422 → abs 868060422 (verified against the TS).
	if calls[0].ID != "call_868060422" {
		t.Fatalf("synthetic id = %q, want call_868060422", calls[0].ID)
	}
}

// An entry that never received a function name is dropped.
func TestBuildStreamToolCallsDropsNameless(t *testing.T) {
	acc := map[int]*toolAccEntry{0: {id: "x", args: "{}"}}
	if got := buildStreamToolCalls(acc); len(got) != 0 {
		t.Fatalf("want 0 calls, got %d", len(got))
	}
}

// Empty args default to "{}".
func TestBuildStreamToolCallsDefaultArgs(t *testing.T) {
	acc := map[int]*toolAccEntry{0: {id: "x", name: "n"}}
	calls := buildStreamToolCalls(acc)
	if calls[0].Function.Arguments != "{}" {
		t.Fatalf("args = %q, want {}", calls[0].Function.Arguments)
	}
}

// TestParseSSEMidStreamErrorSurfaces locks the fix for a provider error delivered
// mid-stream (after HTTP 200) as `data: {"error":{...}}` — it must surface as an error,
// not be silently swallowed into an empty "clean" completion.
func TestParseSSEMidStreamErrorSurfaces(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"par"}}]}`,
		`data: {"error":{"message":"rate limited","code":"429"}}`,
		`data: [DONE]`,
		``,
	}, "\n")
	err := parseSSE(strings.NewReader(stream), func(chunk *streamChunk) {})
	if err == nil {
		t.Fatal("a mid-stream provider error must surface, not be swallowed as empty success")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should carry the provider message, got: %v", err)
	}
}
