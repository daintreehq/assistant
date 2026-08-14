package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// AUTO_APPROVE changes what the assistant may do to the user's machine WITHOUT asking,
// and it is invisible from every other surface: it comes from an env var, possibly set in
// another terminal days ago. A session running unattended must SAY so, on screen, at all
// times — not once at boot, and not only in a log.
func TestStatusLineAlwaysShowsAutoApprove(t *testing.T) {
	th := darkTheme()

	// The status line otherwise stays silent when it has nothing to say. AUTO-APPROVE
	// always has something to say, so it must speak even in an otherwise-idle session.
	quiet := stripAnsi(renderStatusLine(th, statusParams{}, 80))
	if quiet != "" {
		t.Fatalf("test premise broken: an empty status line should render nothing, got %q", quiet)
	}
	on := stripAnsi(renderStatusLine(th, statusParams{AutoApprove: true}, 80))
	if !strings.Contains(on, "AUTO-APPROVE") {
		t.Errorf("an idle session with AUTO_APPROVE on must still show the badge, got %q", on)
	}
}

// It must survive a busy status line, not be pushed out by an agent badge and an
// attention count — those are exactly the moments a bypassed confirmation matters most.
func TestStatusLineKeepsAutoApproveWhenBusy(t *testing.T) {
	th := darkTheme()
	busy := stripAnsi(renderStatusLine(th, statusParams{
		AutoApprove:    true,
		ActiveTone:     "active",
		ActiveLabel:    "WORKING",
		ActiveID:       "term_8",
		ActiveGoal:     "repair watcher tests",
		ActiveDuration: "18s",
		AttentionN:     3,
		TopSeverity:    domain.SeverityUrgent,
	}, 80))
	if !strings.Contains(busy, "AUTO-APPROVE") {
		t.Errorf("the badge was crowded out of a busy status line: %q", busy)
	}
	// And it comes FIRST, so a truncated line keeps it.
	if i := strings.Index(busy, "AUTO-APPROVE"); i > strings.Index(busy, "WORKING") {
		t.Errorf("AUTO-APPROVE must lead the status line: %q", busy)
	}
}

// A narrow terminal must not be a way to lose the warning.
func TestStatusLineKeepsAutoApproveWhenNarrow(t *testing.T) {
	th := darkTheme()
	for _, w := range []int{20, 30, 40} {
		out := stripAnsi(renderStatusLine(th, statusParams{AutoApprove: true, AttentionN: 2}, w))
		if !strings.Contains(out, "AUTO-APPROVE") {
			t.Errorf("width %d dropped the badge: %q", w, out)
		}
	}
	// Narrower than the full label. It abbreviates rather than truncating into a
	// half-rendered warning that reads as a glitch — but something always survives.
	for _, w := range []int{4, 8, 12, 14} {
		out := stripAnsi(renderStatusLine(th, statusParams{AutoApprove: true}, w))
		if strings.TrimSpace(out) == "" {
			t.Errorf("width %d rendered nothing at all — the warning vanished", w)
		}
		if strings.Contains(out, "…") {
			t.Errorf("width %d produced a truncated warning rather than an abbreviation: %q", w, out)
		}
	}
}

// Off is the default and must be silent — a permanent badge for the normal case would
// train people to ignore the row it lives in.
func TestStatusLineSaysNothingWhenAutoApproveIsOff(t *testing.T) {
	if out := stripAnsi(renderStatusLine(darkTheme(), statusParams{AutoApprove: false, Cost: 0}, 80)); out != "" {
		t.Errorf("AUTO_APPROVE off should add nothing, got %q", out)
	}
}

// The masthead is the permanent record: a pasted transcript has to show that the session
// was running without confirmations, beside the tier that bounded it.
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
