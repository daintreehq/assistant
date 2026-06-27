package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// Session-level (issue #286): the open-terminal inventory is fetched ONCE per turn and the
// SAME snapshot rides EVERY round's structured runtime block — never re-fetched per round
// (which would multiply the MCP read budget across a multi-round turn).
func TestOpenTerminals_FetchedOncePerTurnRidesEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	calls := 0
	snap := []backend.OpenTerminal{{ID: "terminal-1", Kind: "agent", AgentState: "running"}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.OpenTerminalsFetcher = func(context.Context) []backend.OpenTerminal { calls++; return snap }
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want a 2-round turn, got %d", len(be.requests()))
	}
	if calls != 1 {
		t.Fatalf("fetcher should run exactly once per turn (not once per round), got %d", calls)
	}
	for i := 0; i < 2; i++ {
		got := be.runtimeAt(i).OpenTerminals
		if len(got) != 1 || got[0].ID != "terminal-1" || got[0].AgentState != "running" {
			t.Errorf("round %d runtime should carry the inventory snapshot, got %+v", i, got)
		}
	}
}

// A nil fetcher (the default and the non-MCP path) simply omits the inventory: no panic,
// the runtime block's OpenTerminals stays empty.
func TestOpenTerminals_NilFetcherOmitsInventory(t *testing.T) {
	r := &injectRouter{} // empty results ⇒ a single final round
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.OpenTerminalsFetcher = nil
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) == 0 {
		t.Fatal("want at least one recorded request")
	}
	if got := be.runtimeAt(0).OpenTerminals; len(got) != 0 {
		t.Fatalf("a nil fetcher must omit the inventory, got %+v", got)
	}
}
