package models

import (
	"encoding/json"
	"strings"
	"testing"
)

// hashString must match the JS string-hash bit-for-bit (synthesized tool-call ids
// appear in transcripts). Expected values verified against the TS implementation.
func TestHashStringBitExact(t *testing.T) {
	cases := []struct {
		in   string
		want int32
		abs  int
	}{
		{"fs.read", -625574377, 625574377},
		{"agentTask.spawnForEdits", -978115529, 978115529},
		{"", 0, 0},
		{"a", 97, 97},
		{"héllo", 103094734, 103094734},  // multi-byte rune (charCodeAt code units)
		{"𝟘abc", 1283596095, 1283596095}, // non-BMP rune → surrogate pair
		{"tool{}", -868060422, 868060422},
	}
	for _, c := range cases {
		if got := hashString(c.in); got != c.want {
			t.Errorf("hashString(%q) = %d, want %d", c.in, got, c.want)
		}
		if got := absInt(hashString(c.in)); got != c.abs {
			t.Errorf("abs hashString(%q) = %d, want %d", c.in, got, c.abs)
		}
	}
}

// toWireMessages must drop the internal `name` on tool messages, emit assistant
// tool_calls as {id,type,function} only, and omit absent optional keys (no null).
func TestToWireMessagesOmission(t *testing.T) {
	msgs := []ChatMessage{
		{Role: "system", StringContent: "sys"},
		{Role: "user", StringContent: "hi"},
		{Role: "assistant", ContentNull: true, ToolCalls: []ToolCallRequest{
			{ID: "c1", Type: "function", Function: ToolCallFunction{Name: "doit", Arguments: `{"x":1}`}},
		}},
		{Role: "tool", StringContent: "result", ToolCallID: "c1", Name: "doit"},
	}
	wm, err := toWireMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wm)
	s := string(b)

	// The internal helper `name` field must never appear on the wire.
	if strings.Contains(s, `"name":"doit"`) && strings.Contains(s, `"role":"tool"`) {
		// name may appear inside function{name:...}; ensure it's not a top-level
		// tool-message field by checking the tool object shape directly.
	}
	// tool message: only role/content/tool_call_id.
	if !strings.Contains(s, `{"role":"tool","content":"result","tool_call_id":"c1"}`) {
		t.Errorf("tool wire msg malformed: %s", s)
	}
	// assistant tool-call turn: content explicit null, tool_calls present.
	if !strings.Contains(s, `"role":"assistant","content":null`) {
		t.Errorf("assistant null content missing: %s", s)
	}
	if !strings.Contains(s, `"tool_calls":[{"id":"c1","type":"function","function":{"name":"doit","arguments":"{\"x\":1}"}}]`) {
		t.Errorf("assistant tool_calls malformed: %s", s)
	}
	// user/system without tool_calls must omit the key (no "tool_calls":null).
	if strings.Contains(s, `"tool_calls":null`) {
		t.Errorf("null tool_calls leaked: %s", s)
	}
}

// A multimodal user message forwards its parts verbatim (array preserved).
func TestToWireMessagesMultimodal(t *testing.T) {
	msgs := []ChatMessage{{
		Role: "user",
		Parts: []ChatContentPart{
			TextPart("look:"),
			ImageDataPart("QUJD", "image/png"),
		},
	}}
	wm, err := toWireMessages(msgs)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(wm)
	s := string(b)
	if !strings.Contains(s, `"content":[{"type":"text","text":"look:"},{"type":"image_url","image_url":{"url":"data:image/png;base64,QUJD"}}]`) {
		t.Errorf("multimodal wire = %s", s)
	}
	// The image part must NOT carry a `detail` field.
	if strings.Contains(s, `"detail"`) {
		t.Errorf("detail leaked: %s", s)
	}
}

func TestExtractJson(t *testing.T) {
	cases := map[string]string{
		`{"a":1}`:                    `{"a":1}`,
		`prose before {"a":1} after`: `{"a":1}`,
		`{"s":"}"}trailing`:          `{"s":"}"}`,            // brace inside string ignored
		`{"s":"\"} still in"}`:       `{"s":"\"} still in"}`, // escaped quote
		`[1,2,{"x":3}]xxx`:           `[1,2,{"x":3}]`,
		`no json here`:               `no json here`,
		`{"unbalanced":`:             `{"unbalanced":`, // returns from first bracket
	}
	for in, want := range cases {
		if got := ExtractJson(in); got != want {
			t.Errorf("ExtractJson(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestContentToTextImageOmitted(t *testing.T) {
	m := ChatMessage{Role: "user", Parts: []ChatContentPart{
		TextPart("a"), ImageDataPart("ZZZ", ""), TextPart("b"),
	}}
	if got := m.ContentToText(); got != "a\n[image omitted]\nb" {
		t.Fatalf("ContentToText = %q", got)
	}
}

func TestNormalizeToolCalls(t *testing.T) {
	calls := []rawToolCall{
		{Function: &rawToolCallFunc{Name: "keep", Arguments: ""}}, // no id, empty args → "{}"
		{Function: &rawToolCallFunc{Name: ""}},                    // dropped (no name)
		{ID: "given", Function: &rawToolCallFunc{Name: "n2", Arguments: `{"y":2}`}},
	}
	out := normalizeToolCalls(calls)
	if len(out) != 2 {
		t.Fatalf("got %d, want 2", len(out))
	}
	if out[0].Function.Arguments != "{}" || out[0].ID == "" {
		t.Errorf("call0 = %+v", out[0])
	}
	if out[1].ID != "given" || out[1].Function.Arguments != `{"y":2}` {
		t.Errorf("call1 = %+v", out[1])
	}
}
