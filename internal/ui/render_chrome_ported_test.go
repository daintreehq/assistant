package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/commands"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_chrome_ported_test.go exercises the rendered Go masthead/status strings
// (the Bubble Tea cockpit renders via a
// View string, so we assert on the stripped text + the width/field invariants).

func darkTheme() theme.Theme {
	t := theme.Resolve()
	t.Mode = theme.ModeDark
	t.Color = theme.PaletteFor(theme.ModeDark)
	return t
}

// --- Header / masthead ---

func TestMasthead_WordmarkVersionAndProject(t *testing.T) {
	th := darkTheme()
	out := stripAnsi(renderMasthead(th, mastheadParams{Version: "0.1.0", ProjectName: "assistant", Tier: domain.TierSystem}, 60))
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "Daintree Assistant") || !strings.Contains(lines[0], "v0.1.0") {
		t.Errorf("identity line missing wordmark+version: %q", lines[0])
	}
	// Project sits directly beneath the wordmark.
	if strings.TrimSpace(lines[1]) != "assistant" {
		t.Errorf("project line = %q, want assistant beneath wordmark", lines[1])
	}
}

func TestMasthead_NoMcpBadge(t *testing.T) {
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{Version: "0.1.0", Tier: domain.TierSystem}, 60))
	// The live MCP link is by-exception status that lives in the StatusLine.
	if strings.Contains(out, "CONNECTED") || strings.Contains(out, "DEGRADED") {
		t.Errorf("masthead must not carry an MCP badge: %q", out)
	}
}

func TestMasthead_TierGloss(t *testing.T) {
	cases := []struct {
		tier domain.Tier
		want string
	}{
		{domain.TierSystem, "full access"},
		{domain.TierOperator, "terminals"},
		{domain.TierSupervisor, "read & UI only"},
	}
	for _, c := range cases {
		out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{Version: "0.1.0", Tier: c.tier}, 60))
		if !strings.Contains(out, "tier "+string(c.tier)) {
			t.Errorf("tier %q: missing labelled tier token: %q", c.tier, out)
		}
		if !strings.Contains(out, c.want) {
			t.Errorf("tier %q: gloss %q missing: %q", c.tier, c.want, out)
		}
	}
}

func TestMasthead_RuleBelowTierLoggingBelowRule(t *testing.T) {
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", Tier: domain.TierSystem, Logging: true, LogFile: "/t.log",
	}, 60))
	lines := strings.Split(out, "\n")
	ruleIdx, tierIdx, logIdx := -1, -1, -1
	for i, l := range lines {
		if tierIdx < 0 && strings.Contains(l, "tier system") {
			tierIdx = i
		}
		if ruleIdx < 0 && strings.Count(l, "─") >= 4 {
			ruleIdx = i
		}
		if logIdx < 0 && strings.Contains(l, "logging") {
			logIdx = i
		}
	}
	if ruleIdx < 0 || tierIdx < 0 || logIdx < 0 {
		t.Fatalf("missing rule/tier/logging rows: rule=%d tier=%d log=%d\n%q", ruleIdx, tierIdx, logIdx, out)
	}
	if !(ruleIdx > tierIdx && logIdx > ruleIdx) {
		t.Errorf("order wrong: tier=%d rule=%d log=%d (want tier<rule<log)", tierIdx, ruleIdx, logIdx)
	}
	if !strings.Contains(out, "/t.log") {
		t.Errorf("logging line should surface the log path: %q", out)
	}
	// The badge leads with the hollow dotted glyph ("◌ logging").
	if !strings.Contains(out, "◌ logging") {
		t.Errorf("logging badge should lead with the hollow dotted glyph: %q", out)
	}
}

func TestCollapseHome(t *testing.T) {
	t.Setenv("HOME", "/home/bob")
	cases := map[string]string{
		"/home/bob/.daintree/logs/x.log": "~/.daintree/logs/x.log",
		"/home/bob":                      "~",
		"/home/bobby/x":                  "/home/bobby/x", // sibling, NOT collapsed
		"/var/log/x":                     "/var/log/x",
	}
	for in, want := range cases {
		if got := collapseHome(in); got != want {
			t.Errorf("collapseHome(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMasthead_LogPathWrapsNotTruncated: the log path must ~-collapse and wrap across
// rows (no ellipsis), keeping the whole filename visible while every row stays within
// the terminal width.
func TestMasthead_LogPathWrapsNotTruncated(t *testing.T) {
	t.Setenv("HOME", "/home/bob")
	const cols = 30
	path := "/home/bob/.daintree/logs/2026-06-22-ses_1a8b2c3d4e5f.log"
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", Tier: domain.TierSystem, Logging: true, LogFile: path,
	}, cols))
	// Scope the no-ellipsis check to the badge region (from "◌ logging" on) — the
	// unrelated tier-gloss line legitimately truncates at this narrow width.
	idx := strings.Index(out, "◌ logging")
	if idx < 0 {
		t.Fatalf("no logging badge in masthead: %q", out)
	}
	badge := out[idx:]
	if strings.Contains(badge, "…") {
		t.Errorf("log path must wrap, not truncate with an ellipsis: %q", badge)
	}
	if strings.Contains(out, "/home/bob") {
		t.Errorf("log path should be ~-collapsed: %q", out)
	}
	// Rejoining the wrapped rows must yield the whole collapsed path contiguously
	// (continuation rows carry only the path, no label, so the join is clean).
	joined := strings.ReplaceAll(out, "\n", "")
	if !strings.Contains(joined, "~/.daintree/logs/2026-06-22-ses_1a8b2c3d4e5f.log") {
		t.Errorf("full collapsed path not present across wrapped rows: %q", out)
	}
	for i, l := range strings.Split(out, "\n") {
		if w := cellWidth(l); w > cols {
			t.Errorf("row %d width %d exceeds cols %d: %q", i, w, cols, l)
		}
	}
}

// logRow returns the single masthead row carrying the "logging" badge.
func logRow(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "logging") {
			return l
		}
	}
	t.Fatalf("no logging row in masthead: %q", out)
	return ""
}

