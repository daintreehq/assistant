package mcpwrap

import "github.com/daintreehq/daintree-assistant/internal/tools"

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
	return forgeRead("worktree.list", "List Daintree worktrees for the current project.")
}

func newWorktreeGetCurrentTool() *tools.Tool {
	return forgeRead("worktree.getCurrent", "Get the current Daintree worktree context.")
}
