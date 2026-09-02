package mcpwrap

import "github.com/daintreehq/assistant/internal/tools"

// Tools returns the typed Daintree-MCP wrapper tools requested for this port:
// recipe.list/run, the worktree reads (worktree.list/getCurrent),
// worktree.createWithRecipe, the forge read wrappers, git.getProjectPulse (read),
// the workflow MCP passthroughs, the project check pair, and the observation reads
// (session history, browser console, and the two diagnostic stores). Order is
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
		newForgeGetPRsTool(),
		newForgeGetChecksTool(),

		newGitGetProjectPulseTool(),

		// The project check pair. detectRunners is the ONLY source of the runnerId
		// runCheck requires, so wrapping one without the other would leave the first
		// step of every verification loop on the system-tier daintree.call path.
		newProjectDetectRunnersTool(),
		newProjectRunCheckTool(),

		// Observation reads (issue #367): the questions a supervising turn asks
		// constantly, each previously reachable only through daintree.call.
		newForgeListIssueCommentsTool(),
		newAgentSessionHistoryListTool(),
		newBrowserGetConsoleMessagesTool(),
		newErrorsRecentTool(),
		newNotificationsRecentTool(),
		newWorktreeResourceStatusTool(),

		newWorkflowStartWorkOnIssueTool(deps),
		newWorkflowPrepBranchForReviewTool(),
	}
}
