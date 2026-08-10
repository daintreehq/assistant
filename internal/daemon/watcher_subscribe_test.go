package daemon

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// Covers issue #204: the terminal watcher subscribes to the agent-state resource
// (daintree://agent/{agentId}/state) so transitions are pushed instead of polled,
// widening the poll cadence while subscribed-and-quiet, with getStatus retained as
// the fallback when subscription is unsupported or no agentId is known.

// persistedTerminalState decodes the per-terminal state the watcher finalized into
// the claim's optionsJson, so a test can assert the subscription bookkeeping.
func persistedTerminalState(t *testing.T, store *fakeStore, watcherID, terminalID string) TerminalState {
	t.Helper()
	patch, ok := store.watchPatches[watcherID]
	if !ok {
		t.Fatalf("no claim patch recorded for %s", watcherID)
	}
	raw, _ := patch["optionsJson"].(string)
	var opts watcherOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("optionsJson did not decode: %v (%q)", err, raw)
	}
	return opts.PerTerminal[terminalID]
}

func workingModel() *progModel {
	return &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}
}

func TestWatcher_SubscribesAgentTerminal(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "working", recentOutput: strptr("compiling")},
	})
	mcp.supportsSub = true
	rec := watcherWith("wch_s", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	subs := mcp.subscribeCalls()
	if len(subs) != 1 || subs[0] != "daintree://agent/agt-1/state" {
		t.Fatalf("expected one subscribe to agt-1's state resource, got %v", subs)
	}
	st := persistedTerminalState(t, store, "wch_s", "term-a")
	if !st.Subscribed || st.AgentID != "agt-1" || st.ResourceURI != "daintree://agent/agt-1/state" {
		t.Errorf("subscription state not persisted: %+v", st)
	}
}

func TestWatcher_NoSubscribeWhenUnsupported(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "working", recentOutput: strptr("compiling")},
	})
	// supportsSub defaults false → stay on the polling path.
	rec := watcherWith("wch_u", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	if subs := mcp.subscribeCalls(); len(subs) != 0 {
		t.Fatalf("a server without subscribe support must not be subscribed, got %v", subs)
	}
	// Still polls getStatus (the fallback path is unchanged).
	if len(mcp.callsFor("terminal.getStatus")) != 1 {
		t.Error("unsupported subscription must keep the getStatus poll")
	}
}

func TestWatcher_NoSubscribeWithoutAgentId(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		// No agentId → nothing to subscribe to (a non-agent terminal).
		"term-a": {agentState: "working", recentOutput: strptr("compiling")},
	})
	mcp.supportsSub = true
	rec := watcherWith("wch_n", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	if subs := mcp.subscribeCalls(); len(subs) != 0 {
		t.Fatalf("a terminal without an agentId must not be subscribed, got %v", subs)
	}
}

func TestWatcher_IdempotentSubscribeAcrossTicks(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "working", recentOutput: strptr("compiling")},
	})
	mcp.supportsSub = true
	// Already subscribed in a prior tick (persisted state).
	opts := watcherOptions{PerTerminal: map[string]TerminalState{
		"term-a": {Subscribed: true, AgentID: "agt-1", ResourceURI: "daintree://agent/agt-1/state", Seen: true},
	}}
	rec := watcherWith("wch_i", []string{"term-a"}, withOptions(opts))
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	if subs := mcp.subscribeCalls(); len(subs) != 0 {
		t.Fatalf("an already-subscribed terminal must not re-subscribe, got %v", subs)
	}
}

func TestWatcher_WidensCadenceWhenSubscribedAndQuiet(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr("")},
	})
	mcp.supportsSub = true
	// Steady state: the agent has already worked and is now subscribed + idle.
	// SeenWorking is required before idle counts as "quiet" — a not-yet-started
	// agent stays on the fast cadence so its working transition is caught promptly.
	opts := watcherOptions{PerTerminal: map[string]TerminalState{
		"term-a": {Subscribed: true, AgentID: "agt-1", ResourceURI: "daintree://agent/agt-1/state", Seen: true, SeenWorking: true},
	}}
	rec := watcherWith("wch_w", []string{"term-a"}, withOptions(opts)) // CadenceMs 10_000
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	patch := store.watchPatches["wch_w"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if got := next - last; got != SubscribedReconcileMS {
		t.Errorf("subscribed+quiet should widen cadence to %d, got %d", SubscribedReconcileMS, got)
	}
}

// Negative of the widen test: subscribed + idle but NEVER seen working (just
// spawned, parked at its prompt). The poll must stay on the fast cadence so the
// agent's working transition is caught promptly even if the push is missed.
func TestWatcher_DoesNotWidenCadenceBeforeSeenWorking(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr("")},
	})
	mcp.supportsSub = true
	// Subscribed in a prior tick, but SeenWorking has never latched.
	opts := watcherOptions{PerTerminal: map[string]TerminalState{
		"term-a": {Subscribed: true, AgentID: "agt-1", ResourceURI: "daintree://agent/agt-1/state"},
	}}
	rec := watcherWith("wch_nw", []string{"term-a"}, withOptions(opts)) // CadenceMs 10_000
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	patch := store.watchPatches["wch_nw"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if got := next - last; got != int64(rec.CadenceMs) {
		t.Errorf("not-yet-worked agent must stay on the normal cadence %d, not widen to %d; got %d", rec.CadenceMs, SubscribedReconcileMS, got)
	}
}

