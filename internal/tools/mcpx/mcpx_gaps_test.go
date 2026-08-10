package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// The daintree.call denylist redirect NAMES the typed wrapper for every wrapped
// raw tool, and never forwards the raw call.
func TestDaintreeCallDenylistNamesWrapper(t *testing.T) {
	mcp := &fakeMCP{connected: true}
	tool := newCallTool(Deps{MCP: mcp})
	for raw, wrapper := range map[string]string{
		"agent.launch":         "agentTask.spawnForEdits",
		"terminal.sendCommand": "terminal.sendCommand",
		"terminal.rename":      "terminal.rename",
		"terminal.arm":         "terminal.arm",
		"git.getProjectPulse":  "git.getProjectPulse",
	} {
		decoded, _ := tool.Decode(json.RawMessage(`{"name":"` + raw + `"}`))
		res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeUseTypedWrapper {
			t.Fatalf("%s should be redirected, got %+v", raw, res)
		}
		if !strings.Contains(res.Error.Message, wrapper) {
			t.Fatalf("redirect for %s must name %q, got %q", raw, wrapper, res.Error.Message)
		}
		if mcp.lastName == raw {
			t.Fatalf("raw %s must never be forwarded", raw)
		}
	}
}

// tool.search tokenizes the query: every whitespace-separated word must appear in
// the name or description (AND), word order and filler words don't matter, and
// name hits rank first. Regression for session ses_d88b9482, where the old
// whole-query substring match returned 0 for natural multi-word queries like
// "rename terminal" (the description says "Rename the terminal tab", but never in
// that word order) — costing the model several wasted discovery rounds before bare
// "rename" finally hit. NOTE tool.search reads the RAW Daintree catalog, so a word
// the raw description lacks (e.g. "title") still won't match here — that gap is
// covered separately by the terminal.rename typed wrapper + manual + skill, which
// remove the need to search at all.
func TestToolSearchTokenizesQuery(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		{Name: "terminal.rename", Description: "Rename the terminal tab. If name is provided, renames programmatically. Otherwise opens the rename dialog."},
		{Name: "terminal.summarize", Description: "Summarize a terminal's tail with the small model."},
		{Name: "terminal.close", Description: "Close a terminal; it moves to the trash."},
	}}
	tool := newSearchTool(Deps{MCP: mcp})

	names := func(query string) []string {
		args, _ := json.Marshal(map[string]string{"query": query})
		decoded, err := tool.Decode(json.RawMessage(args))
		if err != nil {
			t.Fatalf("decode %q: %v", query, err)
		}
		res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("search %q failed: %+v", query, res)
		}
		raw, ok := res.Result.(map[string]any)
		if !ok {
			t.Fatalf("search %q result shape: %T", query, res.Result)
		}
		matches, _ := raw["matches"].([]map[string]any)
		out := make([]string, 0, len(matches))
		for _, m := range matches {
			out = append(out, m["name"].(string))
		}
		return out
	}

	// Order/filler-insensitive AND match: "rename" is in the description, "terminal"
	// in the name. The whole-query substring match returned 0 for "rename terminal"
	// (no description contains that phrase verbatim); tokenization finds it.
	for _, q := range []string{"rename terminal", "terminal rename", "rename"} {
		if got := names(q); len(got) != 1 || got[0] != "terminal.rename" {
			t.Errorf("%q: want [terminal.rename], got %v", q, got)
		}
	}

	// AND semantics: a word absent from every tool (here "title", which the raw
	// description genuinely lacks) yields zero matches rather than a loose OR hit.
	if got := names("rename title"); len(got) != 0 {
		t.Errorf("AND semantics: 'rename title' should yield 0 (no tool has 'title'), got %v", got)
	}

	// An empty / whitespace-only query has no terms; it must return zero, NOT match
	// every tool (a vacuous AND over no terms) and dump the catalog.
	for _, q := range []string{"", "   "} {
		if got := names(q); len(got) != 0 {
			t.Errorf("empty query %q should yield 0, got %v", q, got)
		}
	}
}

// Name hits rank above description-only hits even when the description-only match
// comes FIRST in the raw catalog order: a stable sort promotes the name-hit while
// preserving raw order within each group.
func TestToolSearchRanksNameHitsFirst(t *testing.T) {
	mcp := &fakeMCP{connected: true, toolList: []MCPToolInfo{
		// description-only "rename" match, listed BEFORE the name-hit.
		{Name: "workflow.startWorkOnIssue", Description: "Begin an issue; may rename the branch."},
		{Name: "terminal.rename", Description: "Rename the terminal tab."},
	}}
	tool := newSearchTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"query":"rename"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	raw := res.Result.(map[string]any)
	matches := raw["matches"].([]map[string]any)
	if len(matches) != 2 {
		t.Fatalf("want 2 matches, got %d", len(matches))
	}
	if matches[0]["name"] != "terminal.rename" {
		t.Errorf("name-hit terminal.rename should rank first, got order %q, %q",
			matches[0]["name"], matches[1]["name"])
	}
}

