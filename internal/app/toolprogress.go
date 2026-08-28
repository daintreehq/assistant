package app

import (
	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/tools"
)

// progressRoute is what one progress beat MEANS to an embedded host: a lifecycle
// promotion for the call's row, an in-tool substep drawn under it, or nothing.
//
// The registry emits both kinds down one channel. It reports its own walk through
// validate → approve → run as progress beats, and a handler reports real substeps
// ("round 2/4") the same way. A host that draws every beat as a substep therefore
// drew the registry's state machine as a line of lowercase text UNDER a row it had
// already labelled from tool:state — "Waiting on 3 terminals … Running" with
// "running" beneath it — and, because the last beat is the one that sticks, left it
// there for the life of the call.
//
// So the beats are read for what they are instead of printed for what they say.
type progressRoute struct {
	// state promotes the call's row. Empty ⇒ no promotion.
	state agent.ToolState
	// substep is the line drawn under the row. Empty ⇒ nothing to draw.
	substep string
}

// routeProgress classifies one beat. `parked` is the caller's per-call memory of
// whether this call is currently sitting on the approval gate — a pointer because the
// answer for the "running" beat depends on how the call got there.
//
// The approval park is the reason this exists rather than a plain filter. "waiting" is
// in the wire vocabulary, the host draws an amber row and an approval count for it,
// and NOTHING ever emitted it: the turn loop drives queued→active→done/failed and
// cannot see the gate, so a call parked on the user reported itself as running until
// they answered. The one beat that knew was the automatic "waiting for approval",
// which was being drawn as a substep — the right fact on the wrong channel.
func routeProgress(p tools.ToolProgress, parked *bool) progressRoute {
	// A handler's own message under any phase is information the host has nowhere
	// else, so it stays a substep. Only the registry's automatic wording is read as
	// lifecycle.
	if !p.Lifecycle() {
		return progressRoute{substep: p.Message}
	}
	switch p.Phase {
	case tools.ProgressAwaitingApproval:
		*parked = true
		return progressRoute{state: agent.ToolStateWaiting}
	case tools.ProgressRunning:
		// Only for a call coming BACK from the gate. Every other call is already
		// active — the turn loop promoted it before dispatch — so re-announcing it
		// would put a frame on the wire per call to say nothing changed.
		if *parked {
			*parked = false
			return progressRoute{state: agent.ToolStateActive}
		}
	}
	// "validating" is a sub-100ms blip on the way to a state the row already shows.
	return progressRoute{}
}
