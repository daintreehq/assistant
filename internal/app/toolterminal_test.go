package app

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
)

// fakeTerminalMCP is a scripted mcpToolCaller for the FetchOpenTerminals tests: it returns
// a per-tool-name result/error and records the call order so a test can assert which reads
// fired (e.g. that terminal.getStatus is SKIPPED on an empty roster). When block is set,
// CallTool waits on ctx.Done() and returns ctx.Err() — exercising the cancel-based deadline.
type fakeTerminalMCP struct {
	connected bool
	block     bool
	results   map[string]mcp.CallResult
	errs      map[string]error
	calls     []string
}

func (f *fakeTerminalMCP) IsConnected() bool { return f.connected }

func (f *fakeTerminalMCP) CallTool(ctx context.Context, name string, _ map[string]any, _ mcp.CallOptions) (mcp.CallResult, error) {
	f.calls = append(f.calls, name)
	if f.block {
		<-ctx.Done()
		return mcp.CallResult{}, ctx.Err()
	}
	if e := f.errs[name]; e != nil {
		return mcp.CallResult{}, e
	}
	return f.results[name], nil
}

func (f *fakeTerminalMCP) called(name string) bool {
	for _, c := range f.calls {
		if c == name {
			return true
		}
	}
	return false
}

// terminalsResult builds a terminal.list / terminal.getStatus result with the given entry
// maps in the JSON text body (Daintree returns results in the text block, and marshalling
// through JSON makes numeric fields land as float64 exactly as the real wire would).
func terminalsResult(entries ...map[string]any) mcp.CallResult {
	body, _ := json.Marshal(map[string]any{"terminals": entries})
	return mcp.CallResult{Text: string(body)}
}

func newFetchAdapter(f *fakeTerminalMCP) terminalReaderAdapter {
	return terminalReaderAdapter{c: f}
}

// Disconnected MCP ⇒ no inventory and not a single MCP call — the turn must never pay for a
// terminal read when there is no connection.
func TestFetchOpenTerminals_DisconnectedReturnsNil(t *testing.T) {
	f := &fakeTerminalMCP{connected: false}
	if got := newFetchAdapter(f).FetchOpenTerminals(context.Background()); got != nil {
		t.Fatalf("disconnected should yield nil, got %v", got)
	}
	if len(f.calls) != 0 {
		t.Fatalf("disconnected must make no MCP calls, got %v", f.calls)
	}
}

// A terminal.list transport error is best-effort: yield nil, never block or error.
func TestFetchOpenTerminals_ListErrorReturnsNil(t *testing.T) {
	f := &fakeTerminalMCP{
		connected: true,
		errs:      map[string]error{"terminal.list": context.DeadlineExceeded},
	}
	if got := newFetchAdapter(f).FetchOpenTerminals(context.Background()); got != nil {
		t.Fatalf("a list error should yield nil, got %v", got)
	}
	if f.called("terminal.getStatus") {
		t.Fatal("getStatus must not run after a failed list")
	}
}

// An IsError result (not a transport error) is also a failure ⇒ nil.
func TestFetchOpenTerminals_ListIsErrorReturnsNil(t *testing.T) {
	f := &fakeTerminalMCP{
		connected: true,
		results:   map[string]mcp.CallResult{"terminal.list": {IsError: true, Text: `{"terminals":[{"id":"t1"}]}`}},
	}
	if got := newFetchAdapter(f).FetchOpenTerminals(context.Background()); got != nil {
		t.Fatalf("an IsError list result should yield nil, got %v", got)
	}
}

// An empty roster short-circuits: getStatus is SKIPPED entirely (no wasted MCP call).
func TestFetchOpenTerminals_EmptyRosterSkipsStatus(t *testing.T) {
	f := &fakeTerminalMCP{
		connected: true,
		results:   map[string]mcp.CallResult{"terminal.list": terminalsResult()},
	}
	if got := newFetchAdapter(f).FetchOpenTerminals(context.Background()); got != nil {
		t.Fatalf("an empty roster should yield nil, got %v", got)
	}
	if f.called("terminal.getStatus") {
		t.Fatalf("an empty roster must skip getStatus, calls=%v", f.calls)
	}
	if !f.called("terminal.list") {
		t.Fatal("terminal.list should have run")
	}
}

