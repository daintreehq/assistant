package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/tools"
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
	return passthroughWithOptions(ctx, tctx, mcpName, args, requestKey, tools.MCPCallOptions{})
}

// passthroughWithOptions is passthrough with per-call transport knobs. Split out
// rather than added to every call site because exactly one wrapper needs them:
// project.runCheck waits on a project command whose server-side budget can reach an
// hour, far past the transport's 120s default. Every other wrapper keeps the default
// by construction, so a future wrapper cannot acquire a long deadline by accident.
func passthroughWithOptions(ctx context.Context, tctx *tools.ToolContext, mcpName string, args map[string]any, requestKey string, opts tools.MCPCallOptions) tools.ToolResult {
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

	res, err := mcp.CallTool(ctx, mcpName, call, opts)
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

// strictDecode unmarshals raw into out rejecting unknown fields AND runs out's
// Validate() when it has one, returning an INVALID_ARGS failure (and false) when
// either rejects. Handlers call it at the top to mirror the registry's Decode
// contract for the inner shape.
//
// Running Validate() here is what makes the mirror faithful. tools.StrictDecoder
// (the registry's Decode) runs it; tools.DecodeStrict alone does not. A handler
// invoked directly — which is how every wrapper unit test invokes it — would then
// skip every bound the arg type declares, so a test could prove a rule the
// dispatch path enforces and the handler does not.
func strictDecode(raw json.RawMessage, name string, out any) (tools.ToolResult, bool) {
	if err := tools.DecodeStrict(raw, out); err != nil {
		return tools.Fail(codeInvalidArgs, fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error())), false
	}
	if v, ok := out.(tools.Validator); ok {
		if err := v.Validate(); err != nil {
			return tools.Fail(codeInvalidArgs, fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error())), false
		}
	}
	return tools.ToolResult{}, true
}

// structuredFrom digs the object a typed wrapper promised out of a passthrough
// result. Daintree returns it as structuredContent on every action that declares
// mcpOutputSchema, and as a JSON text block otherwise, so both channels are read —
// structuredContent first, because it is the one the server typed.
//
// Returning false is meaningful, not a shrug: it means the action answered but not
// with the object its own result schema promises, and a wrapper that shaped a
// verdict out of that would be inventing one.
func structuredFrom(result any) (map[string]any, bool) {
	top, ok := result.(map[string]any)
	if !ok {
		return nil, false
	}
	if obj, ok := top["structuredContent"].(map[string]any); ok && obj != nil {
		return obj, true
	}
	text, _ := top["text"].(string)
	if strings.TrimSpace(text) == "" {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(text), &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// failMalformed is the shared "the action replied, but not with its own result
// shape" failure. Recoverable: a transient renderer hiccup can produce it, and the
// model retrying once is cheaper than us deciding on its behalf that it cannot.
func failMalformed(mcpName string) tools.ToolResult {
	return tools.Fail(codeMCPToolError, fmt.Sprintf(
		"Daintree returned no readable result object for %s, so its result cannot be reported. Do not infer an outcome from the absence.", mcpName))
}
