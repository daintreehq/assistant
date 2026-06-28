package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Regressions for the internal/mcp code-review findings (#1 leak on degrade, #2
// stale-call degrades a fresh session, #3 binding-terminal jsonrpc retry, #4
// rand.Rand data race, #5 read-only retry guard, #6 drift-baseline parity).

// countingLow is a LowLevelClient that counts Close() calls and lets a test drive
// per-attempt CallTool errors. Concurrency-safe so the -race retry test can hammer
// it from many goroutines.
type countingLow struct {
	closes     int32 // atomic
	callErr    error // returned by every CallTool when non-nil
	listErr    error
	tools      []rawTool
	serverInfo *ServerInfo
}

func (f *countingLow) ListTools(ctx context.Context) ([]rawTool, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}
func (f *countingLow) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	if f.callErr != nil {
		return rawResult{}, f.callErr
	}
	return rawResult{Text: "ok"}, nil
}
func (f *countingLow) GetServerVersion() *ServerInfo { return f.serverInfo }
func (f *countingLow) Close() error {
	atomic.AddInt32(&f.closes, 1)
	return nil
}
func (f *countingLow) SupportsSubscribe() bool                           { return false }
func (f *countingLow) Subscribe(ctx context.Context, uri string) error   { return nil }
func (f *countingLow) Unsubscribe(ctx context.Context, uri string) error { return nil }
func (f *countingLow) ReadResource(ctx context.Context, uri string) (string, error) {
	return "", nil
}
func (f *countingLow) closeCount() int32 { return atomic.LoadInt32(&f.closes) }

// --- #1: degrade must not leak the low client ---

// TestMarkDegradedClosesLowClient: a real transport failure that degrades the
// connection must Close() the detached low client (so its transport
// goroutines/connections don't leak) and nil it out.
func TestMarkDegradedClosesLowClient(t *testing.T) {
	low := &countingLow{callErr: errors.New("fetch failed")}
	c := newInjected(low)
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "terminal.sendCommand", nil, CallOptions{}); err == nil {
		t.Fatal("expected the transport failure to surface")
	}
	if c.IsConnected() {
		t.Fatal("a real callTool failure must degrade the connection")
	}
	if got := low.closeCount(); got != 1 {
		t.Errorf("degrade must Close the detached low client exactly once, got %d", got)
	}
	// And the low client is detached (a follow-up call is Unavailable, not retried).
	_, err := c.CallTool(context.Background(), "terminal.sendCommand", nil, CallOptions{})
	if !isUnavailable(err) {
		t.Errorf("after degrade the client should be detached → Unavailable, got %v", err)
	}
}

// TestCloseIsIdempotent: calling Close twice closes the underlying client once and
// never panics; a second Close is a no-op (the client was already detached).
func TestCloseIsIdempotent(t *testing.T) {
	low := &countingLow{}
	c := newInjected(low)
	_ = c.Close()
	_ = c.Close()
	if got := low.closeCount(); got != 1 {
		t.Errorf("Close must close the low client exactly once across repeated calls, got %d", got)
	}
}

// --- #2: a stale call from an old session must not degrade a fresh one ---

// TestStaleCallDoesNotDegradeFreshSession: a call that snapshotted the OLD low
// client fails AFTER a successful Reconnect installed a fresh session; the stale
// failure must NOT mark the fresh session disconnected.
func TestStaleCallDoesNotDegradeFreshSession(t *testing.T) {
	oldLow := &countingLow{}
	c := newInjected(oldLow)
	c.Connect(context.Background())

	// Snapshot the old session's generation the way an in-flight call would.
	_, oldGen, err := c.ensure()
	if err != nil {
		t.Fatal(err)
	}

	// A fresh, healthy session replaces the old one (simulating a Reconnect).
	freshLow := &countingLow{}
	c.applyConnected(nil, transportStreamableHTTP) // bumps generation; installs sdkLowLevel(nil)
	_ = freshLow                                   // (the sdkLowLevel wrapper is what's live now)

	// The stale call now reports its failure against the OLD generation.
	c.markDegraded(errors.New("fetch failed"), oldGen)

	if !c.IsConnected() {
		t.Error("a stale failure from a superseded session must NOT degrade the fresh one")
	}
}

// --- #3: binding-terminal jsonrpc errors are terminal, never retried ---

