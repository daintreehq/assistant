package models

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

// ToolCallRequest is one tool call the model emitted (or one we replay back to it).
// arguments is a raw JSON string exactly as the wire carries it.
type ToolCallRequest struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // always "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the name + raw JSON-string arguments of a tool call.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ChatContentPart is a single multimodal content part — either text or an image
// URL. Only these two kinds are ever sent (Fireworks ignores `detail`, so it is
// intentionally omitted from the image part). A zero PartType means "unused".
type ChatContentPart struct {
	// Type is "text" or "image_url".
	Type     string
	Text     string
	ImageURL string
}

// MarshalJSON emits the exact OpenAI wire shape for a content part. text →
// {type:"text",text}; image_url → {type:"image_url",image_url:{url}} with NO
// detail field.
func (p ChatContentPart) MarshalJSON() ([]byte, error) {
	switch p.Type {
	case "image_url":
		return json.Marshal(struct {
			Type     string `json:"type"`
			ImageURL struct {
				URL string `json:"url"`
			} `json:"image_url"`
		}{
			Type: "image_url",
			ImageURL: struct {
				URL string `json:"url"`
			}{URL: p.ImageURL},
		})
	default:
		return json.Marshal(struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: "text", Text: p.Text})
	}
}

// TextPart builds a text content part.
func TextPart(text string) ChatContentPart {
	return ChatContentPart{Type: "text", Text: text}
}

// ImageDataPart builds an image content part from raw base64 image bytes, wrapped
// as the data: URI the OpenAI-compatible wire format expects. mimeType defaults to
// PNG (what Daintree's screenshot capture emits) when empty.
func ImageDataPart(base64Data, mimeType string) ChatContentPart {
	if mimeType == "" {
		mimeType = "image/png"
	}
	return ChatContentPart{Type: "image_url", ImageURL: "data:" + mimeType + ";base64," + base64Data}
}

// ChatMessage is an internal message. Content is either a plain string (StringContent)
// or a multimodal part array (Parts); when Parts is non-nil it takes precedence and
// the array is forwarded verbatim. A nil Parts with empty StringContent and the
// HasString flag false models a `null` content (an assistant tool-call turn).
type ChatMessage struct {
	Role string // "system" | "user" | "assistant" | "tool"

	// StringContent / Parts: at most one is meaningful. Parts non-nil ⇒ multimodal.
	StringContent string
	Parts         []ChatContentPart
	// ContentNull models a TS `content: null` (assistant tool-call turn with no
	// prose). When true and Parts is nil, content marshals as the JS coalesced "".
	ContentNull bool

	ToolCalls  []ToolCallRequest
	ToolCallID string
	Name       string // internal helper field — DROPPED on the wire
}

// TextMessage is a convenience constructor for a plain string-content message.
func TextMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, StringContent: content}
}

// HasImageContent reports whether any message carries an image content part. This
// drives the router's tier gate (only the large tier is vision-capable).
func HasImageContent(messages []ChatMessage) bool {
	for _, m := range messages {
		for _, part := range m.Parts {
			if part.Type == "image_url" {
				return true
			}
		}
	}
	return false
}

// ContentToText flattens a message's content to plain text for the string-only
// paths. Image parts collapse to an "[image omitted]" marker — never stringified
// to their (huge) base64 URI.
func (m ChatMessage) ContentToText() string {
	if m.Parts != nil {
		var sb strings.Builder
		for i, part := range m.Parts {
			if i > 0 {
				sb.WriteByte('\n')
			}
			if part.Type == "image_url" {
				sb.WriteString("[image omitted]")
			} else {
				sb.WriteString(part.Text)
			}
		}
		return sb.String()
	}
	return m.StringContent
}

// ChatTool is a function tool spec sent to the model.
type ChatTool struct {
	Type     string       `json:"type"` // "function"
	Function ChatToolFunc `json:"function"`
}

