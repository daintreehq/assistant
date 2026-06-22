package daemon

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestTailSnippet(t *testing.T) {
	tests := []struct {
		name              string
		in                string
		maxLines, maxChar int
		want              string
	}{
		{"empty", "", 2, 200, ""},
		{"whitespace only", "  \n\t\n   ", 2, 200, ""},
		{"single line", "Proceed? (y/n)", 2, 200, "Proceed? (y/n)"},
		{"trims surrounding blanks", "\n\nProceed? (y/n)\n\n", 2, 200, "Proceed? (y/n)"},
		{"last two non-empty joined", "first\nsecond\nthird", 2, 200, "second | third"},
		{"skips interior blanks", "Question:\n\nAnswer y or n", 2, 200, "Question: | Answer y or n"},
		{"keeps order oldest-to-newest", "a\nb\nc\nd", 3, 200, "b | c | d"},
		{"caps to last maxChars bytes", strings.Repeat("x", 250), 2, 200, "…" + strings.Repeat("x", 200)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tailSnippet(tc.in, tc.maxLines, tc.maxChar); got != tc.want {
				t.Errorf("tailSnippet(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// A long earlier line must NOT crowd out the actual question (the last line):
// left-truncation keeps the most recent bytes, so the question survives.
func TestTailSnippet_LongLeadingLinePreservesQuestion(t *testing.T) {
	tail := strings.Repeat("x", 250) + "\nProceed? (y/n)"
	got := tailSnippet(tail, 2, 200)
	if !strings.Contains(got, "Proceed? (y/n)") {
		t.Errorf("snippet must preserve the trailing question, got %q", got)
	}
	if len(got) > 200+len("…") {
		t.Errorf("snippet should respect the byte cap, got %d bytes: %q", len(got), got)
	}
}

// Left-truncation must land on a valid UTF-8 boundary, never split a rune.
func TestTailSnippet_TruncationIsRuneSafe(t *testing.T) {
	// "aaaaa☃bbb" = 5 + 3 + 3 = 11 bytes; capping to the last 5 bytes lands
	// inside the 3-byte ☃, so the partial leading continuation bytes must be
	// dropped, leaving "…bbb" — valid UTF-8 with no orphaned bytes.
	got := tailSnippet("aaaaa☃bbb", 1, 5)
	if !utf8.ValidString(got) {
		t.Errorf("snippet must be valid UTF-8, got %q", got)
	}
	if got != "…bbb" {
		t.Errorf("expected partial rune dropped → \"…bbb\", got %q", got)
	}
}

func TestRecommendedActionsFor_BlockedOffersReply(t *testing.T) {
	for _, class := range []domain.WatcherClassification{domain.ClassWaitingForInput, domain.ClassPermissionPrompt} {
		actions := recommendedActionsFor(class, "t1")
		if len(actions) != 2 {
			t.Fatalf("%s: expected focus+reply (2 actions), got %d: %+v", class, len(actions), actions)
		}
		// terminal.focus stays FIRST (look-before-you-leap).
		if actions[0].ToolName != "terminal.focus" || actions[0].RequiresConfirmation {
			t.Errorf("%s: action[0] should be a no-confirm terminal.focus, got %+v", class, actions[0])
		}
		reply := actions[1]
		if reply.ToolName != "terminal.sendCommand" {
			t.Errorf("%s: action[1] should be terminal.sendCommand, got %q", class, reply.ToolName)
		}
		if reply.Risk != domain.RiskTerminal || !reply.RequiresConfirmation {
			t.Errorf("%s: reply action must be RiskTerminal + RequiresConfirmation, got %+v", class, reply)
		}
		args, ok := reply.Args.(map[string]any)
		if !ok {
			t.Fatalf("%s: reply args should be a map, got %T", class, reply.Args)
		}
		if args["terminalId"] != "t1" {
			t.Errorf("%s: reply args terminalId should be t1, got %v", class, args["terminalId"])
		}
		// The command is an empty template — validated non-empty only at dispatch.
		if args["command"] != "" {
			t.Errorf("%s: reply args command should be an empty template, got %v", class, args["command"])
		}
	}
}

func TestRecommendedActionsFor_CompletedUnverifiedOnlyFocus(t *testing.T) {
	actions := recommendedActionsFor(domain.ClassCompletedUnverified, "t1")
	if len(actions) != 1 || actions[0].ToolName != "terminal.focus" {
		t.Fatalf("completed_unverified should offer a single terminal.focus, got %+v", actions)
	}
}

func TestRecommendedActionsFor_UnknownClassNil(t *testing.T) {
	if got := recommendedActionsFor(domain.ClassNoChange, "t1"); got != nil {
		t.Errorf("unhandled class should yield nil actions, got %+v", got)
	}
}
