package commands

import (
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/costledger"
)

func usd(v float64) *float64 { return &v }

// A turn costs a fraction of a cent. Two decimal places would render real spend as
// "$0.00" and teach the reader that the panel is broken — which is the one impression a
// cost display cannot afford to give.
func TestFormatUSDKeepsSmallAmountsVisible(t *testing.T) {
	cases := []struct {
		amount     float64
		lowerBound bool
		want       string
	}{
		// Real spend below the display floor is NOT zero. "$0.0000" would be a claim
		// about someone's bill that we have no basis for.
		{0.0000089, false, "< $0.0001"},
		{0.0123, false, "$0.0123"},
		{0.5, false, "$0.5000"},
		{12.5, false, "$12.50"}, // large enough that four decimals are noise
		// A lower bound TRUNCATES. Rounding 0.00256256 up to "≥ $0.0026" would assert a
		// floor the real spend might sit below — inverting the guarantee "≥" makes.
		{0.00256256, true, "≥ $0.0025"},
		{0.0123, true, "≥ $0.0123"},
		{0.00000123, true, "≥ $0.0000 (too small to show)"},
		// Exactly zero really is zero, and says so.
		{0, false, "$0.0000"},
	}
	for _, tc := range cases {
		if got := formatUSD(tc.amount, tc.lowerBound); got != tc.want {
			t.Errorf("formatUSD(%v, %v) = %q, want %q", tc.amount, tc.lowerBound, got, tc.want)
		}
	}
}

// An incomplete total must be visibly hedged AND explained. "≥" on its own invites the
// reader to assume rounding; naming the cause is what makes it read as a caveat.
func TestLowerBoundNoteNamesTheCause(t *testing.T) {
	unreported := lowerBoundNote(costledger.Snapshot{Unreported: 2})
	if !strings.Contains(unreported, "LOWER BOUND") {
		t.Errorf("note does not flag the total as a lower bound: %q", unreported)
	}
	if !strings.Contains(unreported, "2 calls reported no cost") {
		t.Errorf("note does not name the unreported calls: %q", unreported)
	}

	// Singular reads correctly too — "1 calls" undermines the credibility of a number
	// the whole panel is asking to be trusted.
	one := lowerBoundNote(costledger.Snapshot{Unreported: 1})
	if !strings.Contains(one, "1 call reported") || strings.Contains(one, "1 calls") {
		t.Errorf("singular is mis-pluralised: %q", one)
	}

	// Both causes at once are distinct statements and both must survive.
	both := lowerBoundNote(costledger.Snapshot{Unreported: 1, Incomplete: 3})
	if !strings.Contains(both, "1 call reported no cost") || !strings.Contains(both, "3 calls spent something") {
		t.Errorf("note dropped one of the two causes: %q", both)
	}
}

// The per-category line carries the call count, and flags a category whose own figure is
// incomplete — a session total that is exact except for its task spend should not look
// uniformly hedged.
func TestCostLineReportsUnreportedCalls(t *testing.T) {
	clean := costLine(costledger.OpSummary{Amount: 0.02, Calls: 4})
	if !strings.HasPrefix(clean, "$0.0200") || !strings.Contains(clean, "×4") {
		t.Errorf("clean line = %q", clean)
	}
	if strings.Contains(clean, "unreported") {
		t.Errorf("a fully-reported category should carry no caveat: %q", clean)
	}

	hedged := costLine(costledger.OpSummary{Amount: 0.02, Calls: 4, Unreported: 1})
	if !strings.HasPrefix(hedged, "≥ $") || !strings.Contains(hedged, "1 unreported") {
		t.Errorf("hedged line = %q", hedged)
	}
}