// terminal.rename forwards {terminalId, name} to the Daintree terminal.rename MCP
// action, and rejects a blank terminalId or name BEFORE any MCP call — Daintree
// treats both as optional (blank name opens the dialog, blank terminalId hits the
// focused tab), neither of which an orchestrator wants.
func TestTerminalRenameWrapper(t *testing.T) {
	// Blank args never reach the MCP.
	for _, bad := range []string{`{"terminalId":"","name":"x"}`, `{"terminalId":"t1","name":"  "}`} {
		mcp := &fakeMCP{connected: true}
		tool := newTerminalRenameTool(Deps{MCP: mcp})
		decoded, err := tool.Decode(json.RawMessage(bad))
		if err != nil {
			t.Fatalf("decode %s: %v", bad, err)
		}
		res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok {
			t.Errorf("%s should be rejected", bad)
		}
		if mcp.lastName != "" {
			t.Errorf("%s must not reach MCP, forwarded %q", bad, mcp.lastName)
		}
	}

	// Valid args forward verbatim to terminal.rename.
	mcp := &fakeMCP{connected: true}
	tool := newTerminalRenameTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"terminalId":"t1","name":"merge pipeline"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("valid rename failed: %+v", res)
	}
	if mcp.lastName != "terminal.rename" {
		t.Errorf("forwarded to %q, want terminal.rename", mcp.lastName)
	}
	if mcp.lastArgs["terminalId"] != "t1" || mcp.lastArgs["name"] != "merge pipeline" {
		t.Errorf("args not forwarded verbatim: %v", mcp.lastArgs)
	}
}

// A disconnected MCP must point the model at /reconnect — the recovery command
// that works in both the REPL and the cockpit (issue #211). Covers the shared
// passthrough plus the three discovery tools that report disconnection.
func TestMCPUnavailableErrorsNameReconnect(t *testing.T) {
	disc := &fakeMCP{connected: false}

	// The shared passthrough every typed wrapper delegates to.
	pres := passthrough(context.Background(), disc, "worktree.list", nil, "")
	if pres.Ok || pres.Error.Code != codeMCPUnavailable {
		t.Fatalf("disconnected passthrough should be MCP_UNAVAILABLE, got %+v", pres)
	}
	if !strings.Contains(pres.Error.Message, "/reconnect") {
		t.Errorf("passthrough hint must name /reconnect: %q", pres.Error.Message)
	}

	// The discovery tools that surface a disconnected MCP to the model.
	cases := []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"daintree.listTools", newListToolsTool(Deps{MCP: disc}), `{}`},
		{"tool.search", newSearchTool(Deps{MCP: disc}), `{"query":"x"}`},
		{"daintree.call", newCallTool(Deps{MCP: disc}), `{"name":"worktree.list"}`},
	}
	for _, tc := range cases {
		decoded, err := tc.tool.Decode(json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s decode: %v", tc.name, err)
		}
		res := tc.tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeMCPUnavailable {
			t.Fatalf("%s disconnected should be MCP_UNAVAILABLE, got %+v", tc.name, res)
		}
		if !strings.Contains(res.Error.Message, "/reconnect") {
			t.Errorf("%s hint must name /reconnect: %q", tc.name, res.Error.Message)
		}
	}
}

// A connection that reports Connected()==true but then errors mid-RPC (a stale
// link dropping during ListTools/search) is also MCP_UNAVAILABLE, and must carry
// the same /reconnect recovery hint as the up-front disconnected check.
func TestMCPStaleConnectionErrorsNameReconnect(t *testing.T) {
	stale := &fakeMCP{connected: true, listErr: errors.New("stream reset")}

	cases := []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"daintree.listTools", newListToolsTool(Deps{MCP: stale}), `{}`},
		{"tool.search", newSearchTool(Deps{MCP: stale}), `{"query":"x"}`},
	}
	for _, tc := range cases {
		decoded, err := tc.tool.Decode(json.RawMessage(tc.args))
		if err != nil {
			t.Fatalf("%s decode: %v", tc.name, err)
		}
		res := tc.tool.Handle(context.Background(), decoded, &tools.ToolContext{})
		if res.Ok || res.Error.Code != codeMCPUnavailable {
			t.Fatalf("%s mid-RPC failure should be MCP_UNAVAILABLE, got %+v", tc.name, res)
		}
		if !strings.Contains(res.Error.Message, "/reconnect") {
			t.Errorf("%s stale-connection hint must name /reconnect: %q", tc.name, res.Error.Message)
		}
	}
}

// daintree.call forwards requestKey as a dedicated top-level param (not buried in
// the arguments payload).
func TestDaintreeCallForwardsRequestKeyAsParam(t *testing.T) {
	mcp := &fakeMCP{connected: true, result: MCPCallResult{Text: "ok"}}
	tool := newCallTool(Deps{MCP: mcp})
	decoded, _ := tool.Decode(json.RawMessage(`{"name":"worktree.list","arguments":{"a":1},"requestKey":"rk-9"}`))
	res := tool.Handle(context.Background(), decoded, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if mcp.lastArgs["requestKey"] != "rk-9" {
		t.Fatalf("requestKey not forwarded as a param: %v", mcp.lastArgs)
	}
	if mcp.lastArgs["a"] != float64(1) {
		t.Fatalf("arguments payload not forwarded: %v", mcp.lastArgs)
	}
}
