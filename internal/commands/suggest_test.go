package commands

import (
	"context"
	"strings"
	"testing"
)

// suggest_test.go covers the "did you mean?" suggestion for mistyped slash commands.

func TestSuggestCommand(t *testing.T) {
	cases := map[string]string{
		"statuss": "status", // one extra char
		"hepl":    "help",   // transposition-ish (2 edits)
		"inbx":    "inbox",  // one deletion
		"zzzzzz":  "",       // nothing within 2 edits
		"":        "",       // empty
	}
	for in, want := range cases {
		if got := suggestCommand(in); got != want {
			t.Errorf("suggestCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandleUICommand_UnknownSuggests(t *testing.T) {
	res := HandleUICommand(context.Background(), "/statuss", nil)
	if !res.Handled {
		t.Fatal("unknown command not handled")
	}
	if !strings.Contains(res.Text, "did you mean /status") {
		t.Fatalf("unknown command did not surface a suggestion: %q", res.Text)
	}
}

func TestHandleUICommand_UnknownFarFallsBack(t *testing.T) {
	res := HandleUICommand(context.Background(), "/zzzzzz", nil)
	if strings.Contains(res.Text, "did you mean") {
		t.Fatalf("a far-off command should not guess: %q", res.Text)
	}
	if !strings.Contains(res.Text, "/help") {
		t.Fatalf("fallback should point at /help: %q", res.Text)
	}
}