// TestJSONRPCBindingTerminalNotRetriable: a jsonrpc.Error carrying a retriable code
// (-32000/-32001) but a BINDING_STALE / SESSION_BINDING_GONE message is TERMINAL —
// the binding-terminal check must run before the code classification.
func TestJSONRPCBindingTerminalNotRetriable(t *testing.T) {
	cases := []*jsonrpc.Error{
		{Code: -32000, Message: "BINDING_STALE"},
		{Code: -32001, Message: "SESSION_BINDING_GONE: window closed"},
		{Code: -32000, Data: []byte(`{"reason":"BINDING_STALE"}`)},
	}
	for _, e := range cases {
		if isRetriableMcpError(e) {
			t.Errorf("binding-terminal jsonrpc error must not be retriable: %+v", e)
		}
	}
	// Sanity: the same codes WITHOUT a binding marker stay retriable.
	if !isRetriableMcpError(&jsonrpc.Error{Code: -32000, Message: "connection closed"}) {
		t.Error("a plain -32000 should remain retriable")
	}
}

// TestCallToolJSONRPCBindingNotRetried: end-to-end, a read-only call whose transport
// returns a binding-terminal jsonrpc.Error is attempted exactly once despite a
// retry budget.
func TestCallToolJSONRPCBindingNotRetried(t *testing.T) {
	low := &fakeLow{callErrs: []error{
		&jsonrpc.Error{Code: -32000, Message: "BINDING_STALE"},
		&jsonrpc.Error{Code: -32000, Message: "BINDING_STALE"},
	}}
	c := newInjected(low)
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2}); err == nil {
		t.Fatal("expected the binding error to surface")
	}
	if low.callCalls != 1 {
		t.Errorf("a binding-terminal jsonrpc error must not be retried, got %d attempts", low.callCalls)
	}
}

// --- #4: rand.Rand must not race under concurrent retries ---

// TestFullJitterDelayConcurrentNoRace: drive fullJitterDelay (which reads the shared
// jitter PRNG) from many goroutines. Run under `go test -race` to catch an
// unsynchronized *rand.Rand.
func TestFullJitterDelayConcurrentNoRace(t *testing.T) {
	const goroutines = 32
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				_ = fullJitterDelay(j%5, mcpReadRetryPolicy.BaseDelayMs, mcpReadRetryPolicy.MaxDelayMs)
			}
		}()
	}
	wg.Wait()
}

// TestConcurrentRetriesNoRace: concurrent CallTool retries (each sleeping through
// fullJitterDelay) from several goroutines must be race-clean.
func TestConcurrentRetriesNoRace(t *testing.T) {
	const goroutines = 16
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			low := &fakeLow{callErrs: []error{
				&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000},
			}}
			c := newInjected(low)
			c.Connect(context.Background())
			_, _ = c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2})
		}()
	}
	wg.Wait()
}

// --- #5: read-only retry guard ---

// TestRetryGuardMutationSingleShot: a mutating tool (not on the read-only allowlist)
// is forced single-shot even when the caller sets Retries>0, so a transient blip can
// never double-apply a mutation.
func TestRetryGuardMutationSingleShot(t *testing.T) {
	for _, name := range []string{"terminal.sendCommand", "agent.launch", "recipe.run"} {
		low := &fakeLow{callErrs: []error{&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}}}
		c := newInjected(low)
		c.Connect(context.Background())
		if _, err := c.CallTool(context.Background(), name, nil, CallOptions{Retries: 5}); err == nil {
			t.Fatalf("%s: expected the error to surface", name)
		}
		if low.callCalls != 1 {
			t.Errorf("%s: a mutation must not be retried (read-only guard), got %d attempts", name, low.callCalls)
		}
	}
}

// TestRetryGuardReadOnlyHonored: a read-only tool DOES honor the retry budget.
func TestRetryGuardReadOnlyHonored(t *testing.T) {
	low := &fakeLow{callErrs: []error{&jsonrpc.Error{Code: -32000}, nil}, callResult: rawResult{Text: "ok"}}
	c := newInjected(low)
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2}); err != nil {
		t.Fatalf("read-only tool should retry then succeed, got %v", err)
	}
	if low.callCalls != 2 {
		t.Errorf("read-only tool should have retried once, got %d attempts", low.callCalls)
	}
}

// TestReadOnlyAllowlistOnlyDocumentedReads: every name on the read-only allowlist is
// a real documented tool on EITHER MCP server — the Daintree control plane or the docs
// server (guards against a typo'd allowlist entry that would never match and silently
// disable retries).
func TestReadOnlyAllowlistOnlyDocumentedReads(t *testing.T) {
	documented := map[string]struct{}{}
	for _, n := range DocumentedMcpToolNames {
		documented[n] = struct{}{}
	}
	for _, n := range DocsDocumentedToolNames {
		documented[n] = struct{}{}
	}
	for n := range readOnlyToolNames {
		if _, ok := documented[n]; !ok {
			t.Errorf("read-only allowlist entry %q is not a documented MCP tool", n)
		}
	}
}

// (TestDriftBaselineParityWithPrompts was removed: the duplicate documented-tools
// baseline in internal/models/prompts was deleted with the rest of the server-owned
// prompt machinery. internal/mcp keeps the single authoritative DocumentedMcpToolNames
// for drift detection.)
