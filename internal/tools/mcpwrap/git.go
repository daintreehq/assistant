package mcpwrap

import "github.com/daintreehq/daintree-assistant/internal/tools"

// Git READ (risk read). git.getProjectPulse is the ONLY git tool the Daintree MCP
// exposes to this wrapper set — it reports commits, branch state, and uncommitted
// changes for the current or a named worktree. Without a wrapper the model could only
// reach it through the system-tier, always-confirmed daintree.call escape hatch, so a
// plain "what's the git state?" forced a confirmation prompt (issue #205). It takes an
// optional opaque {worktreeId?} that Daintree owns, so — exactly like the worktree
// reads — the forgeRead opaque-args read constructor is reused as-is. The target is
// already on the read-only auto-retry allowlist (internal/mcp/tools.go).
//
// (The git.snapshotRevert / git.snapshotDelete wrappers were removed in lockstep with
// Daintree dropping its snapshot tool family — the live server no longer advertises
// them, so wrapping them only produced tool-not-found drift.)
func newGitGetProjectPulseTool() *tools.Tool {
	return forgeRead("git.getProjectPulse", "Read a worktree's git pulse — branch state, uncommitted changes, and recent commits — for the current or a specified worktree.")
}
