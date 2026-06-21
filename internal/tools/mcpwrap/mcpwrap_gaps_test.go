package mcpwrap

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

func mcpResult(text string) tools.MCPCallResult { return tools.MCPCallResult{Text: text} }

// Every wrapper carries the risk class that drives tier-gating, and every
// MUTATING wrapper (git/project/external) carries a non-empty user-facing
// consequence line.
func TestWrapperRiskClassesAndConsequences(t *testing.T) {
	wantRisk := map[string]domain.RiskClass{
		"recipe.list":                  domain.RiskRead,
		"recipe.run":                   domain.RiskProject,
		"worktree.createWithRecipe":    domain.RiskProject,
		"forge.listIssues":             domain.RiskRead,
		"forge.getIssue":               domain.RiskRead,
		"forge.listPRs":                domain.RiskRead,
		"forge.getPR":                  domain.RiskRead,
		"git.snapshotDelete":           domain.RiskGit,
		"git.snapshotRevert":           domain.RiskGit,
		"workflow.startWorkOnIssue":    domain.RiskExternal,
		"workflow.prepBranchForReview": domain.RiskExternal,
		"workflow.focusNextAttention":  domain.RiskUI,
	}
	mutating := map[domain.RiskClass]bool{
		domain.RiskGit: true, domain.RiskProject: true, domain.RiskExternal: true,
	}
	all := Tools(Deps{})
	seen := map[string]bool{}
	for _, tool := range all {
		want, ok := wantRisk[tool.Name]
		if !ok {
			continue
		}
		seen[tool.Name] = true
		if tool.Risk != want {
			t.Errorf("%s risk: got %s want %s", tool.Name, tool.Risk, want)
		}
		if mutating[tool.Risk] && strings.TrimSpace(tool.Consequence) == "" {
			t.Errorf("%s is mutating (%s) but has no consequence line", tool.Name, tool.Risk)
		}
	}
	for name := range wantRisk {
		if !seen[name] {
			t.Errorf("expected wrapper %s to be registered", name)
		}
	}
}

// A whitespace-only required field is rejected locally — never forwarded to MCP.
func TestGitSnapshotRejectsBlankWorktreeLocally(t *testing.T) {
	m := &fakeMCP{connected: true, result: mcpResult("ok")}
	tool := findTool(Tools(Deps{}), "git.snapshotDelete")
	parsed, err := tool.Decode(json.RawMessage(`{"worktreeId":"   "}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("blank worktreeId should be INVALID_ARGS, got %+v", res)
	}
	if m.lastName != "" {
		t.Fatalf("MCP must not be called for a locally-rejected field, called %q", m.lastName)
	}
}

// recipe.run forwards requestKey as a dedicated passthrough param, separate from
// the merged arguments payload.
func TestRecipeRunForwardsRequestKeyAsParam(t *testing.T) {
	m := &fakeMCP{connected: true, result: mcpResult("ok")}
	tool := findTool(Tools(Deps{}), "recipe.run")
	parsed, err := tool.Decode(json.RawMessage(`{"recipeId":"r","arguments":{"x":1},"requestKey":"rk-7"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastArgs["requestKey"] != "rk-7" {
		t.Fatalf("requestKey not forwarded as a param: %v", m.lastArgs)
	}
	if m.lastArgs["recipeId"] != "r" || m.lastArgs["x"] != float64(1) {
		t.Fatalf("merged arguments not forwarded: %v", m.lastArgs)
	}
}
