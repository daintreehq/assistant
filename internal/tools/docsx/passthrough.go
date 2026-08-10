package docsx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/tools"
)

// Docs-family error codes (model-facing recovery signals — keep exact).
const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeMCPUnavailable = "MCP_UNAVAILABLE"
	codeMCPToolError   = "MCP_TOOL_ERROR"
	codeCancelled      = "CANCELLED"
)

// passthrough forwards a validated call to the named docs MCP tool and maps the MCP
// envelope to a ToolResult. Disconnected → MCP_UNAVAILABLE; a context cancel during the
// call → CANCELLED (unrecoverable); isError → MCP_TOOL_ERROR with the server text;
// success → ok(summary, {text, structuredContent}). The summary is supplied by the
// caller so the audit/transcript reads "Searched Daintree documentation for: …" rather
// than a generic "Called search." Structurally mirrors mcpx/mcpwrap passthrough.
func passthrough(ctx context.Context, mcp MCPClient, mcpName, summary string, args map[string]any) tools.ToolResult {
	if mcp == nil || !mcp.Connected() {
		return tools.Fail(codeMCPUnavailable,
			fmt.Sprintf("The Daintree documentation MCP is not connected; cannot call %s. Use /reconnect to retry.", mcpName))
	}

	res, err := mcp.CallTool(ctx, mcpName, args)
	if err != nil {
		// A cancelled turn is a distinct, non-recoverable outcome from a tool error.
		if ctx.Err() != nil {
			return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", mcpName), tools.Unrecoverable())
		}
		return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree docs MCP call %s failed: %s", mcpName, err.Error()))
	}
	if res.IsError {
		msg := fmt.Sprintf("Daintree docs MCP returned an error for %s.", mcpName)
		if res.Text != "" {
			msg = fmt.Sprintf("Daintree docs MCP error on %s: %s", mcpName, res.Text)
		}
		return tools.Fail(codeMCPToolError, msg, tools.WithDetails(map[string]any{
			"structuredContent": res.StructuredContent,
			"rawText":           res.Text,
		}))
	}
	return tools.Ok(summary, map[string]any{
		"text":              res.Text,
		"structuredContent": res.StructuredContent,
	})
}

// strictDecode unmarshals raw into out rejecting unknown fields, returning an
// INVALID_ARGS failure (and false) when the args are malformed.
func strictDecode(raw json.RawMessage, name string, out any) (tools.ToolResult, bool) {
	if err := tools.DecodeStrict(raw, out); err != nil {
		return tools.Fail(codeInvalidArgs, fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error())), false
	}
	return tools.ToolResult{}, true
}
