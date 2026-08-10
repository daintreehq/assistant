package mcpwrap

import "github.com/daintreehq/assistant/internal/tools"

// Tools returns the typed Daintree-MCP wrapper tools requested for this port:
// recipe.list/run, the worktree reads (worktree.list/getCurrent),
// worktree.createWithRecipe, the forge read wrappers, git.getProjectPulse (read),
// and the workflow MCP passthroughs. Order is
// presentation-only.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newRecipeListTool(),
		newRecipeRunTool(),
		newWorktreeListTool(),
		newWorktreeGetCurrentTool(),
		newWorktreeCreateWithRecipeTool(),

		newForgeListIssuesTool(),
		newForgeGetIssueTool(),
		newForgeListPRsTool(),
		newForgeGetPRTool(),

		newGitGetProjectPulseTool(),

		newWorkflowStartWorkOnIssueTool(deps),
		newWorkflowPrepBranchForReviewTool(),
	}
}
