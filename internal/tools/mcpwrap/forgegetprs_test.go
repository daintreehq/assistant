package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// getPRs decodes then dispatches, the way a real turn does: Decode is where the
// declared schema's STRUCTURAL rules are enforced, and the handler is where the list
// rules are — so a test that skips Decode would be testing half the contract.
func getPRs(t *testing.T, m *fakeMCP, args string) tools.ToolResult {
	t.Helper()
	tool := findTool(Tools(Deps{}), "forge.getPRs")
	parsed, err := tool.Decode(json.RawMessage(args))
	if err != nil {
		// A decode failure is a legitimate outcome for some of these inputs; report it
		// as the INVALID_ARGS the dispatcher would produce rather than failing the test,
		// so the callers below can assert "rejected" without caring which layer said no.
		return tools.Fail(codeInvalidArgs, err.Error())
	}
	return tool.Handle(context.Background(), parsed, ctxWith(m))
}

// numbers renders a prNumbers list of n distinct numbers.
func numbers(n int) string {
	parts := make([]string, 0, n)
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprint(i+1))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// The batch bounds are Daintree's, and StrictDecoder runs NO json-schema engine — the
// declared minItems/maxItems are advisory, so the Go check is the only thing enforcing
// them. Without it the refusal arrives from the forge AFTER the round trip, which is
// the round trip this tool exists to save.
func TestForgeGetPRsEnforcesTheBatchBounds(t *testing.T) {
	for _, tc := range []struct {
		label  string
		count  int
		wantOk bool
	}{
		{"one below the floor", 1, false},
		{"at the floor", forgeGetPRsMin, true},
		{"at the ceiling", forgeGetPRsMax, true},
		{"one over the ceiling", forgeGetPRsMax + 1, false},
	} {
		m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "{}"}}
		res := getPRs(t, m, `{"prNumbers":`+numbers(tc.count)+`}`)
		if res.Ok != tc.wantOk {
			t.Errorf("%s (%d numbers): ok = %v, want %v (%+v)", tc.label, tc.count, res.Ok, tc.wantOk, res.Error)
		}
		if !tc.wantOk && m.lastName != "" {
			t.Errorf("%s: a refused batch must not reach the forge, called %q", tc.label, m.lastName)
		}
	}
}

// A single number is a real question with a real answer, so the refusal names the tool
// that answers it. Recovery here is a DIFFERENT tool, not a longer list, and a message
// that only restated the bound would leave the model to work that out.
func TestForgeGetPRsPointsASingletonAtTheSingularTool(t *testing.T) {
	res := getPRs(t, &fakeMCP{connected: true}, `{"prNumbers":[7]}`)
	if res.Ok {
		t.Fatal("one number is below the batch floor")
	}
	if !strings.Contains(res.Error.Message, "forge.getPR") {
		t.Errorf("refusal should name forge.getPR, got %q", res.Error.Message)
	}
}

// Duplicates are REJECTED, never deduplicated. Collapsing [9,9,9] would let a
// three-number call clear the floor and come back with one result, so the caller's list
// and the reply's would disagree about how many PRs were asked about.
func TestForgeGetPRsRejectsDuplicatesRatherThanCollapsing(t *testing.T) {
	m := &fakeMCP{connected: true}
	res := getPRs(t, m, `{"prNumbers":[9,9,9]}`)
	if res.Ok {
		t.Fatal("a duplicated number must not be silently deduplicated")
	}
	if !strings.Contains(res.Error.Message, "distinct") {
		t.Errorf("refusal should say the numbers must be distinct, got %q", res.Error.Message)
	}
	if m.lastName != "" {
		t.Errorf("nothing should reach the forge, called %q", m.lastName)
	}
}

// Zero and negative numbers are refused for the same reason the singular tool refuses
// them: a PR number is a positive integer, and forwarding a 0 spends a round trip to be
// told so.
func TestForgeGetPRsRejectsNonPositiveNumbers(t *testing.T) {
	for _, args := range []string{`{"prNumbers":[0,4]}`, `{"prNumbers":[4,-1]}`} {
		m := &fakeMCP{connected: true}
		if res := getPRs(t, m, args); res.Ok {
			t.Errorf("%s should be refused", args)
		}
		if m.lastName != "" {
			t.Errorf("%s: nothing should reach the forge, called %q", args, m.lastName)
		}
	}
}

// All three worktree locator spellings reach Daintree, because the underlying forge
// action accepts all three and this tool is on the daintree.call denylist — a spelling
// the wrapper cannot express is a spelling with no path at all.
func TestForgeGetPRsForwardsEveryLocatorSpelling(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "{}"}}
	res := getPRs(t, m, `{"prNumbers":[1,2],"cwd":"/a","worktreeId":"wt_1","worktreePath":"/b"}`)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if m.lastName != "forge.getPRs" {
		t.Fatalf("forwarded to %q, want forge.getPRs", m.lastName)
	}
	for k, want := range map[string]any{"cwd": "/a", "worktreeId": "wt_1", "worktreePath": "/b"} {
		if m.lastArgs[k] != want {
			t.Errorf("%s = %v, want %v", k, m.lastArgs[k], want)
		}
	}
}

// An OMITTED locator is omitted, not sent empty. Daintree resolves the worktree itself
// when none is named, and an empty string is a value that would defeat that.
func TestForgeGetPRsOmitsAbsentLocators(t *testing.T) {
	m := &fakeMCP{connected: true, result: tools.MCPCallResult{Text: "{}"}}
	if res := getPRs(t, m, `{"prNumbers":[1,2]}`); !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	for _, k := range []string{"cwd", "worktreeId", "worktreePath"} {
		if _, present := m.lastArgs[k]; present {
			t.Errorf("%q should be absent when not supplied, got %v", k, m.lastArgs[k])
		}
	}
	// The numbers themselves survive the forward intact and in order.
	arr, ok := m.lastArgs["prNumbers"].([]int)
	if !ok || len(arr) != 2 || arr[0] != 1 || arr[1] != 2 {
		t.Errorf("prNumbers = %#v, want [1 2]", m.lastArgs["prNumbers"])
	}
}

// The batch read carries the same policy as its siblings: read risk (so it costs no
// approval) and parallelizable (so several forge reads in one batch overlap). Both are
// double-gated by the runner, which refuses a parallelizable tool that is not RiskRead.
func TestForgeGetPRsIsAReadAndParallelizable(t *testing.T) {
	tool := findTool(Tools(Deps{}), "forge.getPRs")
	if tool.Risk != domain.RiskRead {
		t.Errorf("risk = %v, want RiskRead", tool.Risk)
	}
	if !tool.Parallelizable {
		t.Error("a bounded independent MCP read must be Parallelizable")
	}
	// A read must not carry a consequence line: that is the mutating wrappers' field,
	// and a consequence on a read would put a warning in front of a listing.
	if tool.Consequence != "" {
		t.Errorf("a read should have no consequence line, got %q", tool.Consequence)
	}
}