// ChatToolFunc is the name/description/JSON-schema parameters of a tool.
type ChatToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// emptySchema is the JSON-schema body sent for a tool that declares no parameters.
// A nil/empty json.RawMessage marshals to `"parameters":null`, which Fireworks
// rejects — an absent schema is "no arguments", i.e. an empty object.
var emptySchema = json.RawMessage(`{}`)

// normalizeTools returns a copy of tools with any nil/empty Parameters replaced by
// the empty-object schema `{}`, so a parameterless tool never marshals to a wire
// `null`. Returns the input unchanged when there's nothing to fix.
func normalizeTools(tools []ChatTool) []ChatTool {
	needsFix := false
	for _, t := range tools {
		if len(bytesTrimSpace(t.Function.Parameters)) == 0 {
			needsFix = true
			break
		}
	}
	if !needsFix {
		return tools
	}
	out := make([]ChatTool, len(tools))
	copy(out, tools)
	for i := range out {
		if len(bytesTrimSpace(out[i].Function.Parameters)) == 0 {
			out[i].Function.Parameters = emptySchema
		}
	}
	return out
}

// bytesTrimSpace trims ASCII whitespace from a raw JSON message so a whitespace-
// only Parameters value counts as empty.
func bytesTrimSpace(b json.RawMessage) []byte {
	return bytes.TrimSpace(b)
}

// wireMessage is the reduced shape we actually send to Fireworks. Pointers /
// omitempty honour the omit-when-undefined rule (never send null for an absent
// optional field, except where null content is deliberately coalesced to "").
type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content,omitempty"`
	ToolCalls  []wireToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"
	Function ToolCallFunction `json:"function"`
}

// toWireMessages reduces internal ChatMessages to exactly the fields Fireworks
// accepts (extra fields cause a 400 on replay). Per role:
//   - tool: content is always flattened to text; the internal `name` is dropped.
//   - assistant: content as-is (null→null literal), tool_calls only when present,
//     emitted as {id,type,function} only.
//   - user/system: multimodal Parts forwarded verbatim; null content → "".
func toWireMessages(messages []ChatMessage) ([]wireMessage, error) {
	out := make([]wireMessage, 0, len(messages))
	for _, m := range messages {
		switch m.Role {
		case "tool":
			c, err := json.Marshal(m.ContentToText())
			if err != nil {
				return nil, err
			}
			out = append(out, wireMessage{Role: "tool", Content: c, ToolCallID: m.ToolCallID})
		case "assistant":
			wm := wireMessage{Role: "assistant"}
			// Assistant content: a string, or the `null` literal for a pure
			// tool-call turn. A null content serializes as JSON null (not omitted)
			// so the provider sees an explicit null on a tool-call turn.
			if m.ContentNull && m.Parts == nil {
				wm.Content = json.RawMessage("null")
			} else {
				c, err := marshalStringOrParts(m)
				if err != nil {
					return nil, err
				}
				wm.Content = c
			}
			if len(m.ToolCalls) > 0 {
				wm.ToolCalls = make([]wireToolCall, 0, len(m.ToolCalls))
				for _, t := range m.ToolCalls {
					wm.ToolCalls = append(wm.ToolCalls, wireToolCall{
						ID: t.ID, Type: "function",
						Function: ToolCallFunction{Name: t.Function.Name, Arguments: validToolArgs(t.Function.Arguments)},
					})
				}
			}
			out = append(out, wm)
		default: // user / system
			c, err := marshalStringOrParts(m)
			if err != nil {
				return nil, err
			}
			out = append(out, wireMessage{Role: m.Role, Content: c})
		}
	}
	return out, nil
}

