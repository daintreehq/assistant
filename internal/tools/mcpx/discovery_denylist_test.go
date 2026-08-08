package mcpx

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Finding 5: the daintree.call typed-wrapper denylist must (a) normalize the
// requested name (trim/strip whitespace, case-insensitive) so a padded/case-shifted
// variant can't slip past, and (b) cover the typed-wrapper MCP actions
// (recipe.run, workflow.startWorkOnIssue, …) so the raw forward can't bypass their
// validation. Each of these must redirect to USE_TYPED_WRAPPER, never reach the MCP.
func TestDaintreeCallDenylistEvasionAndCoverage(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	tool := newCallTool(Deps{MCP: mcp})
	dispatch := func(name string) tools.ToolResult {
		raw, _ := json.Marshal(map[string]any{"name": name})
		decoded, err := tool.Decode(raw)
		if err != nil {
			t.Fatalf("decode %q: %v", name, err)
		}
		return tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	}

	cases := []struct {
		name string
		why  string
	}{
		// Whitespace / case variants of an existing denylist entry.
		{"  agent.launch  ", "leading/trailing whitespace"},
		{"Agent.Launch", "case shift"},
		{"agent.launch\t", "trailing tab"},
		{" recipe.run ", "non-breaking-space padding"},
		// Newly-covered typed-wrapper MCP actions.
		{"recipe.run", "recipe.run wrapper"},
		{"recipe.list", "recipe.list wrapper"},
		{"worktree.createWithRecipe", "worktree wrapper"},
		{"workflow.startWorkOnIssue", "workflow wrapper"},
		{"workflow.prepBranchForReview", "workflow wrapper"},
		{"WORKFLOW.STARTWORKONISSUE", "case-shifted wrapper"},
		{"forge.getPR", "forge wrapper"},
		// The forge reads are strictly typed on both sides (issue #299), so the raw
		// action must not be reachable around the wrapper's validation.
		{"forge.listIssues", "forge list wrapper"},
		{"forge.listPRs", "forge list wrapper"},
		{"forge.getIssue", "forge get wrapper"},
		{"FORGE.LISTISSUES", "case-shifted forge wrapper"},
		{"git.getProjectPulse", "git read wrapper"},
	}
	for _, c := range cases {
		res := dispatch(c.name)
		if res.Ok || res.Error.Code != codeUseTypedWrapper {
			t.Errorf("%q (%s) must redirect to USE_TYPED_WRAPPER, got %+v", c.name, c.why, res.Error)
		}
	}
	if mcp.lastName != "" {
		t.Fatalf("a denylisted call must NEVER reach the MCP; forwarded %q", mcp.lastName)
	}

	// A genuinely-unwrapped tool still forwards (the normalization didn't over-match).
	mcp2 := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool2 := newCallTool(Deps{MCP: mcp2})
	raw, _ := json.Marshal(map[string]any{"name": "some.unwrapped.tool"})
	decoded, _ := tool2.Decode(raw)
	if res := tool2.Handle(context.Background(), decoded, &tools.ToolContext{}); !res.Ok {
		t.Fatalf("an unwrapped tool should still forward, got %+v", res.Error)
	}
	if mcp2.lastName != "some.unwrapped.tool" {
		t.Fatalf("unwrapped tool should forward to MCP, called %q", mcp2.lastName)
	}
}