// The panel must never present an unmeasured session as an exact one. This is the
// end-to-end version of the ledger's central rule, at the surface a user actually reads.
func TestCostTextHedgesAnIncompleteSession(t *testing.T) {
	l := costledger.New()
	l.Record(backend.CostEvent{Op: costledger.RespondOp, Amount: usd(0.01), Complete: true, PromptTokens: 100, CachedTokens: 90})
	l.Record(backend.CostEvent{Op: "terminal_summarize", Amount: nil, Complete: true})

	got := renderCostPanel(l.Snapshot())
	if !strings.Contains(got, "≥ $") {
		t.Errorf("an unmeasured session was rendered as exact:\n%s", got)
	}
	if !strings.Contains(got, "LOWER BOUND") {
		t.Errorf("panel does not explain the hedge:\n%s", got)
	}
	// The cache ratio explains the bill sitting next to it.
	if !strings.Contains(got, "90.0%") {
		t.Errorf("panel omits the prompt-cache ratio:\n%s", got)
	}
	// The standing caveat is not optional: these figures are for proportion and trend,
	// and the spend is the DEPLOYMENT's rather than the reader's. It used to send them
	// to "your OpenRouter dashboard" for the real number — a dashboard they do not have,
	// for an account they do not hold, since the backend funds every call from its own
	// credential.
	if !strings.Contains(got, "proportion and trend") {
		t.Errorf("panel omits the trend caveat:\n%s", got)
	}
	if !strings.Contains(got, "not yours") {
		t.Errorf("panel does not say whose spend this is:\n%s", got)
	}
	if strings.Contains(got, "your bill") || strings.Contains(got, "your OpenRouter") {
		t.Errorf("panel attributes the spend to the reader:\n%s", got)
	}
}

// A session that has spent nothing yet says so plainly, and still carries the caveat —
// a reader forms their expectations before the first number appears.
func TestCostTextWithNoSpendYet(t *testing.T) {
	got := renderCostPanel(costledger.New().Snapshot())
	if !strings.Contains(got, "Nothing billed yet") {
		t.Errorf("empty panel = %q", got)
	}
	if strings.Contains(got, "≥") {
		t.Errorf("an empty session must not be hedged — there is nothing to hedge: %q", got)
	}
	if !strings.Contains(got, "proportion and trend") || !strings.Contains(got, "not yours") {
		t.Errorf("empty panel omits the caveat: %q", got)
	}
}

// Turn spend and task spend are reported separately because they answer different
// questions, and task spend is the half a user has no other way to see.
func TestCostTextSplitsTurnsFromTasks(t *testing.T) {
	l := costledger.New()
	l.Record(backend.CostEvent{Op: costledger.RespondOp, Amount: usd(0.02), Complete: true})
	l.Record(backend.CostEvent{Op: "terminal_summarize", Amount: usd(0.03), Complete: true})
	l.Record(backend.CostEvent{Op: "watcher_classify", Amount: usd(0.01), Complete: true})

	got := renderCostPanel(l.Snapshot())
	for _, want := range []string{"turns", "tasks", "terminal_summarize", "watcher_classify", "respond (turns)"} {
		if !strings.Contains(got, want) {
			t.Errorf("panel omits %q:\n%s", want, got)
		}
	}
	// Tasks outspent turns here; the breakdown leads with the biggest line.
	if i, j := strings.Index(got, "terminal_summarize"), strings.Index(got, "watcher_classify"); i > j {
		t.Errorf("breakdown is not ordered most-expensive-first:\n%s", got)
	}
}

// A backend that reports no cost at all must be NAMED, not rendered as "≥ $0.0000 over
// 12 calls" — a figure that is technically true, reads as a malfunction, and hides the
// actual explanation.
func TestCostTextExplainsABackendThatReportsNothing(t *testing.T) {
	l := costledger.New()
	for i := 0; i < 3; i++ {
		l.Record(backend.CostEvent{Op: costledger.RespondOp, Amount: nil, Complete: true})
	}
	got := renderCostPanel(l.Snapshot())
	if !strings.Contains(got, "reported no cost for any") {
		t.Errorf("panel does not explain the empty backend:\n%s", got)
	}
	if strings.Contains(got, "≥ $0.0000") {
		t.Errorf("panel rendered a meaningless zero total:\n%s", got)
	}
	// One reported call is enough to switch back to a real (hedged) total.
	l.Record(backend.CostEvent{Op: costledger.RespondOp, Amount: usd(0.01), Complete: true})
	got = renderCostPanel(l.Snapshot())
	if !strings.Contains(got, "≥ $0.0100") {
		t.Errorf("a partially-reported session should show its hedged total:\n%s", got)
	}
}
