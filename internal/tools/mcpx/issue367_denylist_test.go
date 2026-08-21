package mcpx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// issue367Wrapped are the raw MCP action names that gained a typed wrapper in issue #367.
// Every one must be refused by daintree.call: the wrapper is the ONLY route, so leaving
// the raw path open would let a caller skip the wrapper's strict decoding and hand
// Daintree a dropped or misspelled argument — the exact failure typed wrappers exist to
// prevent. That the wrapper name equals the raw name changes nothing (forge.getPR is the
// standing precedent).
var issue367Wrapped = []string{
	"project.detectRunners",
	"project.runCheck",
	"forge.listIssueComments",
	"agentSessionHistory.list",
	"browser.getConsoleMessages",
	"errors.recent",
	"notifications.recent",
	"worktree.resource.status",
}

func TestIssue367WrappedActionsAreDeniedRaw(t *testing.T) {
	for _, name := range issue367Wrapped {
		mcp := &fakeMCP{connected: true}
		tool := newCallTool(Deps{MCP: mcp})
		args, _ := json.Marshal(map[string]any{"name": name, "arguments": map[string]any{}})
		res := tool.Handle(context.Background(), args, &tools.ToolContext{})
		if res.Ok {
			t.Errorf("daintree.call(%s) succeeded; it must redirect to the typed wrapper", name)
			continue
		}
		if res.Error == nil || res.Error.Code != codeUseTypedWrapper {
			t.Errorf("daintree.call(%s): want %s, got %+v", name, codeUseTypedWrapper, res.Error)
		}
		// The redirect has to name the wrapper AND its arguments, or the model's next
		// move is to guess them — which is how it ends up back on the raw path.
		if res.Error != nil && !strings.Contains(res.Error.Message, name) {
			t.Errorf("daintree.call(%s) redirect must name the wrapper, got %q", name, res.Error.Message)
		}
		if mcp.lastName != "" {
			t.Errorf("daintree.call(%s) reached the transport as %q; the denylist must short-circuit before forwarding", name, mcp.lastName)
		}
	}
}

// The denylist normalizes case and whitespace, so a padded or case-shifted spelling must
// not slip past it into the raw forward.
func TestIssue367DenylistResistsEvasion(t *testing.T) {
	for _, variant := range []string{
		"  project.runCheck  ", "PROJECT.RUNCHECK", "Errors.Recent",
		"worktree.resource.STATUS", "agentsessionhistory.list", "browser.get\tConsoleMessages",
	} {
		mcp := &fakeMCP{connected: true}
		tool := newCallTool(Deps{MCP: mcp})
		args, _ := json.Marshal(map[string]any{"name": variant})
		res := tool.Handle(context.Background(), args, &tools.ToolContext{})
		if res.Ok || res.Error == nil || res.Error.Code != codeUseTypedWrapper {
			t.Errorf("daintree.call(%q) evaded the denylist: %+v", variant, res.Error)
		}
		if mcp.lastName != "" {
			t.Errorf("daintree.call(%q) reached the transport", variant)
		}
	}
}

// actions.getSchema must stay REACHABLE. tool.schema answers a narrower question (the
// catalog's input schema); actions.getSchema returns a fuller manifest entry, so denying
// it would remove a capability while claiming to redirect it to an equivalent.
func TestActionsGetSchemaIsNotDenied(t *testing.T) {
	if _, denied := denylistLookup[normalizeMCPName("actions.getSchema")]; denied {
		t.Error("actions.getSchema must not be on the daintree.call denylist — tool.schema is not an equivalent replacement for it")
	}
}
