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
// Spec: §4.14 Forge wrappers (reads).

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
		Schema:      forgeArgumentsSchema,
		Decode:      tools.StrictDecoder(func() any { return &forgeArgumentsArgs{} }),
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
		Name:        "forge.getPR",
		Description: "Get a single forge (GitHub) pull request by number.",
		Risk:        domain.RiskRead,
		Schema:      forgeGetPRSchema,
		Decode:      tools.StrictDecoder(func() any { return &forgeGetPRArgs{} }),
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
