package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/mcp"

	"github.com/daintreehq/assistant/internal/tools"
)

// mcpToolAdapter is the ONE boundary where a handler's requested wire deadline becomes a
// real transport deadline, so the clamp lives there and is pinned here. These tests
// exercise the pure mapping only — no client, no transport, no network.

func TestToolCallTimeoutClamp(t *testing.T) {
	cases := []struct {
		label string
		in    time.Duration
		want  time.Duration
	}{
		// Zero means "no opinion": the transport's own defaultCallTimeout applies, which
		// is right for every bounded read/write. It must NOT become the ceiling.
		{"zero falls through to the transport default", 0, 0},
		// A negative duration is meaningless as a deadline; honouring it literally would
		// produce an already-expired context and fail every call instantly.
		{"negative falls through to the transport default", -1 * time.Second, 0},
		{"an ordinary budget passes untouched", 30 * time.Second, 30 * time.Second},
		// project.runCheck's largest legitimate request: the host's 1h ceiling plus the
		// settlement margin. It must survive the clamp exactly, or the one caller the
		// mechanism exists for would be silently truncated by it.
		{"the largest legitimate request survives", maxToolCallTimeout, maxToolCallTimeout},
		{"just under the ceiling passes", maxToolCallTimeout - time.Second, maxToolCallTimeout - time.Second},
		// Beyond that is a caller bug, and honouring it would let one call pin an MCP
		// slot for hours — the failure internal/mcp's own default exists to prevent.
		{"beyond the ceiling clamps", 5 * time.Hour, maxToolCallTimeout},
	}
	for _, c := range cases {
		if got := clampMCPCallTimeout(c.in); got != c.want {
			t.Errorf("%s: clampMCPCallTimeout(%v) = %v, want %v", c.label, c.in, got, c.want)
		}
	}
}

// The ceiling must leave real headroom over project.runCheck's own maximum budget, or a
// legitimate hour-long check would be clamped to less than it was promised.
func TestToolCallTimeoutCeilingCoversTheLongestCheck(t *testing.T) {
	const hostMaxCheck = time.Hour // PROJECT_CHECK_MAX_TIMEOUT_MS
	if maxToolCallTimeout <= hostMaxCheck {
		t.Fatalf("maxToolCallTimeout %v must exceed Daintree's %v check ceiling plus settlement margin",
			maxToolCallTimeout, hostMaxCheck)
	}
}

// A compile-time proof that the production adapter still satisfies the seam every tool
// handler holds. Without it, widening tools.MCPClient again would surface as a confusing
// failure at the wiring site rather than here.
var _ tools.MCPClient = mcpToolAdapter{}

// TestToMcpxToolInfosPreservesInputSchema guards the EXACT seam that made issue #311
// unrecoverable, and it has to live here because nothing else can catch it.
//
// The regression was this adapter silently dropping InputSchema while projecting the
// concrete client's descriptors onto the mcpx consumer struct. Every test in
// mcpx/schema_test.go injects an ALREADY-projected mcpx.MCPToolInfo through a fake, which
// is downstream of this function — so deleting the InputSchema field from the mapping
// again would leave that entire suite green while `tool.schema` returned nothing useful
// in production. This is the one test positioned to notice.
func TestToMcpxToolInfosPreservesInputSchema(t *testing.T) {
	// Deliberately nested, with a combinator and a $defs block: the value of tool.schema
	// is that it returns the schema VERBATIM rather than flattening it, so the mapping
	// must carry arbitrary structure, not just a top-level properties bag.
	nested := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"wait": map[string]any{
				"oneOf": []any{
					map[string]any{"type": "string"},
					map[string]any{"type": "object", "properties": map[string]any{"ms": map[string]any{"type": "integer"}}},
				},
			},
		},
		"required": []any{"wait"},
		"$defs":    map[string]any{"id": map[string]any{"type": "string", "minLength": 1}},
	}
	got := toMcpxToolInfos([]mcp.ToolInfo{
		{Name: "a.tool", Description: "desc", InputSchema: nested},
		{Name: "b.tool", Description: "", InputSchema: nil},
		{Name: "c.tool", InputSchema: map[string]any{}},
	})
	if len(got) != 3 {
		t.Fatalf("projected %d infos, want 3", len(got))
	}
	if got[0].Name != "a.tool" || got[0].Description != "desc" {
		t.Errorf("name/description mangled: %+v", got[0])
	}
	// Compare by encoding: map ordering is not stable, but encoding/json sorts keys, so
	// equal encodings mean structurally equal schemas.
	wantJSON, _ := json.Marshal(nested)
	gotJSON, err := json.Marshal(got[0].InputSchema)
	if err != nil {
		t.Fatalf("marshal projected schema: %v", err)
	}
	if string(gotJSON) != string(wantJSON) {
		t.Errorf("InputSchema was altered in projection.\n got: %s\nwant: %s", gotJSON, wantJSON)
	}
	// A nil schema must stay nil rather than becoming an empty object: tool.schema
	// distinguishes "the server advertised {}" from "the server sent nothing", and
	// manufacturing one here would erase that distinction at the seam.
	if got[1].InputSchema != nil {
		t.Errorf("a nil InputSchema became %v; absence must survive the projection", got[1].InputSchema)
	}
	if got[2].InputSchema == nil || len(got[2].InputSchema) != 0 {
		t.Errorf("an empty-object InputSchema must survive as an empty object, got %v", got[2].InputSchema)
	}
}

// tool.schema's "a local typed tool governs this call" annotation is derived from the
// registered wrapper names. mcpx can only see its OWN family, so the mcpwrap wrappers are
// injected through Deps.WrapperNames — and if that wiring is dropped, the annotation goes
// missing for exactly the eight tools issue #367 added, silently.
func TestMcpwrapNamesAreOfferedToSchemaAnnotation(t *testing.T) {
	names := map[string]bool{}
	for _, n := range mcpwrapToolNames() {
		names[n] = true
	}
	for _, want := range []string{
		"project.runCheck", "project.detectRunners", "forge.listIssueComments",
		"agentSessionHistory.list", "browser.getConsoleMessages", "errors.recent",
		"notifications.recent", "worktree.resource.status",
	} {
		if !names[want] {
			t.Errorf("mcpwrapToolNames() omits %q, so tool.schema would hand back its raw schema with no note that a local wrapper governs the call", want)
		}
	}
}
