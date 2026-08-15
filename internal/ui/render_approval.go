package ui

import (
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/ui/theme"
)

// render_approval.go renders the approval sheet: a full-width
// bordered card directly ABOVE the composer, leading with the consequence, visually
// defaulting to DECLINE, understandable with color stripped.

// titleFor returns the risk-specific question.
func titleFor(req tools.ConfirmRequest) string {
	// daintree.call is the raw MCP escape hatch (RiskSystem). Name it specifically so
	// the riskiest forge writes don't hide under the generic system-level title; the
	// exact-name match precedes the risk switch so a future RiskSystem tool still gets
	// the generic phrasing.
	if req.ToolName == "daintree.call" {
		return "Call a raw MCP tool?"
	}
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

	// Top border + title. The glyph prefix is measured WITH the title, not bolted on after
	// a `width-2` truncate: at a width under 2 cells that arithmetic goes non-positive and
	// the prefix alone then overruns the budget.
	b.WriteString(th.Blocked().Render(truncateCells(g.Approval+" "+titleFor(req), width)))
	b.WriteByte('\n')
	// Affects (the consequence to lead with).
	b.WriteString(th.Body().Render(truncateCells("affects  "+consequenceFor(req), width)))
	b.WriteByte('\n')
	// Tool (dim secondary).
	b.WriteString(th.Dim().Render(truncateCells("tool     "+req.ToolName, width)))
	b.WriteByte('\n')
	// Risk class (dim secondary) — surfaces the safety taxonomy bucket at the moment
	// of decision so the human can see WHICH class of action they're approving, not
	// just its consequence prose. Column-aligned with the affects/tool rows.
	b.WriteString(th.Dim().Render(truncateCells("risk     "+string(req.Risk), width)))

	if p.showArgs {
		if req.Summary != "" {
			b.WriteByte('\n')
			// 'about' — this is the tool's own description (what it does), NOT a per-call
			// rationale, so it must not be mislabeled "reason".
			b.WriteString(th.Dim().Render(truncateCells("about    "+req.Summary, width)))
		}
		if len(req.Args) > 0 {
			b.WriteByte('\n')
			b.WriteString(th.Dim().Render(truncateCells("args", width)))
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
		b.WriteString(renderActionRows(th, req, width))
	}
	return b.String()
}

// actionGap separates two controls sharing a row. Two spaces, not one: at one space
// "N decline V inspect" reads as a single phrase.
const actionGap = "  "

// actionRows renders a chosen layout, one explicit "\n"-delimited row per group, each
// bounded to width. Explicit rows are the whole point: the footer's height budget counts
// "\n"s, so a row the host soft-wraps makes the fixed bottom band taller than the model
// believes and corrupts the inline layout.
func actionRows(rows [][]string, width int) string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, truncateCells(strings.Join(row, actionGap), width))
	}
	return strings.Join(out, "\n")
}

// rowsFit reports whether every row of a layout is within width (cell-measured — the
// controls carry SGR bytes, so len() would over-count by however many the theme emitted).
func rowsFit(rows [][]string, width int) bool {
	for _, row := range rows {
		if cellWidth(strings.Join(row, actionGap)) > width {
			return false
		}
	}
	return true
}

// renderActionRows renders the standard single-key controls — DECLINE is the visual
// default (inverse) — into one or more EXPLICITLY measured rows. The A (allow a bounded
// number of further calls) and F (allow for the whole session) affordances appear only for
// risk classes eligible for the session allow-list.
//
// Laid out from a fixed set of candidates rather than word-wrapped, because these are
// semantic units: a generic wrapper would happily break "A allow 5×" in half or strand a
// key from its verb. The candidates are ordered widest-first and are chosen by measurement,
// and every one of them leads with the immediate approve/decline decision so the thing the
// user is actually being asked survives at the top of a cramped sheet.
func renderActionRows(th theme.Theme, req tools.ConfirmRequest, width int) string {
	approve := th.Body().Render("Y approve")
	decline := th.Body().Reverse(true).Render(" N decline ")
	inspect := th.Dim().Render("V inspect")
	// Spell out what Escape does. The bare "Esc" was readable in context, but naming the
	// verb costs a few cells and restates the fail-closed default at the point of decision.
	esc := th.Dim().Render("Esc decline")

	layouts := [][][]string{
		{{approve, decline, inspect, esc}},
		{{approve, decline}, {inspect, esc}},
	}
	if rememberable(req.Risk) {
		allow := th.Dim().Render(fmt.Sprintf("A allow %d×", approveDefaultCount))
		always := th.Dim().Render("F always")
		layouts = [][][]string{
			{{approve, decline, allow, always, inspect, esc}},
			{{approve, decline, inspect}, {allow, always, esc}},
			{{approve, decline}, {inspect, esc}, {allow, always}},
		}
	}
	for _, l := range layouts {
		if rowsFit(l, width) {
			return actionRows(l, width)
		}
	}
	// Narrower than the core approve/decline pair (~22 cells) — a terminal already at or
	// below the height/width floor that has its own "terminal too small" fallback. Keep the
	// narrowest layout and let each row truncate rather than invent a cryptic keys-only UI:
	// truncation still guarantees explicit, width-bounded rows.
	return actionRows(layouts[len(layouts)-1], width)
}

// renderTypedConfirm renders the high-risk typed-confirmation prompt: the expected
// phrase, the input typed so far (with a caret), and the Enter/decline actions. The
// Enter label brightens once the typed phrase matches.
func renderTypedConfirm(th theme.Theme, p *pendingConfirm, width int) string {
	var b strings.Builder
	b.WriteString(th.Danger().Render(truncateCells(`This action is irreversible. Type "`+confirmPhrase+`" to approve:`, width)))
	b.WriteByte('\n')
	// The caret is one more cell beyond the typed text, so the text gets width-1 — and at
	// a width under 1 there is no room for even the caret.
	line := th.Body().Render(truncateCells("› "+p.confirmInput, width-1))
	if width >= 1 {
		line += th.Body().Reverse(true).Render(" ")
	}
	b.WriteString(line)
	b.WriteByte('\n')
	enter := th.Dim().Render("Enter approve")
	if strings.EqualFold(strings.TrimSpace(p.confirmInput), confirmPhrase) {
		enter = th.Body().Render("Enter approve")
	}
	// In typed mode N is a typeable letter, so only Esc declines.
	//
	// Measured and stacked rather than joined blind: this is the sheet guarding git
	// history rewrites and system-level actions, so it is the last place that may hand the
	// host terminal a line to soft-wrap and quietly outgrow the footer's row budget.
	decline := th.Body().Reverse(true).Render(" Esc decline ")
	rows := [][]string{{enter, decline}}
	if !rowsFit(rows, width) {
		rows = [][]string{{enter}, {decline}}
	}
	b.WriteString(actionRows(rows, width))
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
