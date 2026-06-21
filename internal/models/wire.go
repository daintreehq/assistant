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

// rawMessage is choice.message on the non-stream path.
type rawMessage struct {
	Content   *string       `json:"content"`
	ToolCalls []rawToolCall `json:"tool_calls"`
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

// rawDelta is choice.delta on the streaming path.
type rawDelta struct {
	Content   *string       `json:"content"`
	ToolCalls []rawToolCall `json:"tool_calls"`
}

// streamChoice is one choice in a streamed chunk.
type streamChoice struct {
	Delta        rawDelta `json:"delta"`
	FinishReason *string  `json:"finish_reason"`
}

// streamChunk is a single SSE `data:` payload on the streaming path. The
// usage-only final chunk carries an empty Choices array.
type streamChunk struct {
	Choices []streamChoice `json:"choices"`
	Usage   *rawUsage      `json:"usage"`
}
