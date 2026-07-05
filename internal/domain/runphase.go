package domain

// RunPhase is the explicit run lifecycle of the active turn. The cockpit's
// liveness is driven from this value, NEVER inferred from "is assistantText
// empty" (that inference is the bug that makes spinners vanish mid-work).
// Shared between the agent loop and the UI.
type RunPhase int

const (
	PhaseReceived         RunPhase = iota // submission accepted
	PhaseAnalyzing                        // waiting for first model output
	PhaseGenerating                       // visible response tokens arriving
	PhaseToolQueued                       // tool batch announced, none running yet
	PhaseToolRunning                      // a tool is executing
	PhaseAwaitingApproval                 // confirmation sheet up
	PhaseAwaitingQuestion                 // multiple-choice question sheet up, blocked on the user
	PhaseIntegrating                      // tools done, model called again, no token yet
	PhaseCancelling                       // Esc pressed, abort propagating
	PhaseComplete
	PhaseFailed
	PhaseCancelled
)

// phaseNames maps each phase to the canonical wire/log name.
var phaseNames = map[RunPhase]string{
	PhaseReceived:         "received",
	PhaseAnalyzing:        "analyzing",
	PhaseGenerating:       "generating",
	PhaseToolQueued:       "tool_queued",
	PhaseToolRunning:      "tool_running",
	PhaseAwaitingApproval: "awaiting_approval",
	PhaseAwaitingQuestion: "awaiting_question",
	PhaseIntegrating:      "integrating",
	PhaseCancelling:       "cancelling",
	PhaseComplete:         "complete",
	PhaseFailed:           "failed",
	PhaseCancelled:        "cancelled",
}

// String returns the canonical snake_case name of the phase.
func (p RunPhase) String() string {
	if n, ok := phaseNames[p]; ok {
		return n
	}
	return "unknown"
}

// IsTerminal reports whether the phase ends the turn lifecycle.
func (p RunPhase) IsTerminal() bool {
	switch p {
	case PhaseComplete, PhaseFailed, PhaseCancelled:
		return true
	default:
		return false
	}
}
