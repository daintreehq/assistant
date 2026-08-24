// Package models is the CONVERSATION WIRE VOCABULARY shared by the agent loop, the
// tool registry, and the backend client: the message/tool/result shapes a turn is
// built from.
//
// It is NOT a model client. The direct provider HTTP transport, the tier Router, the
// SSE parser, the retry/reliability layer, and the pricing table all used to live here;
// they were deleted once the backend became the CLI's only model gateway
// (internal/backend). Nothing in this package talks to a provider, and nothing
// should: adding a transport back here would let a handler bypass the backend that
// owns prompt assembly, runbook selection, and the provider credentials.
//
// # Reading the "DeepSeek" comments in this repo
//
// The backend reaches EVERY model through OpenRouter, using the caller's own key. So
// wherever a comment here or in internal/agent / internal/backend says "DeepSeek 400s
// if …" or "DeepSeek requires …", read it as *the behaviour of the DeepSeek route
// (`deepseek/deepseek-v4-flash-0731`) as observed through OpenRouter* — the constraint
// is real and load-bearing, but it belongs to that model route, not to OpenRouter and
// not to a direct provider integration this binary has never had. A different model the
// backend selects may not share it; verify before generalising, and never treat such a
// note as licence to add provider-specific transport code here.

package models

import (
	"encoding/json"
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
// URL. Only these two kinds are ever sent (DeepSeek ignores `detail`, so it is
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
	// Name is an internal label, and reaches the wire for EXACTLY one message: the
	// server-delivered compacted context block, whose reserved "daintree_compaction"
	// must come back on every later request — that name is the entire mechanism by
	// which the backend recognises frozen history without holding any state of its
	// own, so dropping it would leave the server re-compacting a prefix the client had
	// already replaced.
	//
	// Every OTHER use is local bookkeeping that stays local: tool-result messages carry
	// the tool's own name here (see Session.runToolBatch and internal/subagent), and the
	// wire encoder and the persister both drop it. See agent.isCompactionBlockName.
	Name string

	// ReasoningContent is an assistant turn's chain-of-thought (DeepSeek thinking
	// mode). Captured from the backend response and replayed verbatim on later
	// requests — DeepSeek 400s if an assistant tool-call turn's reasoning is dropped.
	// Empty in the default (thinking-off) posture.
	ReasoningContent string
}

// TextMessage is a convenience constructor for a plain string-content message.
func TextMessage(role, content string) ChatMessage {
	return ChatMessage{Role: role, StringContent: content}
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
// A nil/empty json.RawMessage marshals to `"parameters":null`, which DeepSeek
// rejects — an absent schema is "no arguments", i.e. an empty object.
var emptySchema = json.RawMessage(`{}`)

// Usage is the per-call token accounting, with pointer fields mirroring the
// optional wire fields (a missing count is nil, not a misleading 0).
type Usage struct {
	PromptTokens     *int
	CompletionTokens *int
	TotalTokens      *int
	// CachedTokens is the cached-prompt subset (billed at a discount), when the
	// provider reports it under prompt_tokens_details.cached_tokens.
	CachedTokens *int
}

// ChatOptions is the input to a model call. The turn's cancellation is carried by
// the context.Context passed to each method (the AbortSignal equivalent).
type ChatOptions struct {
	Model       string
	Messages    []ChatMessage
	Tools       []ChatTool
	ToolChoice  string // "auto" | "none" | "required" | "" (omit)
	Temperature *float64
	MaxTokens   *int
	// PromptCacheKey caches a static system-prompt prefix on the DeepSeek side.
	// Sent ONLY on chat/chatStream (never on json) and only when non-empty.
	PromptCacheKey string
	// ReasoningEffort is the abstract think-control the Router sets per tier. It does
	// NOT map 1:1 to a DeepSeek wire field, because DeepSeek splits the two concerns:
	//   - the OFF switch is the `thinking` object — DeepSeek's reasoning_effort has NO
	//     "none" variant (it accepts only high|low|medium|max|xhigh and 400s on "none").
	//   - a real effort (high|low|…) passes straight through as `reasoning_effort`.
	// So buildBody translates: "none" ⇒ thinking:{type:"disabled"} (think-free, what the
	// Router forces for the small tier and for flash everywhere so every deepseek-v4-flash
	// call — judge / summary / extraction / classification — stays fast and light); any
	// other non-empty value ⇒ the `reasoning_effort` field verbatim; empty ⇒ omit both
	// (provider default, which for a reasoning model is think-ON).
	ReasoningEffort string
}

// ChatResult is the normalized output of a model call.
type ChatResult struct {
	Content      string
	Reasoning    string
	ToolCalls    []ToolCallRequest
	FinishReason string
	Usage        *Usage
}
