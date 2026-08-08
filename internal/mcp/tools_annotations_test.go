package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
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

// TestAnnotatedMutationIsSingleShot: the end-to-end guard that a LISTED, explicitly
// mutation-annotated tool makes exactly one call even with a retry budget and a
// retriable transport error. Distinct from the unlisted-tool case: this proves the
// client honours a present "this mutates" annotation rather than merely failing
// closed on an unknown name.
func TestAnnotatedMutationIsSingleShot(t *testing.T) {
	low := &fakeLow{
		tools: []rawTool{mutatingTool("terminal.sendCommand")},
		callErrs: []error{
			&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000},
		},
	}
	c := newInjected(low)
	c.Connect(context.Background())

	if _, err := c.CallTool(context.Background(), "terminal.sendCommand", nil, CallOptions{Retries: 2}); err == nil {
		t.Fatal("expected the transport error to surface")
	}
	if low.callCalls != 1 {
		t.Errorf("a mutation must never be replayed, got %d attempts", low.callCalls)
	}
}

// TestRetryClassificationClearedOnReconnect is the regression guard for a stale
// verdict outliving the listing it came from.
//
// Reconnect (like markDegraded) invalidates the tool cache. If the retry
// classification survived that, a tool the OLD session called read-only could still
// be replayed against a NEW session where it is a mutation. The classification is
// gated on the same cacheWarm flag the cache uses, so an invalidated cache is
// necessarily an un-classified one.
func TestRetryClassificationClearedOnReconnect(t *testing.T) {
	low := &fakeLow{tools: []rawTool{readTool("terminal.getStatus")}}
	c := newInjected(low)
	c.Connect(context.Background())
	if !c.isRetrySafe("terminal.getStatus") {
		t.Fatal("precondition: the warmed read should be retry-safe")
	}

	// Drive the REAL invalidation path rather than assigning the fields by hand: a
	// hand-rolled reset would pass even if markDegraded stopped clearing them, which
	// is precisely the regression this guards.
	_, gen, err := c.ensure()
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	c.markDegraded(errors.New("fetch failed"), gen)

	if c.isRetrySafe("terminal.getStatus") {
		t.Error("a classification must not outlive the tool cache it was derived from")
	}
	c.mu.Lock()
	cache, safe, warm := c.toolCache, c.retrySafe, c.cacheWarm
	c.mu.Unlock()
	if cache != nil || safe != nil || warm {
		t.Errorf("markDegraded must clear the cache trio, got cache=%v retrySafe=%v warm=%v", cache, safe, warm)
	}
}

// TestRetryClassificationDoesNotCrossSessions is the guard for the subtler hole:
// applyConnected installs a NEW session but deliberately leaves the old tool cache
// in place (so the UI does not blink to zero tools while the new one warms). The
// cache is therefore still "warm", yet it describes a session that is gone — and a
// remembered read-only verdict must NOT be honoured against the new one.
func TestRetryClassificationDoesNotCrossSessions(t *testing.T) {
	low := &fakeLow{tools: []rawTool{readTool("terminal.getStatus")}}
	c := newInjected(low)
	c.Connect(context.Background())
	if !c.isRetrySafe("terminal.getStatus") {
		t.Fatal("precondition: the warmed read should be retry-safe")
	}

	// A new session is installed without touching the cache.
	c.applyConnected(nil, transportStreamableHTTP)

	c.mu.Lock()
	warm := c.cacheWarm
	c.mu.Unlock()
	if !warm {
		t.Fatal("precondition: applyConnected is expected to leave the cache warm")
	}
	if c.isRetrySafe("terminal.getStatus") {
		t.Error("a verdict from the previous session must not apply to a new one")
	}
}

// TestStaleClassificationCannotSurviveColdCache: even if the map were somehow left
// populated (a future edit forgetting to clear it), the cacheWarm gate must still
// refuse to serve it. This pins the STRUCTURAL guarantee rather than the bookkeeping.
func TestStaleClassificationCannotSurviveColdCache(t *testing.T) {
	low := &fakeLow{tools: []rawTool{readTool("terminal.getStatus")}}
	c := newInjected(low)
	c.Connect(context.Background())

	// Cache invalidated, but the map deliberately left behind.
	c.mu.Lock()
	c.toolCache = nil
	c.cacheWarm = false
	c.mu.Unlock()

	if c.isRetrySafe("terminal.getStatus") {
		t.Error("cacheWarm must gate the classification, not just the map being cleared")
	}
}

// TestRefreshReclassifiesReadToMutation: a forced refresh that re-declares a tool as
// mutating must revoke its retry budget. This is the "server changed under us"
// case — a host rollout that corrects a wrong annotation.
func TestRefreshReclassifiesReadToMutation(t *testing.T) {
	low := &fakeLow{tools: []rawTool{readTool("worktree.list")}}
	c := newInjected(low)
	ctx := context.Background()
	c.Connect(ctx)
	if !c.isRetrySafe("worktree.list") {
		t.Fatal("precondition: should start retry-safe")
	}

	low.mu.Lock()
	low.tools = []rawTool{mutatingTool("worktree.list")}
	low.mu.Unlock()
	if _, err := c.ListTools(ctx, true); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}

	if c.isRetrySafe("worktree.list") {
		t.Error("a refresh declaring the tool mutating must revoke retry")
	}
}

// TestRefreshDroppingToolRevokesRetry: a tool that disappears from the surface
// entirely must also lose its budget, not keep the last verdict.
func TestRefreshDroppingToolRevokesRetry(t *testing.T) {
	low := &fakeLow{tools: []rawTool{readTool("worktree.list"), readTool("terminal.list")}}
	c := newInjected(low)
	ctx := context.Background()
	c.Connect(ctx)

	low.mu.Lock()
	low.tools = []rawTool{readTool("terminal.list")}
	low.mu.Unlock()
	if _, err := c.ListTools(ctx, true); err != nil {
		t.Fatalf("forced refresh: %v", err)
	}

	if c.isRetrySafe("worktree.list") {
		t.Error("a tool dropped from the live surface must lose its retry budget")
	}
	if !c.isRetrySafe("terminal.list") {
		t.Error("a tool still present must keep it")
	}
}
