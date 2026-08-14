package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// AUTO_APPROVE changes what the assistant may do to the user's machine WITHOUT asking,
// and it is invisible from every other surface: it comes from an env var, possibly set in
// another terminal days ago. So a session running unattended must SAY so — ONCE, in the
// masthead.
//
// It used to be stated twice, in the masthead and again in the live status line, on the
// theory that a scrolled-away masthead leaves no warning. The flag is read at process
// start and cannot change mid-session, so the masthead's statement stays permanently
// accurate — and a warning repeated on every frame beside the composer stops being read,
// which costs more than the scrollback risk it was insuring against.
func TestStatusLineDoesNotRepeatAutoApprove(t *testing.T) {
	th := darkTheme()
	// The status line stays silent when it has nothing to say; AUTO-APPROVE is not one
	// of the things it has to say.
	if got := stripAnsi(renderStatusLine(th, statusParams{AutoApprove: true}, 80)); got != "" {
		t.Errorf("the status line repeats the masthead's AUTO-APPROVE warning: %q", got)
	}
	// A busy line carries its real content and still no badge.
	busy := stripAnsi(renderStatusLine(th, statusParams{
		AutoApprove: true, ActiveLabel: "WORKING", ActiveID: "term_8", AttentionN: 3,
	}, 80))
	if strings.Contains(busy, "AUTO") {
		t.Errorf("the badge crept back into a busy status line: %q", busy)
	}
	if !strings.Contains(busy, "WORKING") {
		t.Errorf("the status line lost its real content: %q", busy)
	}
}

func TestMastheadRecordsAutoApprove(t *testing.T) {
	th := darkTheme()
	off := stripAnsi(renderMasthead(th, mastheadParams{Version: "1.0", Tier: domain.TierSystem}, 80))
	if strings.Contains(off, "AUTO-APPROVE") {
		t.Errorf("the masthead should be quiet when AUTO_APPROVE is off: %q", off)
	}
	on := stripAnsi(renderMasthead(th, mastheadParams{Version: "1.0", Tier: domain.TierSystem, AutoApprove: true}, 80))
	if !strings.Contains(on, "AUTO-APPROVE") {
		t.Errorf("the masthead must record AUTO_APPROVE: %q", on)
	}
	if !strings.Contains(on, "will not ask first") {
		t.Errorf("the masthead should say what it MEANS, not just name the flag: %q", on)
	}
	// Beside the tier, because it is a statement about the tier: the gate is unchanged,
	// but nothing inside it will ask first.
	tierIdx, badgeIdx := strings.Index(on, string(domain.TierSystem)), strings.Index(on, "AUTO-APPROVE")
	if tierIdx < 0 || badgeIdx < tierIdx {
		t.Errorf("the badge should follow the tier: %q", on)
	}
	// On its OWN row, left-anchored. Appended to the tier line it sat at the right-hand
	// end, where truncation eats it first — the tier survived a narrow terminal and the
	// safety warning did not, which is exactly backwards.
	var badgeRow string
	for _, row := range strings.Split(on, "\n") {
		if strings.Contains(row, "AUTO-APPROVE") {
			badgeRow = row
		}
	}
	if badgeRow == "" {
		t.Fatalf("no row carries the badge: %q", on)
	}
	if strings.Contains(badgeRow, string(domain.TierSystem)) {
		t.Errorf("the badge shares the tier row, so truncation would eat it first: %q", badgeRow)
	}
}

// A narrow terminal must not be how the warning disappears. Below the full label's width
// it abbreviates rather than truncating — a half-rendered safety warning reads as a
// glitch, which is arguably worse than none.
func TestMastheadBadgeSurvivesNarrowWidths(t *testing.T) {
	th := darkTheme()
	for _, w := range []int{80, 60, 40, 30, 20} {
		out := stripAnsi(renderMasthead(th, mastheadParams{Tier: domain.TierSystem, AutoApprove: true}, w))
		if !strings.Contains(out, "AUTO-APPROVE") {
			t.Errorf("width %d dropped the masthead badge: %q", w, out)
		}
	}
}
