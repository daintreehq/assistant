package mcpwrap

import "github.com/daintreehq/assistant/internal/tools"

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
	return forgeRead("git.getProjectPulse", "Read a worktree's git pulse through Daintree: current branch, uncommitted/staged changes and recent commits. Takes an optional opaque {worktreeId} — omit it for the active worktree. Read-only, no confirmation: this wrapper is why a plain \"what's the git state?\" never needs the system-tier daintree.call escape hatch. For a review-readiness go/no-go verdict use workflow.prepBranchForReview.")
}
