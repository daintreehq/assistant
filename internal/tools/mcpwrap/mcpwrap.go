package mcpwrap

import "github.com/daintreehq/daintree-assistant/internal/tools"

// Tools returns the typed Daintree-MCP wrapper tools requested for this port:
// recipe.list/run, worktree.createWithRecipe, the forge read wrappers,
// git.snapshotDelete/Revert, the workflow MCP passthroughs, and
// workflow.focusNextAttention. Order is presentation-only.
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

		newGitSnapshotDeleteTool(),
		newGitSnapshotRevertTool(),

		newWorkflowStartWorkOnIssueTool(deps),
		newWorkflowPrepBranchForReviewTool(),
		newWorkflowFocusNextAttentionTool(),
	}
}