// validToolArgs returns a tool call's arguments as a guaranteed-valid JSON OBJECT
// string for the OUTGOING request. The provider (Fireworks) re-validates EVERY
// message in the replayed history and rejects the whole request with a 400 when any
// assistant tool call's arguments are not a JSON object — so a single malformed-JSON
// tool call the model emitted (already caught and rejected locally as
// INVALID_TOOL_ARGS_JSON) would otherwise poison every subsequent turn and kill the
// session. The raw arguments are still handed to the LOCAL executor unchanged (so it
// keeps rejecting the bad call); here, at the wire boundary ONLY, an empty /
// malformed / non-object value is coerced to "{}" so the history stays acceptable and
// the model can recover next turn. A valid object passes through untouched. "null"
// unmarshals to a nil map without error, so the explicit nil check rejects it too.
func validToolArgs(args string) string {
	if strings.TrimSpace(args) == "" {
		return "{}"
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(args), &obj); err != nil || obj == nil {
		return "{}"
	}
	return args
}

// marshalStringOrParts marshals a message's content: a multimodal part array
// verbatim, else the string content (null coalesced to "").
func marshalStringOrParts(m ChatMessage) (json.RawMessage, error) {
	if m.Parts != nil {
		return json.Marshal(m.Parts)
	}
	s := m.StringContent
	if m.ContentNull {
		s = "" // user/system null coalesces to ""
	}
	return json.Marshal(s)
}

// normalizeToolCalls maps raw response tool-calls to ToolCallRequest, filtering to
// those with a function name, synthesizing a stable id from hashString when the
// provider omitted one, and defaulting arguments to "{}".
func normalizeToolCalls(calls []rawToolCall) []ToolCallRequest {
	out := make([]ToolCallRequest, 0, len(calls))
	for _, c := range calls {
		if c.Function == nil || c.Function.Name == "" {
			continue
		}
		id := c.ID
		if id == "" {
			id = "call_" + strconv.Itoa(absInt(hashString(c.Function.Name)))
		}
		args := c.Function.Arguments
		if args == "" {
			args = "{}"
		}
		out = append(out, ToolCallRequest{
			ID: id, Type: "function",
			Function: ToolCallFunction{Name: c.Function.Name, Arguments: args},
		})
	}
	return out
}

// hashString is a DJB2-style hash over UTF-16 code units, NOT runes, forced to
// int32 each step — so synthesized tool-call ids stay stable across transcripts:
// h = (h<<5) - h + <UTF-16 code unit at i>. A non-BMP rune contributes two code
// units.
func hashString(s string) int32 {
	var h int32
	for _, u := range utf16Units(s) {
		h = (h << 5) - h + int32(u)
		// A 32-bit signed truncation is required each step; int32 arithmetic already
		// wraps, so no extra masking is needed.
	}
	return h
}

// utf16Units returns the UTF-16 code units of s.
func utf16Units(s string) []uint16 {
	var units []uint16
	for _, r := range s {
		if r <= 0xFFFF {
			units = append(units, uint16(r))
		} else {
			r -= 0x10000
			units = append(units, uint16(0xD800+(r>>10)), uint16(0xDC00+(r&0x3FF)))
		}
	}
	return units
}

// absInt mirrors Math.abs over the int32 hash. Math.abs(-2147483648) yields
// 2147483648 (a JS number, not wrapped), so widen to int before negating to avoid
// the int32 overflow that would keep it negative.
func absInt(v int32) int {
	n := int(v)
	if n < 0 {
		return -n
	}
	return n
}

// ExtractJson pulls the first balanced JSON object/array out of a string, ignoring
// trailing prose or stray <think> residue. String- and escape-aware so braces
// inside string literals don't unbalance the count. Operates on bytes; the
// bracket/quote/escape bytes are all ASCII so byte indexing is safe.
func ExtractJson(s string) string {
	b := []byte(s)
	start := -1
	for i, ch := range b {
		if ch == '[' || ch == '{' {
			start = i
			break
		}
	}
	if start == -1 {
		return s
	}
	open := b[start]
	var closeCh byte = '}'
	if open == '[' {
		closeCh = ']'
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(b); i++ {
		ch := b[i]
		if inString {
			if escaped {
				escaped = false
			} else if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			inString = true
		case open:
			depth++
		case closeCh:
			depth--
			if depth == 0 {
				return string(b[start : i+1])
			}
		}
	}
	// Unbalanced — return from the first bracket and let the JSON parser report it.
	return string(b[start:])
}
