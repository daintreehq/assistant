package commands

import "testing"

// TestIsKnownCommandAnswersCatalogMembership pins the distinction the JSONL stream
// depends on: UICommandResult.Handled is true even for a command nobody has heard of
// (the handler consumed the line and produced an "Unknown command" card), so only a
// registry lookup can tell a SCRIPT that it typed the name wrong.
func TestIsKnownCommandAnswersCatalogMembership(t *testing.T) {
	for _, tt := range []struct {
		line string
		want bool
	}{
		{"/clear", true},
		{"/status", true},
		{"/audit 5", true},  // arguments do not change the command's identity
		{"  /help  ", true}, // surrounding whitespace is not part of the name
		{"/?", true},        // alias of /help
		{"/q", true},        // alias of /quit
		{"/exit", true},     // ditto
		{"/claer", false},   // the typo this field exists to catch
		{"/nonsense", false},
		{"/", false},
		{"", false},
		{"not a command", false},
	} {
		if got := IsKnownCommand(tt.line); got != tt.want {
			t.Errorf("IsKnownCommand(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

// TestUnknownCommandIsStillHandled documents WHY IsKnownCommand had to exist: the UI
// handler's own bit cannot answer this question. If this ever flips, the attached session lost
// its "Unknown command" card — and the JSONL field could then have been the same bit.
func TestUnknownCommandIsStillHandled(t *testing.T) {
	res := HandleUICommand(t.Context(), "/claer", nil)
	if !res.Handled {
		t.Fatal("HandleUICommand no longer handles an unknown command; IsKnownCommand's rationale changed")
	}
	if IsKnownCommand("/claer") {
		t.Fatal("IsKnownCommand and Handled now agree, which defeats the point of the split")
	}
}
