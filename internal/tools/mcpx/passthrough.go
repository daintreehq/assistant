package mcpx

import (
	"context"
	"encoding/json"
	"fmt"
	"unicode"

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

// truncateCommand renders a command for a single-line, human-facing summary:
// newlines/tabs collapse to a space (so a heredoc or multi-line command can't
// break the inline render at render_operations.go) and the result is clipped to
// max runes with a "..." marker. Rune-aware so a multibyte command is never cut
// mid-codepoint.
func truncateCommand(s string, max int) string {
	// Collapse any whitespace run (incl. \r\n, tabs, and exotic Unicode line/paragraph
	// separators) to a single space and trim the ends — keeps the summary one line and
	// free of leading indentation. prevSpace starts true so a leading whitespace run is
	// dropped entirely.
	var b []rune
	prevSpace := true
	for _, r := range s {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b = append(b, ' ')
				prevSpace = true
			}
			continue
		}
		b = append(b, r)
		prevSpace = false
	}
	// Trim a single trailing space left by the collapse.
	if len(b) > 0 && b[len(b)-1] == ' ' {
		b = b[:len(b)-1]
	}
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "..."
}

// terminalSendCommandPassthrough runs the shared passthrough then replaces its
// generic "Called terminal.sendCommand." summary with a concrete, self-describing
// one: "Sent to terminal <id>: <command>." The assistant already knows the
// terminalId + command from the validated args, so the human-facing record (audit
// Summary + live transcript) is specific and correlated even though Daintree's
// response carries no acceptance/echo field to verify against. The original result
// payload (text + structuredContent) is preserved verbatim.
func terminalSendCommandPassthrough(ctx context.Context, mcp MCPClient, terminalID, command string, args map[string]any) tools.ToolResult {
	res := passthrough(ctx, mcp, "terminal.sendCommand", args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	return tools.Ok(fmt.Sprintf("Sent to terminal %s: %s.", terminalID, truncateCommand(command, 80)), result)
}

// copyTreeInjectPassthrough mirrors terminalSendCommandPassthrough for
// copyTree.injectToTerminal: the injected payload is a large, unnamed copy-tree
// digest, so the summary names the destination terminal rather than echoing
// content — "Injected copy tree into terminal <id>." replacing the generic
// "Called copyTree.injectToTerminal."
func copyTreeInjectPassthrough(ctx context.Context, mcp MCPClient, terminalID string, args map[string]any) tools.ToolResult {
	res := passthrough(ctx, mcp, "copyTree.injectToTerminal", args, "")
	if !res.Ok {
		return res
	}
	result, _ := res.Result.(map[string]any)
	return tools.Ok(fmt.Sprintf("Injected copy tree into terminal %s.", terminalID), result)
}