// The happy path: list fields are carried, and the no-output getStatus refreshes the
// volatile agentState/waitingReason/exitCode (the status agentState overrides the list's).
func TestFetchOpenTerminals_MergesStatusOverList(t *testing.T) {
	f := &fakeTerminalMCP{
		connected: true,
		results: map[string]mcp.CallResult{
			"terminal.list": terminalsResult(
				map[string]any{"id": "t1", "kind": "agent", "worktreeId": "/wt/a", "title": "Build", "agentId": "claude", "agentState": "starting"},
				map[string]any{"id": "t2", "kind": "shell", "title": "scratch"},
			),
			"terminal.getStatus": terminalsResult(
				map[string]any{"terminalId": "t1", "agentState": "waiting", "waitingReason": "needs input"},
				map[string]any{"terminalId": "t2", "agentState": "exited", "exitCode": 0},
			),
		},
	}
	got := newFetchAdapter(f).FetchOpenTerminals(context.Background())
	if len(got) != 2 {
		t.Fatalf("want 2 terminals, got %d (%v)", len(got), got)
	}

	t1 := got[0]
	if t1.ID != "t1" || t1.Kind != "agent" || t1.WorktreeID != "/wt/a" || t1.Title != "Build" || t1.AgentID != "claude" {
		t.Fatalf("t1 list fields wrong: %+v", t1)
	}
	if t1.AgentState != "waiting" { // status overrides the list's "starting"
		t.Fatalf("t1 agentState should be refreshed from status to waiting, got %q", t1.AgentState)
	}
	if t1.WaitingReason != "needs input" {
		t.Fatalf("t1 waitingReason should come from status, got %q", t1.WaitingReason)
	}
	if t1.ExitCode != nil {
		t.Fatalf("t1 has no exit code, got %v", *t1.ExitCode)
	}

	t2 := got[1]
	if t2.ID != "t2" || t2.Kind != "shell" || t2.Title != "scratch" {
		t.Fatalf("t2 list fields wrong: %+v", t2)
	}
	if t2.AgentState != "exited" {
		t.Fatalf("t2 agentState should be exited, got %q", t2.AgentState)
	}
	if t2.ExitCode == nil || *t2.ExitCode != 0 {
		t.Fatalf("t2 exitCode should be a non-nil 0 (clean exit is meaningful), got %v", t2.ExitCode)
	}
}

// A getStatus failure degrades, not drops: the list-derived fields survive, the volatile
// status fields are simply absent.
func TestFetchOpenTerminals_StatusErrorKeepsListFields(t *testing.T) {
	f := &fakeTerminalMCP{
		connected: true,
		results: map[string]mcp.CallResult{
			"terminal.list": terminalsResult(
				map[string]any{"id": "t1", "kind": "agent", "agentState": "running"},
			),
		},
		errs: map[string]error{"terminal.getStatus": context.Canceled},
	}
	got := newFetchAdapter(f).FetchOpenTerminals(context.Background())
	if len(got) != 1 {
		t.Fatalf("a status failure must not drop the terminal, got %v", got)
	}
	if got[0].ID != "t1" || got[0].AgentState != "running" {
		t.Fatalf("list fields should survive a status failure, got %+v", got[0])
	}
	if got[0].WaitingReason != "" || got[0].ExitCode != nil {
		t.Fatalf("no status ⇒ no waitingReason/exitCode, got %+v", got[0])
	}
}

// The whole two-call read is bounded by a cancel-based deadline: a hung terminal.list
// returns nil promptly (well under the test's own ceiling) instead of stranding the turn.
func TestFetchOpenTerminals_HungListReturnsViaDeadline(t *testing.T) {
	prev := openTerminalsTimeout
	openTerminalsTimeout = 20 * time.Millisecond
	defer func() { openTerminalsTimeout = prev }()

	f := &fakeTerminalMCP{connected: true, block: true}
	done := make(chan []backend.OpenTerminal, 1)
	go func() {
		done <- newFetchAdapter(f).FetchOpenTerminals(context.Background())
	}()
	select {
	case got := <-done:
		if got != nil {
			t.Fatalf("a hung read should yield nil, got %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("FetchOpenTerminals did not return within the cancel deadline — connection-degrading WithTimeout regression?")
	}
}
