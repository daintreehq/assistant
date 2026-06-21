package mcpwrap

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
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
		Description: "List the available Daintree recipes (worktree/operation templates).",
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
// can never override the explicit one (§8.11).
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
		Description: "Run a Daintree recipe by id. Creates/operates on project state per the recipe.",
		Risk:        domain.RiskProject,
		Consequence: "Runs a Daintree recipe that can create worktrees and mutate project state.",
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
			// Merge {...arguments, recipeId} — explicit recipeId wins (§8.11).
			merged := make(map[string]any, len(a.Arguments)+1)
			for k, v := range a.Arguments {
				merged[k] = v
			}
			merged["recipeId"] = a.RecipeID
			return passthrough(ctx, tctx, "recipe.run", merged, a.RequestKey)
		},
	}
}

// worktreeCreateWithRecipeArgs forwards an opaque arguments record (the recipe
// fields are recipe-defined, so the keys are not modelled).
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
		Description: "Create a worktree from a recipe. Arguments are recipe-defined and forwarded verbatim.",
		Risk:        domain.RiskProject,
		Consequence: "Creates a new git worktree from a recipe template.",
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
