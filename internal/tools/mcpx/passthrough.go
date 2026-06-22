package mcpx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCP-family error codes (model-facing).
const (
	codeMCPUnavailable  = "MCP_UNAVAILABLE"
	codeMCPToolError    = "MCP_TOOL_ERROR"
	codeCancelled       = "CANCELLED"
	codeUseTypedWrapper = "USE_TYPED_WRAPPER"
)

// passthrough forwards a call to a named Daintree MCP tool. Shared by every typed
// wrapper and structurally identical to daintree.call — but each wrapper carries
// an accurate risk class. The args map is forwarded verbatim, so wrappers stay
// agnostic to Daintree's exact per-tool argument schema. A non-empty requestKey
// is merged in as the dedicated idempotency parameter.
func passthrough(ctx context.Context, mcp MCPClient, mcpName string, args map[string]any, requestKey string) tools.ToolResult {
	if mcp == nil || !mcp.Connected() {
		return tools.Fail(codeMCPUnavailable,
			fmt.Sprintf("Daintree MCP is not connected; cannot call %s.", mcpName))
	}
	callArgs := make(map[string]any, len(args)+1)
	for k, v := range args {
		callArgs[k] = v
	}
	if requestKey != "" {
		callArgs["requestKey"] = requestKey
	}
	res, err := mcp.CallTool(ctx, mcpName, callArgs)
	if err != nil {
		// A user abort surfaces as a timeout-shaped MCP error; report it as a clean
		// cancellation rather than a tool failure.
		if ctx.Err() != nil {
			return tools.Fail(codeCancelled, fmt.Sprintf("Turn cancelled during %s.", mcpName), tools.Unrecoverable())
		}
		return tools.Fail(codeMCPToolError, fmt.Sprintf("Daintree call %s failed: %s", mcpName, err.Error()))
	}
	if res.IsError {
		// Carry Daintree's own refusal text into the failure summary so a denied
		// grant-authorized mutation surfaces *why* it was refused.
		msg := fmt.Sprintf("Daintree tool %s returned an error.", mcpName)
		if res.Text != "" {
			msg = fmt.Sprintf("Daintree refused %s: %s", mcpName, res.Text)
		}
		return tools.Fail(codeMCPToolError, msg,
			tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent, "rawText": res.Text}))
	}
	return tools.Ok(fmt.Sprintf("Called %s.", mcpName),
		map[string]any{"text": res.Text, "structuredContent": res.StructuredContent})
}

// extractArmedSet pulls the `armed: string[]` set from an arming tool's result,
// reading structuredContent first then falling back to a JSON-encoded text body.
// Returns (set, true) only when a source carries a well-formed string array — so a
// legitimately empty set (after disarmAll) is preserved as [] while a
// missing/garbled set returns (nil, false) and can fail loudly.
func extractArmedSet(res map[string]any) ([]string, bool) {
	fromObj := func(o any) ([]string, bool) {
		m, ok := o.(map[string]any)
		if !ok {
			return nil, false
		}
		arr, ok := m["armed"].([]any)
		if !ok {
			return nil, false
		}
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	if res == nil {
		return nil, false
	}
	if set, ok := fromObj(res["structuredContent"]); ok {
		return set, true
	}
	if text, ok := res["text"].(string); ok && text != "" {
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err == nil {
			if set, ok := fromObj(parsed); ok {
				return set, true
			}
		}
	}
	return nil, false
}

// terminalArmingPassthrough runs the shared passthrough then replaces its generic
// summary with the concrete armed-terminal list Daintree returns as
// {armed:string[]}. Arming must NEVER silently reroute the human's keystrokes
// (#136): if neither result source carries the set we FAIL rather than hide which
// terminals are now armed — an unknown arming state is the one thing these tools
// may not do quietly.
func terminalArmingPassthrough(ctx context.Context, mcp MCPClient, mcpName string, args map[string]any, action string) tools.ToolResult {
	res := passthrough(ctx, mcp, mcpName, args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	armed, ok := extractArmedSet(result)
	if !ok {
		var sc, rawText any
		if result != nil {
			sc = result["structuredContent"]
			rawText = result["text"]
		}
		return tools.Fail(codeMCPToolError,
			fmt.Sprintf("%s did not report the resulting armed set, so the current arming state is unknown — re-check with terminal.getStatus before relying on it.", mcpName),
			tools.WithDetails(map[string]any{"structuredContent": sc, "rawText": rawText}))
	}
	list := "none"
	if len(armed) > 0 {
		list = join(armed, ", ")
	}
	return tools.Ok(fmt.Sprintf("%s Armed terminals now: %s.", action, list), result)
}

func join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}
