package mcp

import (
	"context"
	"strings"
	"testing"
)

// Port of mcpDrift.test.ts (#7). The drift baseline (DocumentedMcpToolNames) is the
// hand-maintained set of tools the live Daintree server is expected to advertise.
// Drift is missing-only: documented names absent from the live surface produce ONE
// warning each; extra live tools are ignored; an empty live list is "unknown" not
// total drift.

// TestDocumentedBaselineHealth: the baseline is a non-empty, dup-free set that
// documents the shipped arming tools and excludes the known negative examples
// (renderer-scoped / non-existent tools that would emit permanent false drift).
func TestDocumentedBaselineHealth(t *testing.T) {
	if len(DocumentedMcpToolNames) == 0 {
		t.Fatal("drift baseline must be non-empty")
	}
	seen := map[string]bool{}
	for _, n := range DocumentedMcpToolNames {
		if n == "" {
			t.Error("baseline contains an empty name")
		}
		if seen[n] {
			t.Errorf("duplicate baseline entry %q would emit duplicate drift warnings", n)
		}
		seen[n] = true
	}
	// Shipped fleet-arming tools ARE on the live surface and must be documented.
	for _, want := range []string{"terminal.arm", "terminal.disarm", "terminal.disarmAll"} {
		if !seen[want] {
			t.Errorf("expected arming tool %q in the drift baseline", want)
		}
	}
	// Negative examples must NOT be in the baseline (they'd warn forever).
	for _, bogus := range []string{
		"terminal.listStatus", "terminal.waitForAny", "terminal.focus",
		"fleet.armByState", "terminal.armByState",
	} {
		if seen[bogus] {
			t.Errorf("baseline must not contain the negative example %q", bogus)
		}
	}
}

// TestDriftSupersetNoWarnings: a live server advertising a superset of the baseline
// drifts nothing and stays connected.
func TestDriftSupersetNoWarnings(t *testing.T) {
	tools := make([]rawTool, 0, len(DocumentedMcpToolNames)+1)
	for _, n := range DocumentedMcpToolNames {
		tools = append(tools, rawTool{Name: n})
	}
	tools = append(tools, rawTool{Name: "extra.live.tool"})
	c := newInjected(&fakeLow{tools: tools})
	st := c.Connect(context.Background())
	if !st.Connected {
		t.Fatal("superset must stay connected")
	}
	if st.DriftWarnings != nil {
		t.Errorf("superset must not drift, got %v", st.DriftWarnings)
	}
	if st.ToolCount == nil || *st.ToolCount != len(DocumentedMcpToolNames)+1 {
		t.Errorf("expected tool count %d, got %v", len(DocumentedMcpToolNames)+1, st.ToolCount)
	}
}

// TestDriftOneWarningPerMissing: dropping K documented tools yields exactly K
// warnings, each naming its missing tool, connection still up.
func TestDriftOneWarningPerMissing(t *testing.T) {
	missing := DocumentedMcpToolNames[:3]
	skip := map[string]bool{missing[0]: true, missing[1]: true, missing[2]: true}
	var live []rawTool
	for _, n := range DocumentedMcpToolNames {
		if !skip[n] {
			live = append(live, rawTool{Name: n})
		}
	}
	c := newInjected(&fakeLow{tools: live})
	st := c.Connect(context.Background())
	if !st.Connected {
		t.Fatal("drift is warning-only: connection must stay up")
	}
	if len(st.DriftWarnings) != 3 {
		t.Fatalf("expected exactly 3 warnings (one per missing), got %d", len(st.DriftWarnings))
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
	c := newInjected(&fakeLow{tools: []rawTool{}})
	st := c.Connect(context.Background())
	if st.DriftWarnings != nil {
		t.Errorf("empty live list must be unknown, not drift: %v", st.DriftWarnings)
	}
}
