package agent

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// withFooterSections swaps the package-local registry for the duration of one test
// and restores it afterwards, so registry-shape tests never bleed into each other.
func withFooterSections(t *testing.T, sections ...footerSection) {
	t.Helper()
	prev := footerSections
	t.Cleanup(func() { footerSections = prev })
	footerSections = sections
}

// footerBody asserts composeTurnFooter produced exactly one system message and
// returns its body — the common shape for the goal-anchor assertions.
func footerBody(t *testing.T, goal string) string {
	t.Helper()
	msgs := composeTurnFooter(goal)
	if len(msgs) != 1 {
		t.Fatalf("composeTurnFooter(%q) returned %d messages, want 1", goal, len(msgs))
	}
	if msgs[0].Role != "system" {
		t.Fatalf("footer role = %q, want system", msgs[0].Role)
	}
	return msgs[0].StringContent
}

// An empty or whitespace-only goal yields NO footer at all, so the append is a
// no-op and the request is byte-identical to the pre-footer behaviour.
func TestComposeTurnFooter_BlankGoalEmitsNothing(t *testing.T) {
	for _, goal := range []string{"", "   ", "\n\t  \n"} {
		if got := composeTurnFooter(goal); got != nil {
			t.Errorf("composeTurnFooter(%q) = %v, want nil", goal, got)
		}
	}
}

// A normal goal produces a single system message carrying the `# Current goal`
// header, the verbatim ask, and an output-discipline line.
func TestComposeTurnFooter_GoalAnchorShape(t *testing.T) {
	body := footerBody(t, "fix the failing login test")
	if !strings.HasPrefix(body, "# Current goal\n") {
		t.Errorf("body does not start with the header; got %q", body)
	}
	if !strings.Contains(body, "fix the failing login test") {
		t.Errorf("body missing the goal text; got %q", body)
	}
	if !strings.Contains(body, "Stay focused on this goal") {
		t.Errorf("body missing the output-discipline line; got %q", body)
	}
}

// The goal is trimmed before it is embedded, so leading/trailing whitespace in the
// originating ask never bloats the anchor.
func TestComposeTurnFooter_TrimsGoal(t *testing.T) {
	body := footerBody(t, "   ship it   ")
	if !strings.Contains(body, "# Current goal\nship it\n") {
		t.Errorf("goal was not trimmed; got %q", body)
	}
}

// A goal at exactly the rune cap is preserved in full.
func TestComposeTurnFooter_AtCapNotTruncated(t *testing.T) {
	goal := strings.Repeat("a", goalAnchorMaxRunes)
	body := footerBody(t, goal)
	if !strings.Contains(body, goal) {
		t.Errorf("a goal of exactly %d runes was truncated", goalAnchorMaxRunes)
	}
}

// A goal past the rune cap is truncated to the cap — never beyond.
func TestComposeTurnFooter_TruncatesOverCap(t *testing.T) {
	goal := strings.Repeat("a", goalAnchorMaxRunes+1)
	body := footerBody(t, goal)
	if !strings.Contains(body, strings.Repeat("a", goalAnchorMaxRunes)) {
		t.Errorf("body should contain the first %d runes", goalAnchorMaxRunes)
	}
	if strings.Contains(body, strings.Repeat("a", goalAnchorMaxRunes+1)) {
		t.Errorf("body should NOT contain more than %d goal runes", goalAnchorMaxRunes)
	}
}

// Truncation is rune-safe: a multibyte ask past the cap is cut on a rune boundary,
// never mid-character, so the body stays valid UTF-8.
func TestComposeTurnFooter_TruncationIsRuneSafe(t *testing.T) {
	goal := strings.Repeat("世", goalAnchorMaxRunes+1)
	body := footerBody(t, goal)
	if !utf8.ValidString(body) {
		t.Error("truncated body is not valid UTF-8 (a rune was split)")
	}
	if !strings.Contains(body, strings.Repeat("世", goalAnchorMaxRunes)) {
		t.Errorf("body should contain the first %d runes", goalAnchorMaxRunes)
	}
	if strings.Contains(body, strings.Repeat("世", goalAnchorMaxRunes+1)) {
		t.Errorf("body should NOT contain more than %d goal runes", goalAnchorMaxRunes)
	}
}

// Multiple enabled sections coalesce into ONE system message, joined by a blank
// line, in registry order. This is the forward-compat path for later waves.
func TestComposeTurnFooter_JoinsMultipleSections(t *testing.T) {
	withFooterSections(t,
		func(string) (string, bool) { return "SECTION-ONE", true },
		func(string) (string, bool) { return "SECTION-TWO", true },
	)
	body := footerBody(t, "goal")
	if body != "SECTION-ONE\n\nSECTION-TWO" {
		t.Errorf("sections not joined by a blank line in order; got %q", body)
	}
}

// A disabled section (ok=false) and a blank-bodied section are both skipped, so a
// surviving section never carries a stray leading/trailing separator.
func TestComposeTurnFooter_SkipsDisabledAndBlankSections(t *testing.T) {
	withFooterSections(t,
		func(string) (string, bool) { return "DROP-ME", false }, // disabled
		func(string) (string, bool) { return "   ", true },      // blank body
		func(string) (string, bool) { return "KEEP-ME", true },
	)
	body := footerBody(t, "goal")
	if body != "KEEP-ME" {
		t.Errorf("disabled/blank sections not cleanly skipped; got %q", body)
	}
}

// All sections skipped → no message at all.
func TestComposeTurnFooter_AllSectionsSkippedEmitsNothing(t *testing.T) {
	withFooterSections(t, func(string) (string, bool) { return "x", false })
	if got := composeTurnFooter("goal"); got != nil {
		t.Errorf("composeTurnFooter = %v, want nil when every section is skipped", got)
	}
}

// Session-level: the footer is appended as the LAST message of the model request,
// carries the goal, and NEVER leaks into durable history (it must stay ephemeral).
func TestComposeTurnFooter_AppendedToStreamTail(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	before := len(s.Messages())
	if _, err := s.Send(context.Background(), "do the thing", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if len(r.seen) == 0 {
		t.Fatal("router observed no rounds")
	}
	round := r.seen[0]
	last := round[len(round)-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "# Current goal") {
		t.Fatalf("last message is not the goal footer: %+v", last)
	}
	if !strings.Contains(last.StringContent, "do the thing") {
		t.Errorf("footer missing the goal text; got %q", last.StringContent)
	}

	// Ephemeral: durable history grows only by user + assistant (+2), and no stored
	// message carries the goal anchor.
	if after := len(s.Messages()); after-before != 2 {
		t.Errorf("history grew by %d, want 2 (footer must not be persisted)", after-before)
	}
	for _, m := range s.Messages() {
		if m.Role == "system" && strings.Contains(m.StringContent, "# Current goal") {
			t.Fatal("goal footer leaked into durable history; it must stay ephemeral")
		}
	}
}

// Session-level: the footer is rebuilt on EVERY round of a multi-round turn, always
// seeded from the same originating ask (not a mid-turn injection).
func TestComposeTurnFooter_RebuiltEveryRound(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
		{Content: "final"}, // round 1: final answer
	}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "investigate the bug", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	for i, round := range r.seen[:2] {
		last := round[len(round)-1]
		if last.Role != "system" || !strings.Contains(last.StringContent, "investigate the bug") {
			t.Errorf("round %d does not end with the goal footer: %+v", i, last)
		}
	}
}
