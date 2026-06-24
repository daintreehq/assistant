package mcp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Client-retry contract (#123). Read-only callers retry a transient
// transport error; mutating callers stay single-shot (Retries defaults to 0); a
// non-transient error is never retried even when retries are allowed; the timeout is
// threaded into the per-attempt deadline; and binding-terminal markers
// (SESSION_BINDING_GONE / BINDING_STALE) are NEVER retried.

// deadlineFakeLow records whether each CallTool saw a context deadline, so we can
// assert the caller timeout is threaded into the per-attempt context.
type deadlineFakeLow struct {
	hadDeadline []bool
	result      rawResult
	err         error
}

func (d *deadlineFakeLow) ListTools(ctx context.Context) ([]rawTool, error) {
	return []rawTool{{Name: "x"}}, nil
}
func (d *deadlineFakeLow) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	_, ok := ctx.Deadline()
	d.hadDeadline = append(d.hadDeadline, ok)
	if d.err != nil {
		return rawResult{}, d.err
	}
	return d.result, nil
}
func (d *deadlineFakeLow) GetServerVersion() *ServerInfo                     { return nil }
func (d *deadlineFakeLow) Close() error                                      { return nil }
func (d *deadlineFakeLow) SupportsSubscribe() bool                           { return false }
func (d *deadlineFakeLow) Subscribe(ctx context.Context, uri string) error   { return nil }
func (d *deadlineFakeLow) Unsubscribe(ctx context.Context, uri string) error { return nil }
func (d *deadlineFakeLow) ReadResource(ctx context.Context, uri string) (string, error) {
	return "", nil
}

// TestCallToolThreadsTimeout: a non-zero CallOptions.Timeout derives a per-attempt
// deadline on the context handed to the low-level callTool.
func TestCallToolThreadsTimeout(t *testing.T) {
	low := &deadlineFakeLow{result: rawResult{Text: "ok"}}
	c := New(testCfg(), Options{ClientOverride: low})
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Timeout: 12345 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if len(low.hadDeadline) != 1 || !low.hadDeadline[0] {
		t.Errorf("expected a deadline threaded into the call ctx, got %v", low.hadDeadline)
	}
}

// TestCallToolDefaultTimeoutBackstop: a zero CallOptions.Timeout still derives a
// per-attempt deadline (defaultCallTimeout) so a server that accepts the request but never
// responds cannot hang the turn forever.
func TestCallToolDefaultTimeoutBackstop(t *testing.T) {
	low := &deadlineFakeLow{result: rawResult{Text: "ok"}}
	c := New(testCfg(), Options{ClientOverride: low})
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "x", nil, CallOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(low.hadDeadline) != 1 || !low.hadDeadline[0] {
		t.Errorf("expected a default deadline when Timeout==0 (hung-server backstop), got %v", low.hadDeadline)
	}
}

// TestCallToolNonTransientNotRetried: a non-transport error is never retried even
// when the read-only caller allows retries — and it degrades the connection.
func TestCallToolNonTransientNotRetried(t *testing.T) {
	low := &fakeLow{callErrs: []error{errors.New("invalid params"), errors.New("invalid params")}}
	c := newInjected(low)
	c.Connect(context.Background())
	_, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2})
	if err == nil || err.Error() != "invalid params" {
		t.Fatalf("expected the non-transient error to surface, got %v", err)
	}
	if low.callCalls != 1 {
		t.Errorf("a non-transient error must not be retried: got %d attempts", low.callCalls)
	}
	if c.IsConnected() {
		t.Error("a non-transient callTool failure must degrade the connection")
	}
}

// TestCallToolBindingTerminalNotRetried: SESSION_BINDING_GONE / BINDING_STALE are
// terminal binding errors — re-issuing against a dead window would be wrong, so they
// are never retried regardless of the retry budget.
func TestCallToolBindingTerminalNotRetried(t *testing.T) {
	for _, marker := range []string{"SESSION_BINDING_GONE: window closed", "BINDING_STALE"} {
		low := &fakeLow{callErrs: []error{errors.New(marker), errors.New(marker)}}
		c := newInjected(low)
		c.Connect(context.Background())
		_, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2})
		if err == nil {
			t.Fatalf("%s: expected an error", marker)
		}
		if low.callCalls != 1 {
			t.Errorf("%s: terminal binding error must not be retried, got %d attempts", marker, low.callCalls)
		}
	}
}

// TestCallToolRetriesExhaustBudget: a transient error retried up to the budget makes
// exactly 1+Retries attempts, then degrades. (jsonrpc -32000 is retriable.)
func TestCallToolRetriesExhaustBudget(t *testing.T) {
	low := &fakeLow{callErrs: []error{
		&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000},
	}}
	c := newInjected(low)
	c.Connect(context.Background())
	if _, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2}); err == nil {
		t.Fatal("expected exhaustion error")
	}
	if low.callCalls != 3 {
		t.Errorf("expected 1 initial + 2 retries = 3 attempts, got %d", low.callCalls)
	}
	if c.IsConnected() {
		t.Error("connection should be degraded after the retry budget is spent")
	}
}

// TestCallToolCancelledDoesNotDegrade: a caller-cancelled CallTool surfaces the error
// WITHOUT degrading the connection (isAborted matches context.Canceled, the degrade path
// is `!aborted`). This is the exact contract the startup-context reads rely on — they
// bound themselves with a CANCEL (not context.WithTimeout) so a slow agentSettings.get /
// worktree.getCurrent can never tear down a just-established connection. Contrast
// TestCallToolNonTransientNotRetried, where a non-abort error DOES degrade — which is
// why a DeadlineExceeded would be wrong for a best-effort startup read.
func TestCallToolCancelledDoesNotDegrade(t *testing.T) {
	low := &fakeLow{callErrs: []error{context.Canceled}}
	c := newInjected(low)
	c.Connect(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // caller abort

	if _, err := c.CallTool(ctx, "agentSettings.get", nil, CallOptions{}); err == nil {
		t.Fatal("expected the cancelled call to surface an error")
	}
	if !c.IsConnected() {
		t.Error("a cancelled (aborted) CallTool must NOT degrade the connection")
	}
}

func testCfg() config.AppConfig { return config.AppConfig{McpURL: "http://x/mcp", McpToken: "t"} }
