package mcpwrap

import "github.com/daintreehq/assistant/internal/tools"

// Worktree READS (risk read). The Assistant is a conductor — Daintree owns
// worktree creation and the agent lifecycle; these wrappers let the model read
// Daintree's worktree view by name instead of either digging the opaque payload
// out of context.snapshot or going through the system-tier, always-confirmed
// daintree.call escape hatch. Both forward an optional opaque arguments record
// verbatim (Daintree owns the schema and neither takes a required arg), so the
// forgeRead helper — an opaque-args read constructor that is not forge-specific
// — is reused as-is rather than copied. Both target names are already on the
// read-only auto-retry allowlist (internal/mcp/tools.go), so this stays a clean,
// supervisor-tier-reachable read surface.

func newWorktreeListTool() *tools.Tool {
	return forgeRead("worktree.list", "List Daintree's worktrees for this project — each entry's id, path, branch, and whether it is active/main. Read-only passthrough. The CURRENT worktree already rides every round's runtime context, so call this when you need the OTHERS: choosing a spawn target, answering \"which worktrees are ready?\", or resolving a worktreeId — which is a PATH-like id from this list, never a branch name.")
}

func newWorktreeGetCurrentTool() *tools.Tool {
	return forgeRead("worktree.getCurrent", "Get the Daintree worktree the user is currently in — its exact id, path, branch and bound issue/PR. The runtime context already carries a ONE-LINE worktree summary every round, so call this only when you need those exact fields (e.g. a worktreeId to spawn into), after an action that may have switched worktrees, or when the runtime context reported no current worktree.")
}
