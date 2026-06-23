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
// arguments.recipeId — the most error-prone merge in the family.
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

// The opaque-args reads (worktree.list / worktree.getCurrent / git.getProjectPulse)
// forward to the right Daintree action name, carry RiskRead (supervisor-tier
// reachable, no confirmation — unlike system-tier daintree.call), and pass the MCP
// envelope's structuredContent straight through to the model. An omitted arguments
// record forwards an empty (non-nil) map, matching forgeRead's nil-guard, and an
// opaque nested arguments object is forwarded verbatim.
func TestOpaqueArgReadsForwardAndPassThrough(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
	}{
		{"worktree.list", "worktree.list"},
		{"worktree.getCurrent", "worktree.getCurrent"},
		{"git.getProjectPulse", "git.getProjectPulse"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := findTool(Tools(Deps{}), tc.target)
			if tool == nil {
				t.Fatalf("%s not registered", tc.target)
			}
			if tool.Risk != domain.RiskRead {
				t.Fatalf("%s risk: got %s want %s", tc.target, tool.Risk, domain.RiskRead)
			}

			// Omitted arguments → MCP receives an empty, non-nil map.
			m := &fakeMCP{connected: true, result: tools.MCPCallResult{
				Text:              "raw",
				StructuredContent: map[string]any{"worktrees": []any{"wt1"}},
			}}
			parsed, err := tool.Decode(json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			res := tool.Handle(context.Background(), parsed, ctxWith(m))
			if !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if m.lastName != tc.target {
				t.Fatalf("forwarded to %q, want %q", m.lastName, tc.target)
			}
			if m.lastArgs == nil || len(m.lastArgs) != 0 {
				t.Fatalf("omitted arguments should forward an empty non-nil map, got %#v", m.lastArgs)
			}
			payload, ok := res.Result.(map[string]any)
			if !ok {
				t.Fatalf("result not a map: %#v", res.Result)
			}
			sc, ok := payload["structuredContent"].(map[string]any)
			if !ok {
				t.Fatalf("structuredContent not passed through: %#v", payload["structuredContent"])
			}
			if _, ok := sc["worktrees"]; !ok {
				t.Fatalf("structuredContent contents not preserved: %#v", sc)
			}

			// An opaque nested arguments object is forwarded verbatim.
			m2 := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "ok"}}
			parsed2, err := tool.Decode(json.RawMessage(`{"arguments":{"filter":"active"}}`))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if res := tool.Handle(context.Background(), parsed2, ctxWith(m2)); !res.Ok {
				t.Fatalf("expected ok, got %+v", res.Error)
			}
			if m2.lastArgs["filter"] != "active" {
				t.Fatalf("opaque nested arg not forwarded: %#v", m2.lastArgs)
			}
		})
	}
}

// A Daintree-side refusal (IsError) maps to MCP_TOOL_ERROR, and the strict
// top-level schema rejects an unknown key locally (only a nested arguments object
// is allowed) before any transport call.
func TestOpaqueArgReadsErrorPaths(t *testing.T) {
	for _, name := range []string{"worktree.list", "worktree.getCurrent", "git.getProjectPulse"} {
		tool := findTool(Tools(Deps{}), name)

		// Daintree refuses → MCP_TOOL_ERROR (not a transport failure).
		m := &fakeMCP{connected: true, result: tools.MCPCallResult{IsError: true, Text: "not found"}}
		res := tool.Handle(context.Background(), json.RawMessage(`{}`), ctxWith(m))
		if res.Ok || res.Error.Code != codeMCPToolError {
			t.Fatalf("%s: expected MCP_TOOL_ERROR on IsError, got %+v", name, res)
		}

		// An unknown top-level key is rejected by the strict decoder — the schema
		// only admits a nested arguments object.
		if _, err := tool.Decode(json.RawMessage(`{"filter":"active"}`)); err == nil {
			t.Fatalf("%s: expected strict-decode error for unknown top-level key", name)
		}
	}
}

// A disconnected MCP must surface MCP_UNAVAILABLE for the opaque-args reads too,
// never reaching the transport.
func TestOpaqueArgReadsDisconnected(t *testing.T) {
	for _, name := range []string{"worktree.list", "worktree.getCurrent", "git.getProjectPulse"} {
		m := &fakeMCP{connected: false}
		tool := findTool(Tools(Deps{}), name)
		res := tool.Handle(context.Background(), json.RawMessage(`{}`), ctxWith(m))
		if res.Ok || res.Error.Code != codeMCPUnavailable {
			t.Fatalf("%s: expected MCP_UNAVAILABLE, got %+v", name, res)
		}
		if m.lastName != "" {
			t.Fatalf("%s: MCP must not be called when disconnected, called %q", name, m.lastName)
		}
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
// flag to Daintree.
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
