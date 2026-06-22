package mcpwrap

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// focusNoArg builds a no-argument ui-risk focus wrapper that forwards to the
// same-named MCP action (workflow.focusNextAttention and the agent.* focus
// verbs). Risk ui never confirms, so these are one-line forwarders.
func focusNoArg(name, desc string) *tools.Tool {
	schema, _ := json.Marshal(tools.NoArgs)
	return &tools.Tool{
		Name:        name,
		Description: desc,
		Risk:        domain.RiskUI,
		Schema:      schema,
		// No Decode: NoArgs tools accept (and ignore) an empty object.
		Handle: func(ctx context.Context, _ json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			return passthrough(ctx, tctx, name, nil, "")
		},
	}
}

func newWorkflowFocusNextAttentionTool() *tools.Tool {
	return focusNoArg(
		"workflow.focusNextAttention",
		"Focus the Daintree UI on the next workflow/terminal needing attention.",
	)
}
