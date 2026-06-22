package mcp

import (
	"context"
	"errors"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// Client-cancel contract (#81). A call torn down by a caller abort must NOT degrade
// the connection (an abort says nothing about transport health). The client
// expresses caller cancellation through the context (isAborted == context.Canceled),
// so the behaviors are: cancel the parent ctx, make the low client fail, and assert
// the connection stays healthy — while a real (non-abort) failure still degrades.

// TestCallToolAbortDoesNotDegrade: a callTool torn down by a caller abort must NOT
// flip the connection to degraded (a user cancel is not a transport failure).
func TestCallToolAbortDoesNotDegrade(t *testing.T) {
	low := &fakeLow{callErrs: []error{errors.New("request canceled")}}
	c := newInjected(low)
	c.Connect(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // abort before the call

	if _, err := c.CallTool(ctx, "terminal.list", nil, CallOptions{}); err == nil {
		t.Fatal("expected an error from the aborted call")
	}
	if !c.IsConnected() {
		t.Error("a caller abort must NOT degrade the connection")
	}
}

// TestCallToolRealFailureDegrades: without a fired abort, a callTool failure is a
// genuine transport failure → degrade.
func TestCallToolRealFailureDegrades(t *testing.T) {
	low := &fakeLow{callErrs: []error{errors.New("fetch failed")}}
	c := newInjected(low)
	c.Connect(context.Background())

	if _, err := c.CallTool(context.Background(), "terminal.list", nil, CallOptions{}); err == nil {
		t.Fatal("expected a transport error")
	}
	if c.IsConnected() {
		t.Error("a real (non-abort) callTool failure must degrade the connection")
	}
}

// TestForcedListToolsAbortDoesNotDegrade: a forced listTools torn down by a caller
// abort must also leave the connection up (mirrors the listTools branch of #81).
func TestForcedListToolsAbortDoesNotDegrade(t *testing.T) {
	low := &fakeLow{listErr: errors.New("request canceled")}
	c := newInjected(low)
	// Connect's warm uses force=true; with a list error it leaves connected as-is
	// (warmToolCache restores prevConnected). The injected client starts connected.
	c.Connect(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.ListTools(ctx, true); err == nil {
		t.Fatal("expected a list error")
	}
	if !c.IsConnected() {
		t.Error("an aborted forced listTools must NOT degrade the connection")
	}
}

// TestForcedListToolsRealFailureDegrades: a real (non-abort) forced listTools failure
// degrades the connection.
func TestForcedListToolsRealFailureDegrades(t *testing.T) {
	low := &fakeLow{listErr: errors.New("fetch failed")}
	c := newInjected(low)
	c.Connect(context.Background())
	// The warm during Connect swallowed/restored; a direct forced list now degrades.
	if _, err := c.ListTools(context.Background(), true); err == nil {
		t.Fatal("expected a list error")
	}
	if c.IsConnected() {
		t.Error("a real forced listTools failure must degrade the connection")
	}
}

// TestCallToolAbortMidBackoffNoSecondAttempt: a caller abort fired before a retriable
// call still propagates cleanly without a second attempt and without degrading — the
// abort short-circuits the retry path (isAborted gate) entirely.
func TestCallToolAbortMidBackoffNoSecondAttempt(t *testing.T) {
	low := &fakeLow{callErrs: []error{&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}}}
	c := newInjected(low)
	c.Connect(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := c.CallTool(ctx, "x", nil, CallOptions{Retries: 3}); err == nil {
		t.Fatal("expected an error")
	}
	// Even though Retries=3 and the error is retriable, the abort gate stops retries.
	if low.callCalls != 1 {
		t.Errorf("aborted retriable call must not retry: got %d attempts", low.callCalls)
	}
	if !c.IsConnected() {
		t.Error("aborted retriable call must not degrade the connection")
	}
}
