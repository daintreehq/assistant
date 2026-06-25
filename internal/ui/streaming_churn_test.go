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
		strings.Repeat("alpha beta gamma delta ", 32) + "and a `code` span near the end to keep it on the markdown path."
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

// TestStreaming_BulletListDoesNotChurn is the regression for the bullet-list churn (the case that
// persisted after the paragraph fix — confirmed from a real session log). glamour re-flows a list as
// items are added (each new item shifts earlier items' indentation), so a streaming list CAN'T
// commit line by line and is WITHHELD until it seals on "\n\n". It must therefore render WHOLE in the
// live footer; the footer cap (maxLiveRows) is sized to hold a typical list intact so its head never
// scrolls off the top (the churn). This streams a markdown answer with a multi-row bullet list char
// by char and asserts the un-committed tail (what the footer holds) never exceeds the cap — while
// also confirming the list is genuinely tall enough to have churned under the old 8-row cap.
func TestStreaming_BulletListDoesNotChurn(t *testing.T) {
	full := "Here is the project summary you asked for:\n\n" +
		"**Key details:**\n" +
		"- **Branch:** `main` (the only worktree, currently stable)\n" +
		"- **Last commit:** \"feat(ui): extend line-level prose commit to markdown spans\"\n" +
		"- **Agents configured:** antigravity, claude, codex, copilot, cursor, gemini, goose, kiro, opencode\n" +
		"- **Currently open:** one Claude agent (waiting) plus one plain terminal in the dock\n\n" +
		"That covers the essentials."
	turn := &TurnCell{ID: "turn_1", UserText: "Q", State: TurnActive, Phase: domain.PhaseGenerating, Steps: []TurnStep{proseStep("", true)}}
	m := armedModel(turn)
	m.rows = 40 // a roomy terminal so the cap (not the terminal height) is the binding bound

	worst, maxFlushed := 0, 0
	for i := 1; i <= len(full); i++ {
		turn.Steps[0].Text = full[:i]
		_ = m.flushActiveTurn()
		if turn.FlushedRows > maxFlushed {
			maxFlushed = turn.FlushedRows
		}
		uncommitted := len(m.activeTurnRows(turn)) - turn.FlushedRows
		if uncommitted > worst {
			worst = uncommitted
		}
	}
	t.Logf("worst un-committed footer tail while streaming the list: %d rows (cap %d)", worst, maxLiveRows)
	// Non-vacuity: the leading paragraph must actually line-flush (so the list is what's measured),
	// and the list must exceed the OLD 8-row cap (so it WOULD have churned before — this is what
	// fails if maxLiveRows is reverted to 8). The fixture is sized to land here with margin below 16,
	// so the test isn't fragile to a one-row glamour shift.
	if maxFlushed == 0 {
		t.Fatal("nothing ever flushed — the leading paragraph did not line-commit, the measure is vacuous")
	}
	if worst <= 8 {
		t.Fatalf("fixture too short: list reached only %d un-committed rows (need > 8 to exercise the raised cap)", worst)
	}
	// The cap must hold the whole withheld list, so the footer never truncates/churns its head.
	if worst > maxLiveRows {
		t.Errorf("un-committed footer tail peaked at %d rows (> maxLiveRows=%d) — the bullet list would hide/churn its head", worst, maxLiveRows)
	}
}