// TestMasthead_LogPathFitsOneRow: at a comfortable width the ~-collapsed path sits on
// the single badge row, untruncated — wrapping is the narrow-terminal fallback, not the
// common case.
func TestMasthead_LogPathFitsOneRow(t *testing.T) {
	t.Setenv("HOME", "/home/bob")
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", Tier: domain.TierSystem, Logging: true,
		LogFile: "/home/bob/.daintree/logs/2026-06-22-ses_1a8b.log",
	}, 80))
	row := logRow(t, out)
	if !strings.Contains(row, "◌ logging · ~/.daintree/logs/2026-06-22-ses_1a8b.log") {
		t.Errorf("path should sit ~-collapsed on one row: %q", row)
	}
	if strings.Contains(row, "…") {
		t.Errorf("a wide terminal must not truncate the path: %q", row)
	}
}

// TestMasthead_LogBadgeEmptyPathNoDanglingSeparator: with logging on but no path yet,
// the badge is just the label — never "◌ logging ·" with a dangling separator.
func TestMasthead_LogBadgeEmptyPathNoDanglingSeparator(t *testing.T) {
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", Tier: domain.TierSystem, Logging: true, LogFile: "",
	}, 60))
	row := logRow(t, out)
	if strings.TrimSpace(row) != "◌ logging" {
		t.Errorf("empty-path badge should be just the label, got %q", row)
	}
}

// TestMasthead_LogPathWideRuneNoOverflow: a wide (2-cell) leading grapheme whose width
// exceeds the one-cell first-row budget must wrap to a full-width row, never spill past
// the terminal width. Exercises the wrap loop's wide-grapheme guard.
func TestMasthead_LogPathWideRuneNoOverflow(t *testing.T) {
	const cols = 13 // headW(12) + 1 → the first-row path budget is a single cell
	out := stripAnsi(renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", Tier: domain.TierSystem, Logging: true, LogFile: "中文/trace.log",
	}, cols))
	for i, l := range strings.Split(out, "\n") {
		if w := cellWidth(l); w > cols {
			t.Errorf("row %d width %d exceeds cols %d: %q", i, w, cols, l)
		}
	}
}

func TestMasthead_NarrowNoRowOverflow(t *testing.T) {
	const cols = 22
	out := renderMasthead(darkTheme(), mastheadParams{
		Version: "0.1.0", ProjectName: "a-very-long-project-name-that-overflows",
		Tier: domain.TierSystem, Logging: true, LogFile: "/Users/x/.daintree/logs/2026-06-20-ses_02f0965b.log",
	}, cols)
	for i, line := range strings.Split(out, "\n") {
		if w := cellWidth(line); w > cols {
			t.Errorf("row %d width %d exceeds cols %d: %q", i, w, cols, stripAnsi(line))
		}
	}
}

func TestMasthead_DestructiveEscalatesTier(t *testing.T) {
	th := darkTheme()
	quiet := renderMasthead(th, mastheadParams{Version: "0.1.0", Tier: domain.TierSystem}, 60)
	danger := renderMasthead(th, mastheadParams{Version: "0.1.0", Tier: domain.TierSystem, Destructive: true}, 60)
	// At rest the tier is dim; a destructive pending action recolors it — the raw ANSI
	// must differ (the word stays, only the style escalates).
	if quiet == danger {
		t.Error("destructive pending must change the tier styling (escalate to danger)")
	}
	if !strings.Contains(stripAnsi(danger), "tier system") {
		t.Errorf("tier word must remain on escalation: %q", stripAnsi(danger))
	}
}

// --- StatusLine ---

func TestStatusLine_IdleIsEmpty(t *testing.T) {
	// Silence means idle: no "Standing by", no MCP/tier token.
	if got := renderStatusLine(darkTheme(), statusParams{}, 80); strings.TrimSpace(stripAnsi(got)) != "" {
		t.Errorf("idle status line must be empty, got %q", got)
	}
}

