package domain

// AgentEvent is the structured event vocabulary the runtime emits via an
// EventSink (see internal/ports). The UiBridge or the console/json sinks consume
// it. It is a tagged union: Type discriminates which payload fields are set.
//
// This mirrors the JsonlEventType wire vocabulary (assistant:* / tool:* / error
// / info / result) plus a usage event, so the live stream, the durable
// RunEvent log, and the one-shot JSONL output all share one shape.
type AgentEventType string

const (
	EventAssistantStart     AgentEventType = "assistant:start"
	EventAssistantChunk     AgentEventType = "assistant:content"
	EventAssistantEnd       AgentEventType = "assistant:end"
	EventAssistantCancelled AgentEventType = "assistant:cancelled"
	EventToolCall           AgentEventType = "tool:call"
	EventToolResult         AgentEventType = "tool:result"
	EventError              AgentEventType = "error"
	EventInfo               AgentEventType = "info"
	EventUsage              AgentEventType = "usage"
)

// ToolCallInfo describes a tool invocation announced or batched in a turn.
type ToolCallInfo struct {
	ToolCallID string `json:"toolCallId"`
	ToolName   string `json:"toolName"`
	ArgsJson   string `json:"argsJson,omitempty"`
}

// Usage carries token accounting for a model call.
type Usage struct {
	PromptTokens     int `json:"promptTokens"`
	CompletionTokens int `json:"completionTokens"`
	TotalTokens      int `json:"totalTokens"`
}

// AgentEvent is a single structured event. Optional fields are populated per
// Type. RunID/Seq/Ts give every event a stable place in the durable replay log.
type AgentEvent struct {
	Type  AgentEventType `json:"type"`
	RunID string         `json:"runId,omitempty"`
	Seq   int            `json:"seq"`
	Ts    int64          `json:"ts"`

	// assistant:content — a streamed chunk of assistant text.
	Chunk string `json:"chunk,omitempty"`

	// assistant:end — the final assistant text for the turn.
	Text string `json:"text,omitempty"`

	// tool:call — the invoked tool.
	ToolCall *ToolCallInfo `json:"toolCall,omitempty"`

	// tool:result — the settled tool result.
	ToolResult *ToolResult `json:"toolResult,omitempty"`

	// error — a structured error.
	ErrorCode    string `json:"errorCode,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`

	// info — a freeform informational message.
	Info string `json:"info,omitempty"`

	// usage — token accounting.
	Usage *Usage `json:"usage,omitempty"`
}
