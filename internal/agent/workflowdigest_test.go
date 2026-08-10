package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// fakeDigestLister returns a canned workflow_state block and counts reads.
type fakeDigestLister struct {
	digests []backend.WorkflowDigest
	calls   int
}

func (f *fakeDigestLister) WorkflowDigests(limit int) []backend.WorkflowDigest {
	f.calls++
	if len(f.digests) > limit {
		return f.digests[:limit]
	}
	return f.digests
}

// The workflow_state block rides EVERY round's turn context (re-read per round,
// like the async ledger) so a mid-turn graph change surfaces on the next round.
func TestWorkflowDigests_RideEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0 → loop
		{Content: "final"}, // round 1
	}}
	lister := &fakeDigestLister{digests: []backend.WorkflowDigest{{
		ID: "wfg_11223344", Goal: "fix the watcher tests", Status: "active",
		Progress: "2/5 nodes done",
	}}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowDigestLister = lister
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "continue the work", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) < 2 {
		t.Fatalf("want a 2-round turn, got %d", len(be.requests()))
	}
	for i := 0; i < 2; i++ {
		got := be.turnAt(i).WorkflowState
		if len(got) != 1 || got[0].ID != "wfg_11223344" || got[0].Progress != "2/5 nodes done" {
			t.Errorf("round %d should carry the workflow digest, got %+v", i, got)
		}
	}
	if lister.calls != 2 {
		t.Fatalf("lister should be read once per round (2 rounds), got %d", lister.calls)
	}
}

// A nil lister (the default, and always when DAINTREE_WORKFLOW_INTELLIGENCE is
// off) omits workflow_state entirely — the wire stays byte-identical to the
// pre-feature request, so a backend without the contract never 422s.
func TestWorkflowDigests_NilListerOmitsBlock(t *testing.T) {
	r := &injectRouter{} // single final round
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	deps.WorkflowDigestLister = nil
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(be.requests()) == 0 {
		t.Fatal("want at least one recorded request")
	}
	if got := be.turnAt(0).WorkflowState; got != nil {
		t.Fatalf("nil lister must omit workflow_state, got %+v", got)
	}
}
