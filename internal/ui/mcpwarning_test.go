package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// mcpwarning_test.go pins the degraded-MCP warning as ACTIONABLE and CAUSE-SPECIFIC. The
// old detail ("Check the MCP server, then restart the assistant") named none of the three
// real situations: a dropped link /reconnect retries, a session Daintree closed or
// replaced whose token only a panel reopen can refresh, and no configured endpoint at all,
// where /reconnect has nothing to retry.

// warnWidths spans the golden set plus 84 — the band where the old one-row detail fit but
// the new one wraps, which is exactly where a wrapping regression would first show.
var warnWidths = []int{40, 55, 72, 84, 100, 120}

func degradedModel(cols int, unconfigured bool) Model {
	m := testModel(cols)
	m.degraded = true
	m.mcpUnconfigured = unconfigured
	return m
}

func TestMCPDegradedView_ProvidesActionableRecovery(t *testing.T) {
	for _, w := range warnWidths {
		m := degradedModel(w, false)
		out := ansi.Strip(m.mcpDegradedView(m.contentW()))

		if !strings.Contains(out, "Daintree MCP unavailable") {
			t.Errorf("@%d: title missing:\n%s", w, out)
		}
		// The command is one token, so wrapping can never split it.
		if !strings.Contains(out, "/reconnect") {
			t.Errorf("@%d: the warning must name the in-cockpit recovery command:\n%s", w, out)
		}
		// Wrapping may break the phrase across rows, so match on the unwrapped text.
		flat := strings.Join(strings.Fields(out), " ")
		if !strings.Contains(flat, "reopen the Assistant panel") {
			t.Errorf("@%d: the warning must say when reopening the panel is required:\n%s", w, out)
		}
		if strings.Contains(flat, "restart the assistant") {
			t.Errorf("@%d: restarting is not the recommended recovery:\n%s", w, out)
		}
		assertRowsFit(t, "recovery", w, out, m.contentW())
	}
}

// With no endpoint configured (offline mode, or a launch from outside Daintree) there is
// nothing for /reconnect to retry — recommending it would send the user in a circle.
func TestMCPDegradedView_UnconfiguredDoesNotOfferReconnect(t *testing.T) {
	for _, w := range warnWidths {
		m := degradedModel(w, true)
		out := ansi.Strip(m.mcpDegradedView(m.contentW()))

		if strings.Contains(out, "/reconnect") {
			t.Errorf("@%d: an unconfigured link must not advertise /reconnect:\n%s", w, out)
		}
		if !strings.Contains(out, "not configured") {
			t.Errorf("@%d: the title must say the link is unconfigured, not merely down:\n%s", w, out)
		}
		flat := strings.Join(strings.Fields(out), " ")
		if !strings.Contains(flat, "Launch the assistant from Daintree") {
			t.Errorf("@%d: the only real recovery must be named:\n%s", w, out)
		}
		assertRowsFit(t, "unconfigured", w, out, m.contentW())
	}
}

// Both branches sit in the fixed bottom band, whose height budget counts explicit rows.
func assertRowsFit(t *testing.T, label string, cols int, out string, max int) {
	t.Helper()
	if strings.TrimSpace(out) == "" {
		t.Fatalf("%s@%d: warning rendered nothing", label, cols)
	}
	for i, line := range strings.Split(out, "\n") {
		if got := ansi.StringWidth(line); got > max {
			t.Errorf("%s@%d: row %d is %d cells (max %d): %q", label, cols, i, got, max, line)
		}
	}
}

// A healthy link stays completely silent — the cockpit is exception-first.
func TestMCPDegradedView_SilentWhenHealthy(t *testing.T) {
	m := testModel(100)
	if out := m.mcpDegradedView(m.contentW()); out != "" {
		t.Errorf("a healthy MCP link must render nothing, got %q", out)
	}
}

// The warning must reach the composed frame, and the frame must stay inside its width and
// height budget with the extra wrapped row the longer copy costs at narrow panes.
func TestMCPDegradedView_ReachesTheFrameWithinBudget(t *testing.T) {
	for _, w := range warnWidths {
		m := degradedModel(w, false)
		v := m.View()
		frame := ansi.Strip(v.Content)
		if !strings.Contains(frame, "/reconnect") {
			t.Errorf("@%d: the degraded warning never reached the rendered frame:\n%s", w, frame)
		}
		assertNoOverflow(t, "degraded-warning@"+itoa(w), v.Content, m.usableWidth())
		assertNoHeightOverflow(t, "degraded-warning@"+itoa(w), v.Content, m.rows)
	}
}

// The warning's primary instruction is "Run /reconnect", so a SUCCESSFUL /reconnect has
// to clear it. The command handlers return prose, not messages, so nothing used to clear
// m.degraded: the cockpit printed "Reconnected (…)" with the red warning still telling the
// user to run /reconnect — a loop the new copy would send people around repeatedly.
func TestCommandComplete_ResyncsTheDegradedFlag(t *testing.T) {
	cases := []struct {
		name         string
		resolved     bool
		linkKnown    bool
		connected    bool
		startDegrade bool
		wantDegraded bool
	}{
		{name: "reconnect succeeded", resolved: true, linkKnown: true, connected: true,
			startDegrade: true, wantDegraded: false},
		{name: "reconnect still failing", resolved: true, linkKnown: true, connected: false,
			startDegrade: true, wantDegraded: true},
		{name: "link dropped under a healthy cockpit", resolved: true, linkKnown: true, connected: false,
			startDegrade: false, wantDegraded: true},
		// Before the boot connect settles the async connect owns the flag; reading it here
		// would race that.
		{name: "boot connect still in flight", resolved: false, linkKnown: true, connected: true,
			startDegrade: true, wantDegraded: true},
		// Nothing to ask (headless harness): leave the flag exactly as it was.
		{name: "no link to query", resolved: true, linkKnown: false, connected: false,
			startDegrade: true, wantDegraded: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := harnessModel()
			m.mcpResolved = tc.resolved
			m.degraded = tc.startDegrade
			m.controller.mcpLink = func() (bool, bool) { return tc.connected, tc.linkKnown }

			mm := asModel(t, mustModel(m.onCommandComplete(CommandCompleteMsg{
				Tracked: true, Title: "Reconnect", Text: "Reconnected (streamable-http, 42 tools).",
			})))

			if mm.degraded != tc.wantDegraded {
				t.Fatalf("degraded = %v, want %v", mm.degraded, tc.wantDegraded)
			}
			warned := strings.Contains(ansi.Strip(mm.View().Content), "Daintree MCP")
			if warned != tc.wantDegraded {
				t.Errorf("warning on screen = %v, want %v", warned, tc.wantDegraded)
			}
		})
	}
}

// The degraded flag must survive a shrinking terminal without the band outgrowing it: at
// the height floor the footer collapses to the one-line "terminal too small" fallback
// rather than scrolling a frozen partial copy into scrollback.
func TestMCPDegradedView_ShortTerminalStaysInBudget(t *testing.T) {
	for _, rows := range []int{4, 6, 8, 12} {
		m := degradedModel(40, false)
		m.rows = rows
		v := m.View()
		assertNoHeightOverflow(t, "degraded-short-"+itoa(rows), v.Content, rows)
	}
}
