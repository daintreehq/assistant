package commands

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/costledger"
)

// cost.go renders `/cost`: what this session has spent on the user's own upstream key.
//
// The tester pays the model bill and this prompt is large, so cost visibility is the
// difference between a tester who trusts the tool and one who quietly stops using it
// because they cannot tell what it is spending.
//
// Every rendering decision here follows from one rule: ABSENT MEANS UNKNOWN, NEVER FREE.
// A total that silently coerced an unreported call to zero would under-report someone's
// bill while looking exactly like a receipt — which is worse than showing nothing,
// because it would be believed. So an incomplete total is rendered "≥ $x", and the panel
// says how many calls it could not account for.

// costText is the `/cost` panel.
func costText(a *app.App) string { return renderCostPanel(a.CostLedger.Snapshot()) }

// renderCostPanel is the pure rendering half, split out so the presentation rules —
// which are the substance of this file — are testable without booting an App.
func renderCostPanel(s costledger.Snapshot) string {
	var b strings.Builder
	b.WriteString("Upstream spend for this session.\n\n")

	if s.Calls == 0 {
		b.WriteString("Nothing billed yet — no turns and no utility tasks have run.\n\n")
		b.WriteString(costFootnote)
		return b.String()
	}
	// NOTHING reported, across every call: this backend does not report cost at all.
	// Say that, rather than rendering "≥ $0.0000 over 12 calls" — a number that is
	// technically true, reads as a malfunction, and hides the actual explanation.
	// Derived from what happened rather than from the advertised capability, so it stays
	// right even if a backend starts or stops reporting mid-session.
	if s.Unreported == s.Calls {
		b.WriteString(fmt.Sprintf("This backend reported no cost for any of the %d billed call(s) this session,\n"+
			"so there is nothing to total. Your OpenRouter dashboard has the real figures.\n\n", s.Calls))
		b.WriteString(costFootnote)
		return b.String()
	}

	b.WriteString(padRight("session", 12) + ": " + formatUSD(s.Observed, s.LowerBound) + "\n")
	b.WriteString(padRight("turns", 12) + ": " + costLine(s.Turns) + "\n")
	// Utility tasks are the spend a user has no other way to see: summarize, extract and
	// watcher-classify calls fire from tools and background supervision without ever
	// appearing as a turn, and a busy session runs dozens of them.
	b.WriteString(padRight("tasks", 12) + ": " + costLine(s.Tasks) + "\n")

	if ratio, ok := s.CacheHitRatio(); ok {
		// The cache ratio explains the bill rather than merely accompanying it: the
		// backend's prompt assembly exists to keep ~18k tokens of tool schemas
		// byte-stable, and a collapse here is the first symptom of a regression that
		// costs real money.
		// Scoped honestly: these token counts come from the main completion, since the
		// selector's usage is not exposed per call.
		b.WriteString(padRight("cache", 12) + ": " +
			strconv.FormatFloat(ratio*100, 'f', 1, 64) + "% of main-call prompt tokens served from cache\n")
	}

	if s.LowerBound {
		b.WriteString("\n" + lowerBoundNote(s))
	}

	if len(s.ByOp) > 1 {
		b.WriteString("\nBy call:\n")
		for _, row := range s.ByOp {
			label := row.Op
			if row.Op == costledger.RespondOp {
				label = "respond (turns)"
			}
			line := fmt.Sprintf("  %s  %s  ×%d", padRight(label, 26), formatUSD(row.Amount, row.LowerBound()), row.Calls)
			if row.Unreported > 0 {
				line += fmt.Sprintf(" (%d unreported)", row.Unreported)
			}
			if row.Incomplete > 0 {
				line += fmt.Sprintf(" (%d partial)", row.Incomplete)
			}
			b.WriteString(line + "\n")
		}
	}

	b.WriteString("\n" + costFootnote)
	return b.String()
}

// costFootnote is the standing caveat. It appears whether or not anything has been spent
// yet, because the two claims it makes are always true and both matter before a user
// starts reasoning about the numbers.
const costFootnote = "These are OpenRouter's own reported figures, not estimates — but the OpenRouter\n" +
	"dashboard is the authority on your bill. Use these for proportion and trend.\n" +
	"Counted since this process launched, or since the last /clear; turns the supervisor\n" +
	"daemon ran while detached are not included. Runbook learning, when enabled, is billed\n" +
	"by OpenRouter but reported by nothing and so appears in no total here."

// costLine renders one category's spend plus its call count.
func costLine(s costledger.OpSummary) string {
	out := formatUSD(s.Amount, s.LowerBound()) + "  ×" + strconv.Itoa(s.Calls)
	if s.Unreported > 0 {
		out += fmt.Sprintf(" (%d unreported)", s.Unreported)
	}
	if s.Incomplete > 0 {
		out += fmt.Sprintf(" (%d partial)", s.Incomplete)
	}
	return out
}

// lowerBoundNote explains WHY a total is a floor, in the user's terms. "≥" on its own
// invites the reader to assume rounding; naming the cause is what makes it act like a
// caveat instead of a typo.
func lowerBoundNote(s costledger.Snapshot) string {
	var reasons []string
	if s.Unreported > 0 {
		reasons = append(reasons, fmt.Sprintf("%s reported no cost", plural(s.Unreported, "call", "calls")))
	}
	if s.Incomplete > 0 {
		reasons = append(reasons, fmt.Sprintf("%s spent something the provider could not measure",
			plural(s.Incomplete, "call", "calls")))
	}
	return "This total is a LOWER BOUND, not a sum: " + strings.Join(reasons, ", and ") + "."
}

// smallestDisplayedUSD is the finest amount the four-decimal form can show. Real spend
// below it gets "< $0.0001" rather than a rounded-to-nothing "$0.0000".
const smallestDisplayedUSD = 0.0001

// formatUSD renders an amount as dollars, at the scale these figures actually live at.
//
// Three rules, each fixing a way a naive rendering lies about money:
//
//   - A single turn costs a fraction of a cent, so two decimals would print "$0.00" for
//     real spend and teach the reader the panel is broken. Four decimals below $10, two
//     above, where the extra digits are noise.
//   - Spend too small for even four decimals is rendered "< $0.0001", never "$0.0000".
//     Zero is a claim about someone's bill; "smaller than I can show" is not the same
//     claim, and this panel must never make the first one by accident.
//   - A LOWER BOUND is TRUNCATED, not rounded. Rounding 0.00256 up to "≥ $0.0026" states
//     a floor the actual spend might sit below — which inverts the one guarantee the "≥"
//     is there to make.
func formatUSD(amount float64, lowerBound bool) string {
	digits := 4
	if amount >= 10 {
		digits = 2
	}
	if lowerBound {
		// Truncate toward zero at the displayed precision, so the printed figure is
		// never above the amount it bounds.
		scale := math.Pow(10, float64(digits))
		truncated := math.Trunc(amount*scale) / scale
		if amount > 0 && truncated < smallestDisplayedUSD {
			return "≥ $0.0000 (too small to show)"
		}
		return "≥ $" + strconv.FormatFloat(truncated, 'f', digits, 64)
	}
	if amount > 0 && amount < smallestDisplayedUSD {
		return "< $0.0001"
	}
	return "$" + strconv.FormatFloat(amount, 'f', digits, 64)
}

// plural renders "1 call" / "3 calls".
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.Itoa(n) + " " + many
}
