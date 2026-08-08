package mcp

import (
	"context"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

func boolPtr(b bool) *bool { return &b }

// TestRetrySafeFromAnnotations pins the predicate that replaced the hand-maintained
// readOnlyToolNames allowlist. The asymmetry is the point: a false negative costs one
// retry, a false positive can double-apply a mutation, so every ambiguous case must
// resolve to false.
func TestRetrySafeFromAnnotations(t *testing.T) {
	cases := []struct {
		name string
		in   *ToolAnnotations
		want bool
	}{
		// The Daintree host's own shape for a `kind: "query"` action.
		{"read-only query", &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, DestructiveHint: boolPtr(false)}, true},
		// Spec-correct servers may omit destructive/idempotent on a read-only tool,
		// since the spec calls them meaningful only when readOnlyHint is false.
		// Requiring them would silently strip retry from a correctly annotated server.
		{"read-only, hints omitted", &ToolAnnotations{ReadOnlyHint: true}, true},
		{"read-only, not idempotent", &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: false, DestructiveHint: boolPtr(false)}, true},

		// A mutation, however it is dressed.
		{"command", &ToolAnnotations{ReadOnlyHint: false, IdempotentHint: false, DestructiveHint: boolPtr(true)}, false},
		{"idempotent but not read-only", &ToolAnnotations{ReadOnlyHint: false, IdempotentHint: true}, false},
		// Self-contradictory server: read-only AND destructive. Trust the dangerous half.
		{"contradictory read-only + destructive", &ToolAnnotations{ReadOnlyHint: true, DestructiveHint: boolPtr(true)}, false},

		// No annotations at all → fail closed (only Options.ReadOnlyFallback may
		// override this, and only per-client).
		{"nil annotations", nil, false},
		{"zero-value annotations", &ToolAnnotations{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := retrySafeFromAnnotations(tc.in); got != tc.want {
				t.Errorf("retrySafeFromAnnotations(%+v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizeSDKToolPreservesAnnotations: the SDK→rawTool reduction must carry the
// annotations through. This is the seam that makes retry classification possible at
// all — dropping it (as the code did before) silently disables every retry.
func TestNormalizeSDKToolPreservesAnnotations(t *testing.T) {
	got := normalizeSDKTool(&sdkmcp.Tool{
		Name:        "terminal.getStatus",
		Description: "read a terminal",
		Annotations: &sdkmcp.ToolAnnotations{
			ReadOnlyHint:    true,
			IdempotentHint:  true,
			DestructiveHint: boolPtr(false),
		},
	})
	if got.Name != "terminal.getStatus" || got.Description != "read a terminal" {
		t.Fatalf("identity fields lost: %+v", got)
	}
	if got.Annotations == nil {
		t.Fatal("annotations dropped — retry classification would silently fail closed")
	}
	if !got.Annotations.ReadOnlyHint || !got.Annotations.IdempotentHint {
		t.Errorf("hints not carried through: %+v", got.Annotations)
	}
	if got.Annotations.DestructiveHint == nil || *got.Annotations.DestructiveHint {
		t.Errorf("destructive hint not carried through as explicit false: %+v", got.Annotations)
	}
	if !retrySafeFromAnnotations(got.Annotations) {
		t.Error("a read-only annotated tool must classify as retry-safe end to end")
	}
}

// TestNormalizeSDKToolNilAnnotations: a server that sends no annotations object must
// leave rawTool.Annotations nil, NOT a zero-value struct — the nil is what
// distinguishes "said nothing" (fallback may apply) from "said it mutates".
func TestNormalizeSDKToolNilAnnotations(t *testing.T) {
	got := normalizeSDKTool(&sdkmcp.Tool{Name: "search"})
	if got.Annotations != nil {
		t.Errorf("absent annotations must stay nil, got %+v", got.Annotations)
	}
}

// TestRetryPolicyDerivedFromAnnotations drives the whole path through the client:
// the live server's annotations, not any local list, decide who may retry.
func TestRetryPolicyDerivedFromAnnotations(t *testing.T) {
	low := &fakeLow{tools: []rawTool{
		readTool("terminal.getStatus"),
		mutatingTool("terminal.sendCommand"),
	}}
	c := newInjected(low)
	c.Connect(context.Background())

	if !c.isRetrySafe("terminal.getStatus") {
		t.Error("a server-annotated read must be retry-safe")
	}
	if c.isRetrySafe("terminal.sendCommand") {
		t.Error("a server-annotated mutation must never be retry-safe")
	}
	if c.isRetrySafe("never.listed") {
		t.Error("a tool absent from the live surface must fail closed")
	}
}

// TestRetryPolicyColdCacheFailsClosed: before any tools/list has landed, nothing is
// retryable. A cold cache must not be read as "everything is safe".
func TestRetryPolicyColdCacheFailsClosed(t *testing.T) {
	c := New(testCfg(), Options{})
	if c.isRetrySafe("terminal.getStatus") {
		t.Error("cold cache must classify every tool single-shot")
	}
}

// TestReadOnlyFallbackOnlyWhenAnnotationsAbsent: the docs-MCP escape hatch is
// strictly narrower than the old allowlist. It may only speak for a tool the server
// annotated not at all, and can never contradict a server that declared a mutation.
func TestReadOnlyFallbackOnlyWhenAnnotationsAbsent(t *testing.T) {
	low := &fakeLow{tools: []rawTool{
		unannotatedTool("search"),          // silent server → fallback applies
		unannotatedTool("not_in_fallback"), // silent server, not listed → stays closed
		mutatingTool("get_page"),           // server SPOKE: mutation. Fallback must lose.
	}}
	c := New(testCfg(), Options{
		ClientOverride:   low,
		ReadOnlyFallback: []string{"search", "get_page"},
	})
	c.Connect(context.Background())

	if !c.isRetrySafe("search") {
		t.Error("fallback must restore retry for an unannotated listed tool")
	}
	if c.isRetrySafe("not_in_fallback") {
		t.Error("an unannotated tool outside the fallback must stay single-shot")
	}
	if c.isRetrySafe("get_page") {
		t.Error("a PRESENT annotation declaring a mutation must beat the fallback")
	}
}

// TestNoFallbackByDefault: the primary Daintree client passes no fallback, so an
// unannotated tool there is single-shot. This is what stops the fallback from
// quietly becoming a new global allowlist.
func TestNoFallbackByDefault(t *testing.T) {
	low := &fakeLow{tools: []rawTool{unannotatedTool("terminal.getStatus")}}
	c := newInjected(low)
	c.Connect(context.Background())
	if c.isRetrySafe("terminal.getStatus") {
		t.Error("without an explicit fallback an unannotated tool must be single-shot")
	}
}
