package app

import (
	"testing"
)

// The worktree pin only works if the Session (which WRITES it) and the spawn family
// (which READS it) hold the SAME object. Nothing about the types enforces that: both
// take an interface, and a family wired with a nil pin compiles, passes every unit
// test that injects its own synthetic pin, and silently defaults to nothing.
//
// That is not a hypothetical. This wiring was missing on the first cut, and because
// Daintree now REFUSES an agent-dispatched agent.launch that names no worktree, the
// omission did not degrade to the old ambient behaviour — it turned every ordinary
// spawn into a hard rejection. Unit tests could not see it, because they construct
// agenttaskx.Deps themselves.
//
// So this asserts the wiring end-to-end through the real App: drive the pin the way a
// turn does, and read it back through the accessor the spawn handler actually calls.
func TestWorktreePinIsSharedBetweenSessionAndSpawnFamily(t *testing.T) {
	a := newFlaggedApp(t)
	defer a.Shutdown()

	if a.worktreePin == nil {
		t.Fatal("App.worktreePin is nil — nothing can bind a turn's worktree")
	}
	// The Session's half of the contract: it holds the same object the App built.
	if a.Session == nil {
		t.Fatal("App.Session is nil")
	}

	// Drive the pin exactly as runTurn does, then read it back the way spawn() does.
	a.worktreePin.BeginTurn()
	// fresh=true: a real turn's authoritative read.
	a.worktreePin.Offer("/p/app", "/p/app", "develop", true)

	deps := a.agentTaskDeps()
	if deps.WorktreePin == nil {
		t.Fatal("agenttaskx.Deps.WorktreePin is nil — the spawn family was never given the pin, so every omitted worktreeId stays empty")
	}
	if got := deps.WorktreePin.ID(); got != "/p/app" {
		t.Fatalf("spawn family reads pin id %q, want /p/app — it is holding a DIFFERENT pin from the Session", got)
	}

	// A new turn rebinds through the same shared object.
	a.worktreePin.BeginTurn()
	if got := deps.WorktreePin.ID(); got != "" {
		t.Fatalf("after BeginTurn the spawn family still reads %q — the two halves are not the same object", got)
	}
}
