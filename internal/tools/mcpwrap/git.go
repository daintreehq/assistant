package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Git READ (risk read). git.getProjectPulse is the ONLY git read the Daintree MCP
// exposes — it reports commits, branch state, and uncommitted changes for the
// current or a named worktree. Without a wrapper the model could only reach it
// through the system-tier, always-confirmed daintree.call escape hatch, so a plain
// "what's the git state?" forced a confirmation prompt (issue #205). It takes an
// optional opaque {worktreeId?} that Daintree owns, so — exactly like the worktree
// reads — the forgeRead opaque-args read constructor is reused as-is. The target is
// already on the read-only auto-retry allowlist (internal/mcp/tools.go).
func newGitGetProjectPulseTool() *tools.Tool {
	return forgeRead("git.getProjectPulse", "Read a worktree's git pulse — branch state, uncommitted changes, and recent commits — for the current or a specified worktree.")
}

// worktreeIDArgs is the shared {worktreeId} shape for the git-snapshot wrappers.
type worktreeIDArgs struct {
	WorktreeID string `json:"worktreeId"`
}

var worktreeIDSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["worktreeId"],
  "properties": {
    "worktreeId": { "type": "string", "description": "The worktree to operate on." }
  }
}`)

// gitSnapshot builds a git-snapshot wrapper (risk git, always confirmed). Both
// trim worktreeId and reject blank, then forward to the same-named MCP action.
func gitSnapshot(name, desc, consequence string) *tools.Tool {
	return &tools.Tool{
		Name:        name,
		Description: desc,
		Risk:        domain.RiskGit,
		Consequence: consequence,
		Schema:      worktreeIDSchema,
		Decode:      tools.StrictDecoder(func() any { return &worktreeIDArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a worktreeIDArgs
			if res, ok := strictDecode(args, name, &a); !ok {
				return res
			}
			id := strings.TrimSpace(a.WorktreeID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, name+": worktreeId is required")
			}
			return passthrough(ctx, tctx, name, map[string]any{"worktreeId": id}, "")
		},
	}
}

func newGitSnapshotRevertTool() *tools.Tool {
	return gitSnapshot(
		"git.snapshotRevert",
		"Revert a worktree to its last Daintree snapshot, discarding uncommitted changes.",
		"Discards all uncommitted changes in the worktree, reverting to the last snapshot.",
	)
}

func newGitSnapshotDeleteTool() *tools.Tool {
	return gitSnapshot(
		"git.snapshotDelete",
		"Delete the stored Daintree snapshot for a worktree.",
		"Permanently deletes the worktree's stored snapshot.",
	)
}
