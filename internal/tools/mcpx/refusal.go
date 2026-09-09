package mcpx

import (
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/tools"
)

func hostRefusal(msg string, res MCPCallResult) tools.ToolResult {
	if mcp.ToolRefusalPermanent(res.StructuredContent, res.Text) {
		return tools.Fail(codeMCPToolError, msg, tools.Unrecoverable(), tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent, "rawText": res.Text}))
	}
	return tools.Fail(codeMCPToolError, msg, tools.WithDetails(map[string]any{"structuredContent": res.StructuredContent, "rawText": res.Text}))
}
