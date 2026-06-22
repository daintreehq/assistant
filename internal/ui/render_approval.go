package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_approval.go renders the approval sheet (ui-input.md §4): a full-width
// bordered card directly ABOVE the composer, leading with the consequence, visually
// defaulting to DECLINE, understandable with color stripped.

// titleFor returns the risk-specific question (§4).
func titleFor(req tools.ConfirmRequest) string {
	switch req.Risk {
	case domain.RiskGit:
		// Distinguish the common git verbs by tool name when we can.
		if strings.Contains(req.ToolName, "push") {
			return "Push branch to origin?"
		}
		if strings.Contains(req.ToolName, "commit") {
			return "Commit changes?"
		}
		return "Run a git action?"
	case domain.RiskTerminal:
		return "Send input to terminal?"
	case domain.RiskProject:
		return "Create worktree?"
	case domain.RiskExternal:
		return "Run an external action?"
	case domain.RiskSystem:
		return "Run a system-level action?"
	default:
		return "Approve this action?"
	}
}

// consequenceFor returns the consequence to lead with: the tool's own phrasing OR
// the per-risk fallback (use ||-style so a blank also falls back, §4).
func consequenceFor(req tools.ConfirmRequest) string {
	if strings.TrimSpace(req.Consequence) != "" {
		return req.Consequence
	}
	return riskConsequence(req.Risk)
}

// riskConsequence is the per-risk fallback consequence (all 8 risk classes, §4).
func riskConsequence(r domain.RiskClass) string {
	switch r {
	case domain.RiskTerminal:
		return "sends input to a live terminal session"
	case domain.RiskProject:
		return "modifies the project workspace"
	case domain.RiskGit:
		return "runs a git operation that can change history or remotes"
	case domain.RiskExternal:
		return "performs an action outside this machine"
	case domain.RiskSystem:
		return "runs a system-level action with broad effect"
	case domain.RiskRead:
		return "reads local data"
	case domain.RiskLocal:
		return "makes a local, reversible change"
	case domain.RiskUI:
		return "updates the local interface"
	default:
		return "performs an action that needs your approval"
	}
}

// renderApproval renders the sheet. showArgs reveals reason+args (V toggles).
func renderApproval(th theme.Theme, req tools.ConfirmRequest, showArgs bool, width int) string {
	var b strings.Builder
	g := th.Glyphs

	// Top border + title.
	b.WriteString(th.Blocked().Render(g.Approval + " " + truncateCells(titleFor(req), width-2)))
	b.WriteByte('\n')
	// Affects (the consequence to lead with).
	b.WriteString(th.Body().Render(truncateCells("affects  "+consequenceFor(req), width)))
	b.WriteByte('\n')
	// Tool (dim secondary).
	b.WriteString(th.Dim().Render(truncateCells("tool     "+req.ToolName, width)))

	if showArgs {
		if req.Summary != "" {
			b.WriteByte('\n')
			b.WriteString(th.Dim().Render(truncateCells("reason   "+req.Summary, width)))
		}
		if len(req.Args) > 0 {
			b.WriteByte('\n')
			b.WriteString(th.Muted().Render(truncateCells("args     "+compactArgs(string(req.Args), width-9), width)))
		}
	}

	// Action row — DECLINE is the default (rendered inverse/highlighted).
	b.WriteByte('\n')
	approve := th.Body().Render("Y approve")
	decline := th.Body().Reverse(true).Render(" N decline ")
	inspect := th.Dim().Render("V inspect")
	esc := th.Dim().Render("Esc")
	b.WriteString(approve + "  " + decline + "  " + inspect + "  " + esc)

	return b.String()
}

// compactArgs collapses a JSON args blob to one truncated line for the inspect
// panel (whitespace squashed; truncated to max cells).
func compactArgs(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	return truncateCells(s, max)
}
