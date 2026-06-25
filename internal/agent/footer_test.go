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
// The swap is unsynchronized, so tests that call this MUST NOT use t.Parallel() —
// footerSections is a process-global with no mutex (it is write-once at init in
// production and never mutated at runtime).
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

// Truncation keeps the PREFIX (first N runes), not an arbitrary slice or the suffix.
func TestComposeTurnFooter_TruncationKeepsPrefix(t *testing.T) {
	// 498 'a' + "XY" = exactly the first 500 runes; the trailing 'b's must be dropped.
	goal := strings.Repeat("a", goalAnchorMaxRunes-2) + "XY" + strings.Repeat("b", 10)
	body := footerBody(t, goal)
	want := strings.Repeat("a", goalAnchorMaxRunes-2) + "XY"
	if !strings.Contains(body, want) {
		t.Errorf("body should contain the first %d runes ending in XY", goalAnchorMaxRunes)
	}
	if strings.Contains(body, "XYb") {
		t.Error("truncation kept runes past the cap; it must keep the PREFIX, not the suffix")
	}
}

// Session-level: two sequential turns each anchor their OWN originating ask — the
// footer is never stale from a prior turn.
func TestComposeTurnFooter_DistinctGoalsAcrossSends(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "a"}, {Content: "b"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "goal-one", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "goal-two", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	last0 := r.seen[0][len(r.seen[0])-1]
	last1 := r.seen[1][len(r.seen[1])-1]
	if !strings.Contains(last0.StringContent, "goal-one") {
		t.Errorf("send 1 footer should carry goal-one; got %q", last0.StringContent)
	}
	if !strings.Contains(last1.StringContent, "goal-two") || strings.Contains(last1.StringContent, "goal-one") {
		t.Errorf("send 2 footer should carry goal-two only; got %q", last1.StringContent)
	}
}

// Session-level: a mid-turn redirect folds into history as a user message, but the
// footer stays anchored to the ORIGINAL goal (it never chases the injection).
func TestComposeTurnFooter_StableAcrossMidTurnInjection(t *testing.T) {
	var s *Session
	r := &injectRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}}, // round 0: tool call → loop
			{Content: "final"}, // round 1: final answer
		},
		onRound: func(round int) {
			if round == 0 {
				s.InjectPrompt("stop, explain only")
			}
		},
	}
	s = NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "original goal", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) < 2 {
		t.Fatalf("want >= 2 rounds, got %d", len(r.seen))
	}
	round1 := r.seen[1]
	if !userTextSeen(round1, "stop, explain only") {
		t.Error("round 1 should see the folded-in injection in history")
	}
	last := round1[len(round1)-1]
	if last.Role != "system" || !strings.Contains(last.StringContent, "original goal") {
		t.Errorf("footer should still carry the original goal; got %+v", last)
	}
	if strings.Contains(last.StringContent, "stop, explain only") {
		t.Error("footer must not adopt the mid-turn injection as the goal")
	}
}

// Session-level: a blank send appends NO tail system message — the request is
// byte-identical to the pre-footer behaviour.
func TestComposeTurnFooter_BlankSendAppendsNoFooter(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{{Content: "final"}}}
	s := NewSession(baseDeps(r, &fakeTools{result: domain.Ok("ok", nil)}))

	if _, err := s.Send(context.Background(), "   ", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(r.seen) == 0 {
		t.Fatal("router observed no rounds")
	}
	last := r.seen[0][len(r.seen[0])-1]
	if last.Role == "system" && strings.Contains(last.StringContent, "# Current goal") {
		t.Errorf("blank goal must not append a goal footer; got %+v", last)
	}
}
