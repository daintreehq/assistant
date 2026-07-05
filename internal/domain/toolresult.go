package domain

// ToolError is the error half of the ToolResult envelope. Handlers never throw
// to the caller — the registry catches and returns a fail() result instead.
type ToolError struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Recoverable bool   `json:"recoverable"`
	Details     any    `json:"details,omitempty"`
}

// AsyncHandle marks a successful tool result as an ACCEPTED-BUT-STILL-RUNNING
// asynchronous operation: the runtime owns the work from here and will deliver
// the completion later through the attention queue (an autonomous wake), never
// as a late result for this call. Typed (not sniffed out of Result) so the UI
// can render the distinct async-pending state and the wire shape stays stable.
type AsyncHandle struct {
	ID          string   `json:"id"`       // asy_<uuid8>
	ToolName    string   `json:"toolName"` // the async tool that accepted the work
	Title       string   `json:"title,omitempty"`
	GroupID     string   `json:"groupId,omitempty"`
	TerminalIDs []string `json:"terminalIds,omitempty"`
}

// ToolResult is the uniform envelope every tool handler returns.
//
//	summary  one-line human/LLM-facing message
//	auditId  id of the audit_log row this call produced (set by the registry)
//
// Result carries the typed payload as `any` here so the domain stays generic;
// concrete tool packages can decode it.
type ToolResult struct {
	Ok      bool       `json:"ok"`
	Summary string     `json:"summary"`
	Result  any        `json:"result,omitempty"`
	Error   *ToolError `json:"error,omitempty"`
	AuditID string     `json:"auditId,omitempty"`
	// Async is set ONLY by async tool handlers on the immediate "accepted" result
	// (see AsyncHandle). nil on every synchronous tool result.
	Async *AsyncHandle `json:"async,omitempty"`
}

// Ok builds a successful ToolResult. Pass nil for result when there is none.
func Ok(summary string, result any) ToolResult {
	return ToolResult{Ok: true, Summary: summary, Result: result}
}

// FailOption tunes a failure result.
type FailOption func(*ToolError)

// Unrecoverable marks a failure as non-recoverable (the default is recoverable).
func Unrecoverable() FailOption {
	return func(e *ToolError) { e.Recoverable = false }
}

// WithDetails attaches an arbitrary details payload to a failure.
func WithDetails(details any) FailOption {
	return func(e *ToolError) { e.Details = details }
}

// Fail builds a failed ToolResult. summary mirrors the error message.
// Defaults to recoverable=true.
func Fail(code, message string, opts ...FailOption) ToolResult {
	e := &ToolError{Code: code, Message: message, Recoverable: true}
	for _, opt := range opts {
		opt(e)
	}
	return ToolResult{Ok: false, Summary: message, Error: e}
}

// Common ToolError codes. This is not exhaustive — tool packages may introduce
// their own — but these are the cross-cutting ones the agent loop reasons about.
const (
	CodeValidation   = "validation_error"
	CodeUnauthorized = "unauthorized"
	CodeDenied       = "denied"
	CodeNotFound     = "not_found"
	CodeInternal     = "internal_error"
	CodeTimeout      = "timeout"
	CodeRateLimited  = "rate_limited"
)
