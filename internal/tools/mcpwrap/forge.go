package mcpwrap

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Forge READS (risk read). listIssues/getIssue/listPRs forward an opaque
// arguments record verbatim (forge query options are forge-defined). getPR takes
// a typed {cwd?, prNumber} so the model can't drop the required positive integer.
// Forge wrappers (reads).

// forgeArgumentsArgs forwards an opaque arguments record (strict at the top).
type forgeArgumentsArgs struct {
	Arguments map[string]any `json:"arguments,omitempty"`
}

var forgeArgumentsSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true }
  }
}`)

// forgeRead builds a read-only forge wrapper that forwards args.arguments ?? {}.
func forgeRead(name, desc string) *tools.Tool {
	return &tools.Tool{
		Name:        name,
		Description: desc,
		Risk:        domain.RiskRead,
		// Every forgeRead tool is an independent, bounded MCP snapshot read (list/get over
		// the forge / worktree / git surface) with no ordering dependency on its batch
		// siblings, so a batch of them overlaps its round-trips up to the MCP governor's
		// in-flight cap instead of serializing. Safe like terminal.extract; double-gated on
		// RiskRead by the runner.
		Parallelizable: true,
		Schema:         forgeArgumentsSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeArgumentsArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeArgumentsArgs
			if res, ok := strictDecode(args, name, &a); !ok {
				return res
			}
			fwd := a.Arguments
			if fwd == nil {
				fwd = map[string]any{}
			}
			return passthrough(ctx, tctx, name, fwd, "")
		},
	}
}

func newForgeListIssuesTool() *tools.Tool {
	return forgeRead("forge.listIssues", "List forge (GitHub) issues for the project.")
}

func newForgeListPRsTool() *tools.Tool {
	return forgeRead("forge.listPRs", "List forge (GitHub) pull requests for the project.")
}

func newForgeGetIssueTool() *tools.Tool {
	return forgeRead("forge.getIssue", "Get a single forge (GitHub) issue.")
}

// forgeGetPRArgs is the typed shape for forge.getPR: a required positive
// prNumber (never a string) plus an optional cwd.
type forgeGetPRArgs struct {
	CWD      string `json:"cwd,omitempty"`
	PRNumber int    `json:"prNumber"`
}

var forgeGetPRSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["prNumber"],
  "properties": {
    "cwd": { "type": "string" },
    "prNumber": { "type": "integer", "minimum": 1, "description": "The PR number (positive integer)." }
  }
}`)

func newForgeGetPRTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getPR",
		Description: "Get a single forge (GitHub) pull request by number. " +
			"PARALLEL: forge.getPR calls batched in ONE reply run concurrently — to check several PRs, emit one call each in one batch.",
		Risk: domain.RiskRead,
		// Independent per-PR read over the forge MCP, no ordering dependency on siblings:
		// checking several PRs at once overlaps their round-trips. See terminal.extract.
		Parallelizable: true,
		Schema:         forgeGetPRSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetPRArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetPRArgs
			if res, ok := strictDecode(args, "forge.getPR", &a); !ok {
				return res
			}
			if a.PRNumber <= 0 {
				return tools.Fail(codeInvalidArgs, "forge.getPR: prNumber must be a positive integer")
			}
			fwd := map[string]any{"prNumber": a.PRNumber}
			if a.CWD != "" {
				fwd["cwd"] = a.CWD
			}
			return passthrough(ctx, tctx, "forge.getPR", fwd, "")
		},
	}
}
