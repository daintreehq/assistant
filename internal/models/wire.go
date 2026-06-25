package models

// Wire response shapes for the OpenAI-compatible Chat Completions API. These
// mirror exactly the fields we read; unknown fields are ignored by encoding/json.

// rawToolCall is a tool call as it appears in a response message or a stream delta.
// All fields optional: a streamed delta may carry only index+id, only a name, or
// only an arguments fragment.
type rawToolCall struct {
	Index    *int             `json:"index"`
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function *rawToolCallFunc `json:"function"`
}

type rawToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// rawUsage is the token-usage block. cached_tokens is nested under
// prompt_tokens_details (optional, provider-dependent).
type rawUsage struct {
	PromptTokens     *int `json:"prompt_tokens"`
	CompletionTokens *int `json:"completion_tokens"`
	TotalTokens      *int `json:"total_tokens"`
	PromptDetails    *struct {
		CachedTokens *int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// toUsage maps a raw wire usage block to the public Usage. cachedTokens is taken
// from the nested prompt_tokens_details when present.
func (u *rawUsage) toUsage() *Usage {
	if u == nil {
		return nil
	}
	out := &Usage{
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		TotalTokens:      u.TotalTokens,
	}
	if u.PromptDetails != nil {
		out.CachedTokens = u.PromptDetails.CachedTokens
	}
	return out
}

// rawMessage is choice.message on the non-stream path. ReasoningContent is DeepSeek's
// dedicated reasoning channel: when a <think> phase runs, the chain-of-thought comes
// back here (NOT inline in Content as <think>…</think>), so capturing it is how we
// populate ChatResult.Reasoning for an explicit-effort call.
type rawMessage struct {
	Content          *string       `json:"content"`
	ReasoningContent *string       `json:"reasoning_content"`
	ToolCalls        []rawToolCall `json:"tool_calls"`
}

// rawChoice is one completion choice (non-stream).
type rawChoice struct {
	Message      rawMessage `json:"message"`
	FinishReason *string    `json:"finish_reason"`
}

// chatResponse is the full non-stream response body.
type chatResponse struct {
	Choices []rawChoice `json:"choices"`
	Usage   *rawUsage   `json:"usage"`
}

// rawDelta is choice.delta on the streaming path. ReasoningContent streams DeepSeek's
// reasoning channel in fragments (see rawMessage) — accumulated separately from the
// visible content, never forwarded to onToken.
type rawDelta struct {
	Content          *string       `json:"content"`
	ReasoningContent *string       `json:"reasoning_content"`
	ToolCalls        []rawToolCall `json:"tool_calls"`
}

// streamChoice is one choice in a streamed chunk.
type streamChoice struct {
	Delta        rawDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

// streamError carries a provider-side error delivered MID-STREAM (after HTTP 200) as
// an SSE `data: {"error": {...}}` payload — e.g. a rate limit or upstream failure the
// provider can only report once the stream has opened. Without capturing it, such a
// chunk has no choices and would be silently treated as an empty, clean completion.
type streamError struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// streamChunk is a single SSE `data:` payload on the streaming path. The
// usage-only final chunk carries an empty Choices array.
type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *rawUsage      `json:"usage"`
	Error   *streamError   `json:"error"`
}
