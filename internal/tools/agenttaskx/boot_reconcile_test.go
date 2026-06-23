package agenttaskx

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// --- boot-reconcile fakes ---------------------------------------------------

type bootStoreFake struct {
	launches []domain.AgentLaunchRecord
	err      error
}

func (b bootStoreFake) ListConfirmedAgentLaunchesWithTerminal(int) ([]domain.AgentLaunchRecord, error) {
	return b.launches, b.err
}

type bootQueueFake struct {
	published []domain.QueuePublishArgs
	err       error
}

func (q *bootQueueFake) Publish(_ context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	q.published = append(q.published, args)
	return domain.QueueEvent{ID: "evt_" + args.DedupeKey}, q.err
}

func confirmedLaunch(title, agentID, terminalID string) domain.AgentLaunchRecord {
	tid := terminalID
	return domain.AgentLaunchRecord{
		Title: title, AgentID: agentID, Stage: domain.LaunchConfirmed, TerminalID: &tid,
	}
}

func runBoot(mcp MCPClient, store BootStore, q BootQueue) error {
	return BootReconcile(context.Background(), mcp, store, q)
}

// --- tests ------------------------------------------------------------------

func TestBootReconcilePublishesForRunningOrphan(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix OAuth", "claude", "term_7")}}
	q := &bootQueueFake{}

	if err := runBoot(mcp, store, q); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(q.published) != 1 {
		t.Fatalf("want exactly 1 published event, got %d", len(q.published))
	}
	ev := q.published[0]
	if ev.DedupeKey != "orphan-terminal:term_7" {
		t.Errorf("DedupeKey = %q, want orphan-terminal:term_7", ev.DedupeKey)
	}
	if ev.Source != domain.SourceSystem || ev.Severity != domain.SeverityAttention {
		t.Errorf("event should be a system attention event, got source=%s severity=%s", ev.Source, ev.Severity)
	}
	if ev.EpistemicKind != domain.EpistemicInferred {
		t.Errorf("orphan liveness is inferred, got %s", ev.EpistemicKind)
	}
	if len(ev.RecommendedActions) != 1 {
		t.Fatalf("want one recommended action, got %d", len(ev.RecommendedActions))
	}
	act := ev.RecommendedActions[0]
	if act.ToolName != "agentTask.superviseTerminal" || act.Risk != domain.RiskTerminal {
		t.Errorf("recommended action wrong: %+v", act)
	}
	args, ok := act.Args.(map[string]any)
	if !ok || args["terminalId"] != "term_7" {
		t.Errorf("recommended action should carry terminalId=term_7, got %v", act.Args)
	}
}

func TestBootReconcileSkipsDeadTerminal(t *testing.T) {
	// The confirmed saga bound term_7, but Daintree no longer lists it ⇒ not an orphan.
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_other"})}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{}
	if err := runBoot(mcp, store, q); err != nil {
		t.Fatal(err)
	}
	if len(q.published) != 0 {
		t.Fatalf("a terminal absent from terminal.list must not be surfaced, got %d events", len(q.published))
	}
}

func TestBootReconcileNoMCPConnection(t *testing.T) {
	mcp := &scriptMCP{connected: false}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{}
	if err := runBoot(mcp, store, q); err != nil {
		t.Fatalf("disconnected MCP must be a quiet no-op, got %v", err)
	}
	if len(q.published) != 0 || mcp.called("terminal.list") {
		t.Fatalf("disconnected MCP must not list terminals or publish")
	}
}

func TestBootReconcileNoLaunchesSkipsTerminalList(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	q := &bootQueueFake{}
	if err := runBoot(mcp, bootStoreFake{}, q); err != nil {
		t.Fatal(err)
	}
	if len(q.published) != 0 {
		t.Fatalf("no confirmed launches ⇒ no events, got %d", len(q.published))
	}
	if mcp.called("terminal.list") {
		t.Fatalf("with no candidate launches, terminal.list should be skipped")
	}
}

func TestBootReconcileEmptyTerminalList(t *testing.T) {
	mcp := &scriptMCP{connected: true} // zero-value listResult ⇒ no terminals
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{}
	if err := runBoot(mcp, store, q); err != nil {
		t.Fatal(err)
	}
	if len(q.published) != 0 {
		t.Fatalf("an empty terminal list yields no orphans, got %d", len(q.published))
	}
}

