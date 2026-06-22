package mcpwrap

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// fakeMCP records the last forwarded call and returns a canned envelope.
type fakeMCP struct {
	connected bool
	lastName  string
	lastArgs  map[string]any
	result    tools.MCPCallResult
	err       error
}

func (f *fakeMCP) Connected() bool { return f.connected }
func (f *fakeMCP) CallTool(_ context.Context, name string, args map[string]any) (tools.MCPCallResult, error) {
	f.lastName = name
	f.lastArgs = args
	return f.result, f.err
}

func ctxWith(m *fakeMCP) *tools.ToolContext {
	return &tools.ToolContext{Config: config.AppConfig{Tier: domain.TierSystem}, MCP: m, Actor: domain.ActorMain}
}

func findTool(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// recipe.run must force the top-level recipeId to win over a nested
// arguments.recipeId (§8.11) — the most error-prone merge in the family.
func TestRecipeRunRecipeIDWins(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := findTool(Tools(Deps{}), "recipe.run")
	if tool == nil {
		t.Fatal("recipe.run not registered")
	}
	args := json.RawMessage(`{"recipeId":"real","arguments":{"recipeId":"stale","x":1}}`)
	parsed, err := tool.Decode(args)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if got := m.lastArgs["recipeId"]; got != "real" {
		t.Fatalf("recipeId not overridden: got %v", got)
	}
	if got := m.lastArgs["x"]; got != float64(1) {
		t.Fatalf("nested arg not forwarded: got %v", got)
	}
}

// A disconnected MCP must surface MCP_UNAVAILABLE, never reach the transport.
func TestPassthroughDisconnected(t *testing.T) {
	m := &fakeMCP{connected: false}
	tool := findTool(Tools(Deps{}), "forge.listIssues")
	res := tool.Handle(context.Background(), json.RawMessage(`{}`), ctxWith(m))
	if res.Ok || res.Error.Code != codeMCPUnavailable {
		t.Fatalf("expected MCP_UNAVAILABLE, got %+v", res)
	}
}

// forge.getPR rejects a non-positive prNumber (positive-int contract).
func TestForgeGetPRRejectsNonPositive(t *testing.T) {
	m := &fakeMCP{connected: true}
	tool := findTool(Tools(Deps{}), "forge.getPR")
	parsed, err := tool.Decode(json.RawMessage(`{"prNumber":0}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
}

// git.snapshotDelete trims worktreeId and rejects blank, then forwards the
// trimmed value.
func TestGitSnapshotTrims(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "done"}}
	tool := findTool(Tools(Deps{}), "git.snapshotDelete")
	if tool.Risk != domain.RiskGit {
		t.Fatalf("expected git risk, got %s", tool.Risk)
	}
	parsed, err := tool.Decode(json.RawMessage(`{"worktreeId":"  wt1  "}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastArgs["worktreeId"] != "wt1" {
		t.Fatalf("worktreeId not trimmed: %v", m.lastArgs["worktreeId"])
	}
}

// workflow.startWorkOnIssue must NOT forward the assistant-side attachWatcher
// flag to Daintree (§8.10).
func TestStartWorkOnIssueDoesNotForwardAttachWatcher(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
	tool := findTool(Tools(Deps{}), "workflow.startWorkOnIssue")
	parsed, err := tool.Decode(json.RawMessage(`{"arguments":{"issueNumber":7},"attachWatcher":false}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), parsed, ctxWith(m))
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if _, leaked := m.lastArgs["attachWatcher"]; leaked {
		t.Fatal("attachWatcher leaked to Daintree")
	}
	if m.lastArgs["issueNumber"] != float64(7) {
		t.Fatalf("issueNumber not forwarded: %v", m.lastArgs)
	}
}
