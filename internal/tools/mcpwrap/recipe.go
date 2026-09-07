package mcpwrap

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// recipeListArgs forwards an opaque arguments record to recipe.list (read).
type recipeListArgs struct {
	Arguments map[string]any `json:"arguments,omitempty"`
}

var recipeListSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true }
  }
}`)

func newRecipeListTool() *tools.Tool {
	return &tools.Tool{
		Name:        "recipe.list",
		Description: "List saved workspace recipes when the user requests one. Returns ids, names and terminal counts, NOT startup prompts or argument schemas. A matching name does not prove launch behavior. Skip recipe discovery for plain worktrees or custom agent tasks. Use tool.schema for action arguments. Read-only passthrough; MCP_UNAVAILABLE when disconnected.",
		Risk:        domain.RiskRead,
		Schema:      recipeListSchema,
		Decode:      tools.StrictDecoder(func() any { return &recipeListArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a recipeListArgs
			if res, ok := strictDecode(args, "recipe.list", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "recipe.list", a.Arguments, "")
		},
	}
}

// recipeRunArgs runs a recipe. The top-level recipeId is authoritative: it is
// merged LAST into the forwarded arguments so a stray nested arguments.recipeId
// can never override the explicit one.
type recipeRunArgs struct {
	RecipeID   string         `json:"recipeId"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RequestKey string         `json:"requestKey,omitempty"`
}

var recipeRunSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["recipeId"],
  "properties": {
    "recipeId": { "type": "string", "description": "The recipe to run." },
    "arguments": { "type": "object", "additionalProperties": true },
    "requestKey": { "type": "string" }
  }
}`)

func newRecipeRunTool() *tools.Tool {
	return &tools.Tool{
		Name:        "recipe.run",
		Description: "Run a requested saved recipe in an existing worktree, immediately launching its terminals and startup prompts. Read recipe.list for recipeId and tool.schema for fields nested inside arguments. For a custom task use agentTask.spawnForEdits with the full taskPrompt. Mutates project state under the active confirmation policy. Pass requestKey for an idempotent retry; returns Daintree's raw result.",
		Risk:        domain.RiskProject,
		Consequence: "Launches a saved recipe's terminals and configured startup prompts in a worktree.",
		Schema:      recipeRunSchema,
		Decode:      tools.StrictDecoder(func() any { return &recipeRunArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a recipeRunArgs
			if res, ok := strictDecode(args, "recipe.run", &a); !ok {
				return res
			}
			if a.RecipeID == "" {
				return tools.Fail(codeInvalidArgs, "recipe.run: recipeId is required")
			}
			// Merge {...arguments, recipeId} — explicit recipeId wins.
			merged := make(map[string]any, len(a.Arguments)+1)
			for k, v := range a.Arguments {
				merged[k] = v
			}
			merged["recipeId"] = a.RecipeID
			return passthrough(ctx, tctx, "recipe.run", merged, a.RequestKey)
		},
	}
}

// worktreeCreateWithRecipeArgs forwards Daintree's worktree-creation arguments.
// The host owns the source union; recipeId is an optional startup addition.
type worktreeCreateWithRecipeArgs struct {
	Arguments  map[string]any `json:"arguments"`
	RequestKey string         `json:"requestKey,omitempty"`
}

var worktreeCreateWithRecipeSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["arguments"],
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true },
    "requestKey": { "type": "string" }
  }
}`)

func newWorktreeCreateWithRecipeTool() *tools.Tool {
	return &tools.Tool{
		Name:        "worktree.createWithRecipe",
		Description: "Create a git worktree. Recipes are OPTIONAL: default to omitting recipeId unless the user requests a saved setup. Project setup still starts. Read tool.schema for worktree.createWithRecipe; nest source and other host fields inside arguments. No recipe.list needed for plain creation. For custom agent work, create the worktree first, then agentTask.spawnForEdits with its returned worktreeId and full taskPrompt. Supplying recipeId starts saved terminals/prompts immediately. Mutates project state; pass requestKey for an idempotent retry.",
		Risk:        domain.RiskProject,
		Consequence: "Creates a git worktree and starts project setup; launches recipe terminals only if recipeId is supplied.",
		Schema:      worktreeCreateWithRecipeSchema,
		Decode:      tools.StrictDecoder(func() any { return &worktreeCreateWithRecipeArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a worktreeCreateWithRecipeArgs
			if res, ok := strictDecode(args, "worktree.createWithRecipe", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "worktree.createWithRecipe", a.Arguments, a.RequestKey)
		},
	}
}