func TestBootReconcileStoreErrorReturned(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	store := bootStoreFake{err: errBoom("db gone")}
	if err := runBoot(mcp, store, &bootQueueFake{}); err == nil {
		t.Fatalf("a store read error should propagate for logging")
	}
}

func TestBootReconcilePublishErrorReturned(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{err: errBoom("queue full")}
	if err := runBoot(mcp, store, q); err == nil {
		t.Fatalf("a publish error should propagate for logging")
	}
}

func TestBootReconcileRecommendedActionPreservesLaunchContext(t *testing.T) {
	wt := "wt-A"
	tid := "term_7"
	launch := domain.AgentLaunchRecord{
		Title: "Look around", AgentID: "claude", Stage: domain.LaunchConfirmed,
		TerminalID: &tid, WorktreeID: &wt, Mode: "explore",
	}
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	q := &bootQueueFake{}
	if err := runBoot(mcp, bootStoreFake{launches: []domain.AgentLaunchRecord{launch}}, q); err != nil {
		t.Fatal(err)
	}
	if len(q.published) != 1 {
		t.Fatalf("want 1 event, got %d", len(q.published))
	}
	args, ok := q.published[0].RecommendedActions[0].Args.(map[string]any)
	if !ok {
		t.Fatalf("recommended action args should be a map, got %T", q.published[0].RecommendedActions[0].Args)
	}
	// The re-attach must carry the original mode + worktree, not a blind edit default.
	if args["terminalId"] != "term_7" || args["spawnMode"] != "explore" || args["worktreeId"] != "wt-A" {
		t.Fatalf("re-attach args must preserve launch context, got %v", args)
	}
}

func TestBootReconcileMessageDistinguishesNeverSupervised(t *testing.T) {
	tid := "term_7"
	live := func() *scriptMCP {
		return &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	}

	// Never supervised (watcherId nil) ⇒ must NOT claim supervision ended.
	q1 := &bootQueueFake{}
	if err := runBoot(live(), bootStoreFake{launches: []domain.AgentLaunchRecord{
		confirmedLaunch("Fix", "claude", "term_7"),
	}}, q1); err != nil {
		t.Fatal(err)
	}
	if len(q1.published) != 1 {
		t.Fatalf("want 1 event, got %d", len(q1.published))
	}
	if s := q1.published[0].Summary; !strings.Contains(s, "no supervisor attached") || strings.Contains(s, "supervision ended") {
		t.Fatalf("never-supervised summary wrong: %q", s)
	}

	// Previously supervised (watcherId set) ⇒ wording reflects that supervision ended.
	wid := "wch_1"
	q2 := &bootQueueFake{}
	if err := runBoot(live(), bootStoreFake{launches: []domain.AgentLaunchRecord{
		{Title: "Fix", AgentID: "claude", Stage: domain.LaunchConfirmed, TerminalID: &tid, WatcherID: &wid},
	}}, q2); err != nil {
		t.Fatal(err)
	}
	if len(q2.published) != 1 {
		t.Fatalf("want 1 event, got %d", len(q2.published))
	}
	if s := q2.published[0].Summary; !strings.Contains(s, "supervision ended") {
		t.Fatalf("previously-supervised summary should note supervision ended: %q", s)
	}
}

func TestBootReconcileTerminalListIsError(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: MCPCallResult{IsError: true}}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{}
	if err := runBoot(mcp, store, q); err != nil {
		t.Fatal(err)
	}
	if len(q.published) != 0 {
		t.Fatalf("a terminal.list error response must yield no events, got %d", len(q.published))
	}
}

func TestBootReconcileDedupKeyStableAcrossCalls(t *testing.T) {
	mcp := &scriptMCP{connected: true, listResult: terminalListResult(map[string]any{"id": "term_7"})}
	store := bootStoreFake{launches: []domain.AgentLaunchRecord{confirmedLaunch("Fix", "claude", "term_7")}}
	q := &bootQueueFake{}
	_ = runBoot(mcp, store, q)
	_ = runBoot(mcp, store, q)
	if len(q.published) != 2 {
		t.Fatalf("want 2 publish attempts, got %d", len(q.published))
	}
	// Same per-terminal key each boot ⇒ the queue's own dedup collapses them to one
	// open event (here we just assert the key is stable so dedup can do its job).
	if q.published[0].DedupeKey != q.published[1].DedupeKey {
		t.Fatalf("dedup key must be stable across boots: %q vs %q",
			q.published[0].DedupeKey, q.published[1].DedupeKey)
	}
}
