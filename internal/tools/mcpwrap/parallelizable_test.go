package mcpwrap

import (
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// TestForgeReadsParallelizable locks the concurrency opt-in for the read-only forge /
// worktree / git wrapper family. forge.getPR and every forgeRead-built tool
// (forge.listIssues/listPRs/getIssue, git.getProjectPulse, worktree.list/getCurrent) are
// independent, bounded MCP snapshot reads with no ordering dependency on their batch
// siblings, so checking several at once overlaps their round-trips instead of
// serializing — the same win as terminal.extract. Every parallelizable tool must also be
// RiskRead, or the runner's double-gate (ParallelSafe) would (correctly) refuse it.
func TestForgeReadsParallelizable(t *testing.T) {
	cases := []struct {
		label string
		tool  *tools.Tool
	}{
		{"forge.getPR", newForgeGetPRTool()},
		{"forge.getIssue", newForgeGetIssueTool()},
		{"forge.listPRs", newForgeListPRsTool()},
		{"worktree.list", newWorktreeListTool()},
	}
	for _, c := range cases {
		if !c.tool.Parallelizable {
			t.Errorf("%s must be Parallelizable (independent bounded MCP read; batched checks overlap)", c.label)
		}
		if c.tool.Risk != domain.RiskRead {
			t.Errorf("%s is Parallelizable but risk=%s, want read (double-gate would reject it)", c.label, c.tool.Risk)
		}
	}
}
