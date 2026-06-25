package ui

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestStreaming_MarkdownDoesNotChurn is the end-to-end regression for the cockpit-footer churn the
// user reported: "a ~5-line window, text scrolls up off the top, then flicks over and displays
// properly once a paragraph finishes." Root cause: a markdown-dense paragraph was WITHHELD from
// native scrollback until it sealed on "\n\n", so the whole growing paragraph piled into the live
// footer, which lastLines() caps to maxLiveRows — showing only the tail while earlier rows scrolled
// off the top into nowhere (neither scrollback nor footer) until the seal dumped the block in.
//
// With line-level commit, the paragraph settles into scrollback as it streams, so the UN-COMMITTED
// tail (all the footer ever has to hold) stays small. This test streams a markdown paragraph CHAR BY
// CHAR and asserts that tail never exceeds the footer's live budget — i.e. the footer never has to
// hide content, so it cannot churn. Before the fix the tail grew with the paragraph (to ~12+ rows
// here) and this assertion would fail.
func TestStreaming_MarkdownDoesNotChurn(t *testing.T) {
	full := "Here is a **bold** lead-in, then a long stretch of prose that wraps across many rows so we can watch the footer while it streams: " +
		strings.Repeat("alpha beta gamma delta ", 18) + "and a `code` span near the end to keep it on the markdown path."
	turn := &TurnCell{ID: "turn_1", UserText: "Q", State: TurnActive, Phase: domain.PhaseGenerating, Steps: []TurnStep{proseStep("", true)}}
	m := armedModel(turn)
	m.rows = 24 // a realistic terminal height

	worst, worstAt := 0, ""
	var maxTotal, maxFlushed int
	for i := 1; i <= len(full); i++ {
		turn.Steps[0].Text = full[:i]
		_ = m.flushActiveTurn()
		total := len(m.activeTurnRows(turn))
		if total > maxTotal {
			maxTotal = total
		}
		if turn.FlushedRows > maxFlushed {
			maxFlushed = turn.FlushedRows
		}
		uncommitted := total - turn.FlushedRows // what the live footer must hold this frame
		if uncommitted > worst {
			worst = uncommitted
			worstAt = full[:i]
		}
	}
	// Non-vacuity: the fixture must actually grow past the cap, and flushing must actually advance —
	// otherwise the bound below could pass without exercising line-commit (e.g. a shortened fixture).
	if maxTotal <= maxLiveRows {
		t.Fatalf("fixture too short: paragraph only reached %d rows (need > maxLiveRows=%d) — the test would be vacuous", maxTotal, maxLiveRows)
	}
	if maxFlushed == 0 {
		t.Fatalf("nothing ever flushed (FlushedRows stayed 0) — line-commit did not run, the bound below is vacuous")
	}
	// The un-committed prose tail must stay within the footer's live budget (maxLiveRows): if it
	// exceeds the cap the footer hides rows and churns. A short open span (mid-**bold** / mid-`code`)
	// briefly withholds a row or two — well under the cap — but it must never grow with the paragraph.
	if worst > maxLiveRows {
		t.Errorf("un-committed footer tail peaked at %d rows (> maxLiveRows=%d) — the footer would hide/churn content.\nworst frame ended: …%q",
			worst, maxLiveRows, tailOf(worstAt, 40))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
