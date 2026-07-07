package world

import (
	"sort"
	"strings"
	"time"
)

// Script models a fake agent's behaviour as a pure function of (elapsed time,
// inputs received). No goroutines, no background ticking: every read computes
// the current state from the wall clock, so the world can never drift, race,
// or leak — and a scenario's timeline is exactly its declaration.
//
// A terminal walks Phases from its spawn instant. If input arrives (the
// orchestrator answers a question / sends a command) and OnInput is non-empty,
// the clock REBASES at the input instant and OnInput takes over — modelling an
// agent that was parked on a question and resumes work once answered. Output is
// cumulative: each phase's Append lands when the phase begins and stays.
type Script struct {
	Phases []Phase
	// OnInput, when non-empty, replaces the remaining timeline the first time
	// input is received. Subsequent inputs are recorded but do not re-rebase.
	OnInput []Phase
}

// Phase is one step of a scripted agent's life.
type Phase struct {
	After         time.Duration // offset from the phase clock origin (spawn or first input)
	State         string        // working | waiting | completed | exited | idle
	WaitingReason string        // "" | "prompt" | "question" — meaningful only with waiting
	ExitCode      *int          // present only for exited
	Append        string        // output appended the moment this phase begins
}

// IntPtr is a tiny helper for Phase.ExitCode literals in scenario files.
func IntPtr(v int) *int { return &v }

// Snapshot is the computed live view of a scripted terminal at one instant —
// the exact facts terminal.getStatus / terminal.getOutput serve.
type Snapshot struct {
	State         string
	WaitingReason string
	ExitCode      *int
	Output        string // full cumulative scrollback
}

// At computes the snapshot at elapsed time since spawn, given the instant of
// the first input (zero if none). Pure; safe under the world's lock.
func (s Script) At(elapsed time.Duration, firstInputAt time.Duration, hasInput bool) Snapshot {
	phases := s.Phases
	origin := time.Duration(0)
	var out strings.Builder
	if hasInput && len(s.OnInput) > 0 && elapsed >= firstInputAt {
		// Pre-input phases contribute output/state up to the input instant,
		// then OnInput takes over on a rebased clock.
		pre := snapshotOf(s.Phases, firstInputAt, 0)
		out.WriteString(pre.Output)
		phases = s.OnInput
		origin = firstInputAt
	}
	snap := snapshotOf(phases, elapsed, origin)
	out.WriteString(snap.Output)
	snap.Output = out.String()
	return snap
}

// snapshotOf walks phases on a clock that started at origin: a phase is live
// when origin+phase.After <= elapsed. State is the last live phase's; output
// is every live phase's Append in order. Phases are stable-sorted by After on
// a copy so an out-of-order scenario declaration can't silently corrupt the
// timeline (the walk breaks at the first not-yet-due phase).
func snapshotOf(phases []Phase, elapsed, origin time.Duration) Snapshot {
	if !sort.SliceIsSorted(phases, func(i, j int) bool { return phases[i].After < phases[j].After }) {
		sorted := make([]Phase, len(phases))
		copy(sorted, phases)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].After < sorted[j].After })
		phases = sorted
	}
	snap := Snapshot{State: "idle"}
	var out strings.Builder
	for _, p := range phases {
		if origin+p.After > elapsed {
			break
		}
		snap.State = p.State
		snap.WaitingReason = p.WaitingReason
		snap.ExitCode = p.ExitCode
		out.WriteString(p.Append)
	}
	snap.Output = out.String()
	if snap.State != "waiting" {
		snap.WaitingReason = ""
	}
	return snap
}
