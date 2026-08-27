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
	// /backend is slow for a different reason and a sharper one: with no argument it
	// ASKS, and the answer arrives as a command on the very loop it would be blocking.
	// Inline, that is not a freeze, it is a deadlock.
	if !IsSlowCommand("/backend") {
		t.Error("/backend is not marked slow — its question would deadlock the command loop")
	}
	// …and ONLY without an argument. The argument form resolves, swaps and returns, and
	// it reconfigures the endpoint — the one thing that has to stay ordered against the
	// turns using it. Moving it to a worker buys nothing and costs that ordering.
	for _, line := range []string{"/backend local", "/backend default", "/backend  https://x.test"} {
		if IsSlowCommand(line) {
			t.Errorf("%q is slow — an explicit target returns promptly and belongs on the loop", line)
		}
	}
	// The contrast matters as much as the flag: marking everything slow would move the
	// whole command surface off the loop and change ordering for commands that never
	// needed it.
	for _, line := range []string{"/status", "/help", "/clear", "/quit"} {
		if IsSlowCommand(line) {
			t.Errorf("%s is marked slow — it returns promptly and belongs on the loop", line)
		}
	}
	// A typo is not slow: the answer it produces is immediate.
	if IsSlowCommand("/lgoin") {
		t.Error("an unknown command reported as slow")
	}
}

// Exclusivity is the SECOND question the registry answers about a command, and the two
// answers must not drift: a command that blocks on a user decision holds the session
// while it does, and one that merely waits on the network does not.
//
// Getting this wrong in either direction is a real failure. Too narrow and the host
// admits a prompt that the reservation then refuses, after the turn has started and been
// echoed. Too broad and `/login` blocks every prompt and every autonomous wake for the
// five minutes it spends waiting on a browser.
func TestOnlyTheCommandsThatOwnTheSessionAreExclusive(t *testing.T) {
	if !IsExclusiveCommand("/backend") {
		t.Error("/backend takes the session while its picker is open, but is not exclusive")
	}
	for _, line := range []string{
		"/backend local", // an explicit target asks nothing and holds nothing
		"/login",         // waits on a browser, holds nothing
		"/logout",
		"/account",
		"/status",
		"/lgoin", // a typo is not a command at all
	} {
		if IsExclusiveCommand(line) {
			t.Errorf("%q is exclusive; it would block prompts and wakes for no reason", line)
		}
	}
	// Everything exclusive is also slow, or the host would run it inline on the very
	// loop that has to deliver its answer.
	for _, c := range COMMAND_REGISTRY {
		line := "/" + c.Name
		if IsExclusiveCommand(line) && !IsSlowCommand(line) {
			t.Errorf("%q owns the session but runs inline — that is a deadlock, not a freeze", line)
		}
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
