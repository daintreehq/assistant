package commands

import (
	"context"
	"testing"
)

// /login is a pure lifecycle command: it must signal the surface to hand the
// terminal back to the launcher (Login flag) without ever touching the App —
// the blocking prompt cannot run under Bubble Tea, and the REPL's app is about
// to be rebuilt. The nil App here is deliberate: it pins "no App access".
func TestLoginCommandReturnsLifecycleSignal(t *testing.T) {
	res := HandleUICommand(context.Background(), "/login", nil)
	if !res.Handled || !res.Login {
		t.Fatalf("/login = %+v, want Handled+Login", res)
	}
	if res.Quit {
		t.Fatal("/login must not read as a plain quit — the launcher restarts the surface")
	}
}

func TestLoginCommandRejectsArguments(t *testing.T) {
	res := HandleUICommand(context.Background(), "/login now", nil)
	if !res.Handled || res.Login {
		t.Fatalf("/login with arguments = %+v, want a usage card, not a login signal", res)
	}
	if res.Title != "Usage" {
		t.Fatalf("title = %q, want Usage", res.Title)
	}
}
