package commands

import "testing"

// The account commands exist, are reachable by the names people actually type, and are
// marked slow.
//
// Reachability is the half worth pinning. Before these landed, a `/login` typed into the
// native panel took the unknown-command path and came back as "/login isn't a command" —
// while the engine held a complete OAuth implementation the session had no way to call.
func TestAccountCommandsAreKnownUnderEveryNamePeopleType(t *testing.T) {
	for _, line := range []string{
		"/login", "/signin", "/sign-in",
		"/logout", "/signout", "/sign-out",
		"/account", "/whoami",
	} {
		if !IsKnownCommand(line) {
			t.Errorf("%s is not a known command — it would render as a typo", line)
		}
	}
}

// Slow is what routes a command to a worker instead of the host's command loop. Getting
// it wrong on /login means the panel freezes for the whole browser round trip: no
// interrupt, no approval, no progress, indistinguishable from a hung engine.
func TestSignInIsSlowAndOrdinaryCommandsAreNot(t *testing.T) {
	for _, line := range []string{"/login", "/signin", "/logout", "/account", "/whoami"} {
		if !IsSlowCommand(line) {
			t.Errorf("%s is not marked slow — it would block the host command loop", line)
		}
	}
	// The contrast matters as much as the flag: marking everything slow would move the
	// whole command surface off the loop and change ordering for commands that never
	// needed it.
	for _, line := range []string{"/status", "/help", "/backend", "/clear", "/quit"} {
		if IsSlowCommand(line) {
			t.Errorf("%s is marked slow — it returns promptly and belongs on the loop", line)
		}
	}
	// A typo is not slow: the answer it produces is immediate.
	if IsSlowCommand("/lgoin") {
		t.Error("an unknown command reported as slow")
	}
}

// /quit must never become slow. A slow command runs inside the worker group that
// teardown joins, so a quitting one would deadlock on its own join — the host guards
// against it at runtime, and this keeps the guard from ever being reached.
func TestQuitIsNotSlow(t *testing.T) {
	for _, line := range []string{"/quit", "/q", "/exit"} {
		if IsSlowCommand(line) {
			t.Fatalf("%s is marked slow — tearing down from a command worker deadlocks", line)
		}
	}
}