// workingBadge is the structured active-agent badge equivalent of the original's
// still_working watcher (tone "active", UPPERCASE "WORKING", supervised id "term_8").
func workingBadge() statusParams {
	return statusParams{ActiveTone: "active", ActiveLabel: "WORKING", ActiveID: "term_8"}
}

func TestStatusLine_NoMcpNoTierToken(t *testing.T) {
	p := workingBadge()
	out := stripAnsi(renderStatusLine(darkTheme(), p, 80))
	if strings.Contains(out, "MCP") {
		t.Errorf("status must not show an MCP token while healthy: %q", out)
	}
	if strings.Contains(out, "SYSTEM") || strings.Contains(strings.ToLower(out), " sys ") {
		t.Errorf("status must not carry a tier token (lives in masthead): %q", out)
	}
}

func TestStatusLine_PrefersActiveAgentNoOrphanSep(t *testing.T) {
	p := workingBadge()
	out := stripAnsi(renderStatusLine(darkTheme(), p, 80))
	if !strings.Contains(out, "WORKING") || !strings.Contains(out, "term_8") {
		t.Errorf("active agent badge + terminal missing: %q", out)
	}
	// The badge is built from tone+label: a leading tone glyph (◌ for active) precedes
	// the UPPERCASE label (the StateBadge is inlined) — NOT the
	// old flat "WORKING term_8" string.
	if !strings.Contains(out, "◌ WORKING") {
		t.Errorf("active badge must lead with the tone glyph: %q", out)
	}
	if strings.Contains(out, "·  ·") {
		t.Errorf("orphan separator between segments: %q", out)
	}
}

func TestStatusLine_IdleSurfacesCostAndModel(t *testing.T) {
	out := stripAnsi(renderStatusLine(darkTheme(), statusParams{
		Cost: 0.012, Model: "glm-5p2",
	}, 80))
	if !strings.Contains(out, "$0.012") {
		t.Errorf("cost missing on idle line: %q", out)
	}
	if !strings.Contains(out, "glm-5p2") {
		t.Errorf("model missing on wide idle line: %q", out)
	}
}

func TestStatusLine_CostHiddenWhenUnknown(t *testing.T) {
	out := stripAnsi(renderStatusLine(darkTheme(), statusParams{Cost: 0, Model: "glm-5p2"}, 80))
	if strings.Contains(out, "$") {
		t.Errorf("cost must be hidden when unknown ($0): %q", out)
	}
	if !strings.Contains(out, "glm-5p2") {
		t.Errorf("model must still show on the idle line: %q", out)
	}
}

func TestStatusLine_AttentionChip(t *testing.T) {
	th := darkTheme()
	out := stripAnsi(renderStatusLine(th, statusParams{AttentionN: 2, TopSeverity: domain.SeverityUrgent}, 80))
	want := th.Glyphs.Attention + "2" // "»2" unicode / "!2" ASCII
	if !strings.Contains(out, want) {
		t.Errorf("attention chip %q missing: %q", want, out)
	}
}

func TestStatusLine_Degraded(t *testing.T) {
	out := stripAnsi(renderStatusLine(darkTheme(), statusParams{Degraded: true}, 80))
	if !strings.Contains(out, "Daintree MCP unavailable") {
		t.Errorf("degraded MCP status missing: %q", out)
	}
}

func TestStatusLine_ModelRateLimited(t *testing.T) {
	out := stripAnsi(renderStatusLine(darkTheme(), statusParams{ModelRateLimited: true}, 80))
	if !strings.Contains(out, "Model rate-limited") {
		t.Errorf("model-rate-limited badge missing: %q", out)
	}
	// Cleared state stays silent (surfaced by exception only).
	clear := stripAnsi(renderStatusLine(darkTheme(), statusParams{}, 80))
	if strings.Contains(clear, "Model rate-limited") {
		t.Errorf("badge must not render when not rate-limited: %q", clear)
	}
}

func TestStatusLine_HealthyMcpIsSilent(t *testing.T) {
	// A healthy link is announced ONCE as a top status note (update.go), never a permanent
	// footer segment — so the status line says nothing about MCP while connected.
	out := stripAnsi(renderStatusLine(darkTheme(), statusParams{}, 80))
	if strings.Contains(out, "Connected to Daintree MCP") || strings.Contains(out, "MCP") {
		t.Errorf("healthy status line must not carry an MCP segment: %q", out)
	}
}

// --- HelpOverlay ---

func TestHelpView_RendersEveryRegistryCommandSyntax(t *testing.T) {
	// The help view is built from the command registry, so it can't drift from the
	// handlers. Every registered command's "/name" must render (issue #50 dropped
	// /models + /help — guard them explicitly).
	body := stripAnsi(renderCommandCellText(darkTheme(), "Help", commands.HelpTextUI(), 120))
	for _, e := range commands.PaletteEntries() {
		if !strings.Contains(body, e[0]) {
			t.Errorf("help view missing command syntax %q", e[0])
		}
	}
	if !strings.Contains(body, "/models") || !strings.Contains(body, "/help") {
		t.Errorf("help view must surface /models and /help (issue #50)")
	}
}