func TestWatcher_NormalCadenceWhenWorking(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		// Subscribed, but working (not quiet) → keep sampling at the normal cadence.
		"term-a": {agentID: "agt-1", agentState: "working", recentOutput: strptr("compiling")},
	})
	mcp.supportsSub = true
	rec := watcherWith("wch_k", []string{"term-a"}) // CadenceMs 10_000
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	patch := store.watchPatches["wch_k"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if got := next - last; got != int64(rec.CadenceMs) {
		t.Errorf("a working subscribed terminal must keep the normal cadence %d, got %d", rec.CadenceMs, got)
	}
}

func TestWatcher_TextConditionKeepsNormalCadence(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr(""), tail: "idle"},
	})
	mcp.supportsSub = true
	// A text alert needs fresh scrollback every tick, so cadence must NOT widen.
	rec := watcherWith("wch_t", []string{"term-a"}, withAlert(`{"contains":"FAILED"}`))
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	patch := store.watchPatches["wch_t"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if got := next - last; got != int64(rec.CadenceMs) {
		t.Errorf("a text-condition watcher must keep the normal cadence %d, got %d", rec.CadenceMs, got)
	}
}

func TestAgentStateResourceURI(t *testing.T) {
	if got := agentStateResourceURI("agt-1"); got != "daintree://agent/agt-1/state" {
		t.Errorf("uri = %q, want daintree://agent/agt-1/state", got)
	}
	if got := agentStateResourceURI(""); got != "" {
		t.Errorf("empty agentId must yield empty uri, got %q", got)
	}
}

func TestReadStatusesExtractsAgentID(t *testing.T) {
	body := `{"terminals":[{"terminalId":"t1","agentId":"agt-9","agentState":"working"}]}`
	batch := readStatuses(readCtx(rawMCP{byName: map[string]MCPResult{"terminal.getStatus": {Text: body}}}), []string{"t1"}, false)
	if !batch.Ok {
		t.Fatal("status read should succeed")
	}
	if batch.ByID["t1"].AgentID != "agt-9" {
		t.Errorf("agentId not parsed: %+v", batch.ByID["t1"])
	}
}

func TestWatcher_SubscribeErrorRetriedNextTick(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr("")},
	})
	mcp.supportsSub = true
	mcp.subErr = errors.New("subscribe rejected")
	rec := watcherWith("wch_e", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	// Tick 1: subscribe fails → not subscribed, cadence NOT widened.
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)
	st := persistedTerminalState(t, store, "wch_e", "term-a")
	if st.Subscribed {
		t.Fatal("a failed subscribe must leave Subscribed=false")
	}
	patch := store.watchPatches["wch_e"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if next-last != int64(rec.CadenceMs) {
		t.Errorf("a watcher that failed to subscribe must keep the normal cadence %d, got %d", rec.CadenceMs, next-last)
	}

	// Tick 2: subscribe now succeeds; the persisted (Subscribed=false) state must let
	// it retry rather than be permanently suppressed.
	mcp.subErr = nil
	rec2 := watcherWith("wch_e", []string{"term-a"},
		withOptions(watcherOptions{PerTerminal: map[string]TerminalState{"term-a": st}}))
	store.watchers = []domain.WatcherRecord{rec2}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec2)
	if subs := mcp.subscribeCalls(); len(subs) != 1 || subs[0] != "daintree://agent/agt-1/state" {
		t.Fatalf("the next tick must retry the subscribe, got %v", subs)
	}
}

func TestWatcher_MultiTargetNoWidenWhenOneUnsubscribed(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "waiting", recentOutput: strptr("")},
		"term-b": {agentState: "waiting", recentOutput: strptr("")}, // no agentId → never subscribed
	})
	mcp.supportsSub = true
	rec := watcherWith("wch_m", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)

	patch := store.watchPatches["wch_m"]
	next, _ := patch["nextCheckAt"].(int64)
	last, _ := patch["lastCheckedAt"].(int64)
	if got := next - last; got != int64(rec.CadenceMs) {
		t.Errorf("an un-subscribable target must keep the watcher on the normal cadence %d, got %d", rec.CadenceMs, got)
	}
}

func TestWatcher_UnsubscribesOnStop(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentID: "agt-1", agentState: "exited", exitCode: ptrInt(0), recentOutput: strptr("done")},
	})
	mcp.supportsSub = true
	rec := watcherWith("wch_x", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, workingModel()), rec)
	if !out.Stop {
		t.Fatal("an exited terminal should stop the watcher")
	}
	if un := mcp.unsubscribeCalls(); len(un) != 1 || un[0] != "daintree://agent/agt-1/state" {
		t.Errorf("a stopped watcher must release its subscription, got %v", un)
	}
}
