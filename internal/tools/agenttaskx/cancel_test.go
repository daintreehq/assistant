package agenttaskx

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Pre-aborted context: the user cancelled before we issued the launch → CANCELLED
// with no agent.launch call at all.
func TestSpawnCancelledBeforeLaunch(t *testing.T) {
	mcp := &scriptMCP{connected: true, launchResult: launchOK("term_1")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := spawn(ctx, Deps{MCP: mcp, DB: newSagaStore()}, &spawnArgs{Title: "do it", TaskPrompt: "make a change"})
	if res.Ok || res.Error.Code != codeCancelled {
		t.Fatalf("expected CANCELLED, got %+v", res)
	}
	if mcp.launchCount() != 0 {
		t.Fatalf("no agent should be launched for a cancelled turn, launched %d", mcp.launchCount())
	}
}

// A launch torn down by the abort (the SDK rejects with a timeout-shaped error
// while the ctx is cancelled) maps to CANCELLED, not AGENT_LAUNCH_FAILED/AMBIGUOUS.
func TestSpawnAbortTornLaunchIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	mcp := &scriptMCP{
		connected:    true,
		launchThrows: true,
		launchErr:    errBoom("Request timed out"),
		onLaunch:     func() { cancel() }, // the user pressed Escape mid-launch
	}
	st := newSagaStore()

	res := spawn(ctx, Deps{MCP: mcp, DB: st}, &spawnArgs{Title: "do it", TaskPrompt: "make a change"})
	if res.Ok || res.Error.Code != codeCancelled {
		t.Fatalf("abort-torn launch should be CANCELLED, got %+v", res)
	}
	// The saga record is marked failed/cancelled, not ambiguous.
	details, _ := res.Error.Details.(map[string]any)
	if id, ok := details["launchId"].(string); ok {
		if rec := st.get(id); rec != nil && rec.Stage != domain.LaunchFailed {
			t.Fatalf("expected failed stage on cancel, got %s", rec.Stage)
		}
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

	res := spawn(ctx, Deps{MCP: mcp, DB: st}, &spawnArgs{
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
