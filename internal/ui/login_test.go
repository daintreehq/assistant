package ui

import (
	"testing"
)

// A /login completion must tear the cockpit down exactly like a quit (the
// pending-confirm/turn cleanup lives in onShutdown) while flagging the final
// model, which is what ui.Run translates into domain.ErrLoginRequested for the
// interactive launcher. The zero Model works headlessly: onShutdown guards its
// nil app/controller.
func TestCommandCompleteLoginQuitsWithFlag(t *testing.T) {
	var m Model
	next, cmd := m.onCommandComplete(CommandCompleteMsg{Login: true})
	nm, ok := next.(Model)
	if !ok {
		t.Fatalf("onCommandComplete returned %T, want Model", next)
	}
	if !nm.loginRequested {
		t.Fatal("Login completion must set loginRequested on the final model")
	}
	if !nm.quitting {
		t.Fatal("Login completion must quit the program (onShutdown path)")
	}
	if cmd == nil {
		t.Fatal("Login completion must return the quit command")
	}
}
