package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCP-wrapper error codes (model-facing recovery signals — keep exact).
const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeMCPUnavailable = "MCP_UNAVAILABLE"
	codeMCPToolError   = "MCP_TOOL_ERROR"
	codeCancelled      = "CANCELLED"
)

// mcpFrom returns the consumer MCPClient from a ToolContext. The same concrete
// client satisfies both tools.MCPClient and our local MCPClient, so a structural
// type assertion bridges them without importing the concrete package.
func mcpFrom(tctx *tools.ToolContext) MCPClient {
	if tctx == nil || tctx.MCP == nil {
		return nil
	}
	if m, ok := tctx.MCP.(MCPClient); ok {
		return m
	}
	return nil
}

// passthrough is the shared forwarder every wrapper delegates to. It merges an
// optional requestKey into args, forwards to the named MCP action, and maps the
// MCP envelope to a ToolResult. Disconnected → MCP_UNAVAILABLE; a context cancel
// during the call → CANCELLED (recoverable:false); isError → MCP_TOOL_ERROR with
// the refusal text + structuredContent detail; success → ok("Called <name>.").
func passthrough(ctx context.Context, tctx *tools.ToolContext, mcpName string, args map[string]any, requestKey string) tools.ToolResult {
	mcp := mcpFrom(tctx)
	if mcp == nil || !mcp.Connected() {
		return tools.Fail(codeMCPUnavailable, fmt.Sprintf("Daintree MCP is not connected; cannot call %s. Use /reconnect to retry once Daintree is available.", mcpName))
	}

	call := make(map[string]any, len(args)+1)
	for k, v := range args {
		call[k] = v
	}
	if requestKey != "" {
		call["requestKey"] = requestKey
	}

	res, err := mcp.CallTool(ctx, mcpName, call)
	if err != nil {
		// A cancelled turn is a distinct, non-recoverable outcome from a tool error.
		if ctx.Err() != nil {
			return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", mcpName), tools.Unrecoverable())
		}
		return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree MCP call %s failed: %s", mcpName, err.Error()))
	}
	if res.IsError {
		msg := fmt.Sprintf("Daintree refused %s.", mcpName)
		if res.Text != "" {
			msg = fmt.Sprintf("Daintree refused %s: %s", mcpName, res.Text)
		}
		return tools.Fail(codeMCPToolError, msg, tools.WithDetails(map[string]any{
			"structuredContent": res.StructuredContent,
			"rawText":           res.Text,
		}))
	}
	return tools.Ok(fmt.Sprintf("Called %s.", mcpName), map[string]any{
		"text":              res.Text,
		"structuredContent": res.StructuredContent,
	})
}

// strictDecode unmarshals raw into out rejecting unknown fields, returning an
// INVALID_ARGS failure (and false) when the args are malformed. Handlers call it
// at the top to mirror the registry's Decode contract for the inner shape.
func strictDecode(raw json.RawMessage, name string, out any) (tools.ToolResult, bool) {
	if err := tools.DecodeStrict(raw, out); err != nil {
		return tools.Fail(codeInvalidArgs, fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error())), false
	}
	return tools.ToolResult{}, true
}
