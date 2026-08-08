package mcp

import (
	"context"
	"strings"
	"testing"
)

// Drift contract. The baseline is the set of tool names this build DEPENDS ON,
// injected per client (internal/app passes mcpx.WrappedMCPToolNames(); the docs
// client passes DocsToolNames). Drift is missing-only: a depended-on name absent
// from the live surface produces ONE warning each; extra live tools are ignored;
// an empty live list is "unknown", not total drift.
//
// The old hand-maintained 59-name DocumentedMcpToolNames baseline is gone. It
// transcribed the host's whole surface, so it warned about tools nothing here
// called and went stale on every host change — the maintenance burden issue #300
// set out to remove.

// testBaseline is a small stand-in for a real injected baseline.
var testBaseline = []string{"terminal.getStatus", "terminal.sendCommand", "recipe.run", "worktree.list"}

// TestNoBaselineNoDriftCheck: a client given no baseline performs no drift check.
// There is deliberately no implicit default — an omitted baseline must mean
// "nothing to verify", never "fall back to a built-in list of host tools".
func TestNoBaselineNoDriftCheck(t *testing.T) {
	c := newInjected(&fakeLow{tools: []rawTool{{Name: "only.this"}}})
	st := c.Connect(context.Background())
	if st.DriftWarnings != nil {
		t.Errorf("no baseline must mean no drift check, got %v", st.DriftWarnings)
	}
}

// TestDriftSupersetNoWarnings: a live server advertising a superset of the
// depended-on names drifts nothing and stays connected.
func TestDriftSupersetNoWarnings(t *testing.T) {
	tools := make([]rawTool, 0, len(testBaseline)+1)
	for _, n := range testBaseline {
		tools = append(tools, rawTool{Name: n})
	}
	tools = append(tools, rawTool{Name: "extra.live.tool"})
	c := newInjectedWithBaseline(&fakeLow{tools: tools}, testBaseline)
	st := c.Connect(context.Background())
	if !st.Connected {
		t.Fatal("superset must stay connected")
	}
	if st.DriftWarnings != nil {
		t.Errorf("superset must not drift, got %v", st.DriftWarnings)
	}
	if st.ToolCount == nil || *st.ToolCount != len(testBaseline)+1 {
		t.Errorf("expected tool count %d, got %v", len(testBaseline)+1, st.ToolCount)
	}
}

// TestDriftOneWarningPerMissing: dropping K depended-on tools yields exactly K
// warnings, each naming its missing tool, connection still up.
func TestDriftOneWarningPerMissing(t *testing.T) {
	missing := testBaseline[:2]
	skip := map[string]bool{missing[0]: true, missing[1]: true}
	var live []rawTool
	for _, n := range testBaseline {
		if !skip[n] {
			live = append(live, rawTool{Name: n})
		}
	}
	c := newInjectedWithBaseline(&fakeLow{tools: live}, testBaseline)
	st := c.Connect(context.Background())
	if !st.Connected {
		t.Fatal("drift is warning-only: connection must stay up")
	}
	if len(st.DriftWarnings) != len(missing) {
		t.Fatalf("expected exactly %d warnings (one per missing), got %d", len(missing), len(st.DriftWarnings))
	}
	if len(st.DriftWarnings) != len(st.DriftToolNames) {
		t.Fatal("warnings and tool names must be index-aligned")
	}
	for _, name := range missing {
		found := false
		for _, w := range st.DriftWarnings {
			if strings.Contains(w, name) {
				found = true
			}
		}
		if !found {
			t.Errorf("expected a warning naming missing tool %q", name)
		}
	}
}

// TestDriftEmptyLiveIsUnknown: an empty live list is treated as unknown, NOT total
// drift (no warnings).
func TestDriftEmptyLiveIsUnknown(t *testing.T) {
	c := newInjectedWithBaseline(&fakeLow{tools: []rawTool{}}, testBaseline)
	st := c.Connect(context.Background())
	if st.DriftWarnings != nil {
		t.Errorf("empty live list must be unknown, not drift: %v", st.DriftWarnings)
	}
}

// TestDocsBaselineHealth: the docs MCP keeps a hand-written baseline because it is a
// fixed public surface with no annotations to derive from. Guard it stays sane.
func TestDocsBaselineHealth(t *testing.T) {
	if len(DocsToolNames) == 0 {
		t.Fatal("docs baseline must be non-empty")
	}
	seen := map[string]bool{}
	for _, n := range DocsToolNames {
		if n == "" {
			t.Error("docs baseline contains an empty name")
		}
		if seen[n] {
			t.Errorf("duplicate docs baseline entry %q would emit duplicate warnings", n)
		}
		seen[n] = true
	}
}
