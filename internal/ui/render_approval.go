package ui

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/ui/theme"
)

// render_approval.go renders the approval sheet: a full-width
// bordered card directly ABOVE the composer, leading with the consequence, visually
// defaulting to DECLINE, understandable with color stripped.

// titleFor returns the risk-specific question.
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
// the per-risk fallback (use ||-style so a blank also falls back).
func consequenceFor(req tools.ConfirmRequest) string {
	if strings.TrimSpace(req.Consequence) != "" {
		return req.Consequence
	}
	return riskConsequence(req.Risk)
}

// riskConsequence is the per-risk fallback consequence (all 8 risk classes).
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

// maxArgRows caps how many wrapped rows the inspect panel shows for the args blob.
const maxArgRows = 4

// renderApproval renders the sheet from the pending confirm state. showArgs reveals the
// tool description + full args (V toggles); requireType swaps the single-key action row
// for a typed-confirmation prompt on the highest-risk actions.
func renderApproval(th theme.Theme, p *pendingConfirm, width int) string {
	req := p.req
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

	if p.showArgs {
		if req.Summary != "" {
			b.WriteByte('\n')
			// 'about' — this is the tool's own description (what it does), NOT a per-call
			// rationale, so it must not be mislabeled "reason".
			b.WriteString(th.Dim().Render(truncateCells("about    "+req.Summary, width)))
		}
		if len(req.Args) > 0 {
			b.WriteByte('\n')
			b.WriteString(th.Dim().Render("args"))
			b.WriteByte('\n')
			// Show the FULL args wrapped across rows (capped) rather than one squashed,
			// truncated blob — the load-bearing detail (command text, push target, call
			// method) is exactly what gets cut off otherwise.
			b.WriteString(th.Muted().Render(argsBlock(string(req.Args), width, maxArgRows)))
		}
	}

	b.WriteByte('\n')
	if p.requireType {
		b.WriteString(renderTypedConfirm(th, p, width))
	} else {
		b.WriteString(renderActionRow(th, req))
	}
	return b.String()
}

// renderActionRow renders the standard single-key action row — DECLINE is the visual
// default (inverse). The A (approve & remember) affordance appears only for risk classes
// eligible for the session allow-list.
func renderActionRow(th theme.Theme, req tools.ConfirmRequest) string {
	approve := th.Body().Render("Y approve")
	decline := th.Body().Reverse(true).Render(" N decline ")
	inspect := th.Dim().Render("V inspect")
	esc := th.Dim().Render("Esc")
	if rememberable(req.Risk) {
		allow := th.Dim().Render("A allow tool")
		return approve + "  " + decline + "  " + allow + "  " + inspect + "  " + esc
	}
	return approve + "  " + decline + "  " + inspect + "  " + esc
}

// renderTypedConfirm renders the high-risk typed-confirmation prompt: the expected
// phrase, the input typed so far (with a caret), and the Enter/decline actions. The
// Enter label brightens once the typed phrase matches.
func renderTypedConfirm(th theme.Theme, p *pendingConfirm, width int) string {
	var b strings.Builder
	b.WriteString(th.Danger().Render(truncateCells(`This action is irreversible. Type "`+confirmPhrase+`" to approve:`, width)))
	b.WriteByte('\n')
	caret := th.Body().Reverse(true).Render(" ")
	b.WriteString(th.Body().Render(truncateCells("› "+p.confirmInput, width-1)) + caret)
	b.WriteByte('\n')
	enter := th.Dim().Render("Enter approve")
	if strings.EqualFold(strings.TrimSpace(p.confirmInput), confirmPhrase) {
		enter = th.Body().Render("Enter approve")
	}
	// In typed mode N is a typeable letter, so only Esc declines.
	b.WriteString(enter + "  " + th.Body().Reverse(true).Render(" Esc decline "))
	return b.String()
}

// compactArgs collapses a JSON args blob to one truncated line (whitespace squashed;
// truncated to max cells) — used by the activity tree's single-line tool rows. Secrets
// are masked first (redactArgs) so a token never reaches the ^X expanded row.
func compactArgs(s string, max int) string {
	s = strings.Join(strings.Fields(redactArgs(s)), " ")
	return truncateCells(s, max)
}

// argsBlock renders an args JSON blob as up to maxRows wrapped rows (whitespace squashed
// first), ellipsising the last row if the args overflow the budget. Secrets are masked
// first (redactArgs) so a credential never renders in the approval sheet.
func argsBlock(s string, width, maxRows int) string {
	s = strings.Join(strings.Fields(redactArgs(s)), " ")
	lines := strings.Split(wrapCells(s, width), "\n")
	if len(lines) > maxRows {
		lines = lines[:maxRows]
		lines[maxRows-1] = truncateCells(lines[maxRows-1]+" …", width)
	}
	return strings.Join(lines, "\n")
}
