package app

import (
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/tools"
)

// The registry reports its own walk through the call — validate, park for approval,
// run — down the same channel a handler uses for real substeps. These pin which of the
// two each beat is read as, because getting it wrong is not cosmetic in either
// direction: a lifecycle beat drawn as a substep restates the row's own status in
// lowercase underneath it and stays there for the life of the call, and a substep read
// as lifecycle loses the only account of what a long call is actually doing.
func TestRouteProgress(t *testing.T) {
	auto := func(phase, msg string) tools.ToolProgress {
		return tools.ToolProgress{Phase: phase, Message: msg}
	}

	cases := []struct {
		name      string
		beat      tools.ToolProgress
		parkedIn  bool
		wantState agent.ToolState
		wantSub   string
		parkedOut bool
	}{
		{
			name: "the approval park is a state, not a caption",
			beat: auto(tools.ProgressAwaitingApproval, tools.ProgressMsgAwaitingApproval),
			// The whole point: the row goes amber and says it is blocked on the
			// reader, instead of spinning and claiming to be busy.
			wantState: agent.ToolStateWaiting,
			parkedOut: true,
		},
		{
			name:      "a call released by the reader goes back to running",
			beat:      auto(tools.ProgressRunning, tools.ProgressMsgRunning),
			parkedIn:  true,
			wantState: agent.ToolStateActive,
			parkedOut: false,
		},
		{
			name: "a call that never parked is not re-announced",
			// It was promoted to active by the turn loop before dispatch, so a frame
			// here would cross the wire to say nothing changed.
			beat: auto(tools.ProgressRunning, tools.ProgressMsgRunning),
		},
		{
			name: "validation is a blip on the way to a state already shown",
			beat: auto(tools.ProgressValidating, tools.ProgressMsgValidating),
		},
		{
			name:    "a handler's own words under a lifecycle phase are a substep",
			beat:    tools.ToolProgress{Phase: tools.ProgressRunning, Message: "round 2 of 4"},
			wantSub: "round 2 of 4",
		},
		{
			name:    "a phase the registry never emits is always the handler's",
			beat:    auto(tools.ProgressAwaitingQuestion, "waiting for your choice"),
			wantSub: "waiting for your choice",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parked := tc.parkedIn
			got := routeProgress(tc.beat, &parked)
			if got.state != tc.wantState {
				t.Errorf("state = %q, want %q", got.state, tc.wantState)
			}
			if got.substep != tc.wantSub {
				t.Errorf("substep = %q, want %q", got.substep, tc.wantSub)
			}
			if parked != tc.parkedOut {
				t.Errorf("parked = %v, want %v", parked, tc.parkedOut)
			}
		})
	}
}

// One call, start to finish, through the same routing the host sees: nothing about the
// registry's own progress reaches the transcript, and the approval round trip leaves
// the row where it started rather than parked.
func TestRouteProgressOverAConfirmedCall(t *testing.T) {
	parked := false
	beats := []tools.ToolProgress{
		{Phase: tools.ProgressValidating, Message: tools.ProgressMsgValidating},
		{Phase: tools.ProgressAwaitingApproval, Message: tools.ProgressMsgAwaitingApproval},
		{Phase: tools.ProgressRunning, Message: tools.ProgressMsgRunning},
	}

	var states []agent.ToolState
	var substeps []string
	for _, beat := range beats {
		route := routeProgress(beat, &parked)
		if route.state != "" {
			states = append(states, route.state)
		}
		if route.substep != "" {
			substeps = append(substeps, route.substep)
		}
	}

	if len(substeps) != 0 {
		t.Fatalf("the registry's lifecycle reached the transcript as substeps: %v", substeps)
	}
	want := []agent.ToolState{agent.ToolStateWaiting, agent.ToolStateActive}
	if len(states) != len(want) || states[0] != want[0] || states[1] != want[1] {
		t.Fatalf("lifecycle = %v, want %v", states, want)
	}
	if parked {
		t.Fatal("the call is still recorded as parked after it was released")
	}
}
