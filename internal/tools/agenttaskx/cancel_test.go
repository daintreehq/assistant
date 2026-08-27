package agenttaskx

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// Pre-aborted context: the user cancelled before we issued the launch → CANCELLED
// with no agent.launch call at all.
func TestSpawnCancelledBeforeLaunch(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := spawnMain(ctx, Deps{MCP: mcp, DB: newSagaStore()}, &spawnArgs{Title: "do it", TaskPrompt: "make a change"})
	if res.Ok || res.Error.Code != codeCancelled {
		t.Fatalf("expected CANCELLED, got %+v", res)
	}
	if mcp.launchCount() != 0 {
		t.Fatalf("no agent should be launched for a cancelled turn, launched %d", mcp.launchCount())
	}
}

// A launch torn down by the abort (the SDK rejects with a timeout-shaped error
// while the ctx is cancelled) is AMBIGUOUS, not CANCELLED: the request may have
// reached Daintree before the client aborted, so claiming "nothing started" could
// be untruthful. With no reconciling terminal the saga stays `ambiguous`, so a
// same-args retry reconciles instead of double-launching.
func TestSpawnAbortTornLaunchUnresolvedIsAmbiguous(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mcp := &scriptMCP{
		connected:    true,
		launchThrows: true,
		launchErr:    errBoom("Request timed out"),
		listResult:   terminalListResult(), // reconcile finds nothing
		onLaunch:     func() { cancel() },  // the user pressed Escape mid-launch
	}
	st := newSagaStore()

	res := spawnMain(ctx, Deps{MCP: mcp, DB: st}, &spawnArgs{Title: "do it", TaskPrompt: "make a change"})
	if res.Ok || res.Error.Code != codeAgentLaunchAmbiguous {
		t.Fatalf("abort-torn launch should be AMBIGUOUS, got %+v", res)
	}
	if !strings.Contains(res.Error.Message, "cancelled") {
		t.Fatalf("the ambiguity message should say the turn was cancelled: %q", res.Error.Message)
	}
	launchID := res.Error.Details.(map[string]any)["launchId"].(string)
	if got := st.get(launchID).Stage; got != domain.LaunchAmbiguous {
		t.Fatalf("expected ambiguous stage on mid-flight cancel, got %s", got)
	}
	// The reconcile read must still have run — exactly once, and on a DETACHED,
	// still-LIVE ctx: the turn's ctx is already dead, so a reconcile ridden on it
	// would abort before reaching the wire.
	if len(mcp.listCtxErrs) != 1 {
		t.Fatalf("want exactly 1 bounded reconcile read, got %d", len(mcp.listCtxErrs))
	}
	if mcp.listCtxErrs[0] != nil {
		t.Fatalf("the reconcile read ran on a dead ctx (%v); it must be detached from the cancelled turn", mcp.listCtxErrs[0])
	}
}

// The truthful half of the mid-flight-cancel contract: when the aborted request DID
// reach Daintree and the agent is running, the bounded detached reconcile recovers
// the terminal and the result reports the running agent — never CANCELLED.
func TestSpawnAbortTornLaunchReconcilesRunningAgent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mcp := &scriptMCP{
		connected:    true,
		launchThrows: true,
		launchErr:    errBoom("Request timed out"),
		listResult:   terminalListResult(map[string]any{"id": "term_9", "name": "Claude: do it", "agentId": "claude"}),
		onLaunch:     func() { cancel() },
	}
	st := newSagaStore()

	res := spawnMain(ctx, Deps{MCP: mcp, DB: st}, &spawnArgs{Title: "do it", TaskPrompt: "make a change"})
	if !res.Ok {
		t.Fatalf("a launch that survived the abort must be reported as running, got %+v", res.Error)
	}
	if res.Result.(map[string]any)["terminalId"] != "term_9" {
		t.Fatalf("terminalId: %v", res.Result)
	}
}

// A post-launch bookkeeping failure (watcher insert throws) while the signal
// happens to be aborted must NOT be masked as CANCELLED — the agent IS running.
func TestSpawnPostLaunchFailureNotMaskedAsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mcp := &scriptMCP{
		connected:    true,
		launchResult: launchOK("term_1"),
		onLaunch:     func() { cancel() }, // abort lands AFTER a successful launch
	}
	st := newSagaStore()
	st.insertWatcherErr = errBoom("sqlite boom")

	res := spawnMain(ctx, Deps{MCP: mcp, DB: st}, &spawnArgs{
		Title: "do it", TaskPrompt: "make a change", Watcher: &spawnWatcher{Create: true},
	})
	if !res.Ok {
		t.Fatalf("a running agent must not be hidden by an incidental abort: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if m["terminalId"] != "term_1" {
		t.Fatalf("terminalId: %v", m["terminalId"])
	}
	if _, has := m["watcherId"]; has {
		t.Fatal("watcherId should be absent after attach failure")
	}
	if w, _ := m["watcherWarning"].(string); !strings.Contains(w, "watcher could not be attached: sqlite boom") {
		t.Fatalf("watcherWarning: %v", m["watcherWarning"])
	}
}
