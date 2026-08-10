package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
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

// TestStreaming_HeadingThenListDoesNotChurn is the regression for the "title + blank line + bullet
// list" churn (confirmed from session log ses_34766218 — a `## heading` / `### heading` answer with
// bullet lists). THE BUG: glamour renders a trailing ATX heading UNPADDED when it is the last block
// but PADDED to full width once a block follows it. So the heading committed (unpadded) when its
// "\n\n" sealed, then RE-RENDERED padded the instant the following list sealed — changing an
// already-committed row, tripping flushActiveTurn's reflow guard, and FREEZING the flush for the
// REST of the turn. Every later section then piled into the capped footer and churned until the final
// seal ("carries on … restores at the very end"). renderCompletedBlocks fixes it by committing each
// block in its stable "followed-by-content" form. This streams the real multi-section answer and
// asserts the un-committed tail never exceeds the cap — without the fix it reaches ~22 rows.
func TestStreaming_HeadingThenListDoesNotChurn(t *testing.T) {
	full := "Here's the summary:\n\n" +
		"## Daintree Assistant\n\n" +
		"This is **Daintree's local operations officer** — a single native **Go** binary for the Daintree platform.\n\n" +
		"### What it does\n\n" +
		"- **Orchestrates Daintree operations** — spawns and supervises visible agent terminals, manages worktrees\n" +
		"- **Never edits files directly** — when changes are needed it spawns a visible agent inside Daintree\n" +
		"- **Powered by DeepSeek AI** (`deepseek-v4-flash`) for all model tiers including watchers\n\n" +
		"### Key architecture\n\n" +
		"- **Go 1.25.8+**, pure Go with no CGO — SQLite via `modernc.org/sqlite`, Bubble Tea v2 for the cockpit\n" +
		"- **Three run modes**: interactive cockpit, classic REPL, and one-shot\n" +
		"- **Foreground-only scheduler** — timers and watchers tick in-process only while the assistant is open\n\n" +
		"### Current state\n\n" +
		"Single worktree (`main`), 3 terminals, no open inbox items."
	turn := &TurnCell{ID: "turn_1", UserText: "Q", State: TurnActive, Phase: domain.PhaseGenerating, Steps: []TurnStep{proseStep("", true)}}
	m := armedModel(turn)
	m.rows = 40 // roomy: the cap, not the terminal height, is the binding bound

	worst, maxFlushed, worstAt := 0, 0, ""
	for i := 1; i <= len(full); i++ {
		turn.Steps[0].Text = full[:i]
		_ = m.flushActiveTurn()
		if turn.FlushedRows > maxFlushed {
			maxFlushed = turn.FlushedRows
		}
		if unc := len(m.activeTurnRows(turn)) - turn.FlushedRows; unc > worst {
			worst, worstAt = unc, full[max0(i-30):i]
		}
	}
	t.Logf("worst un-committed footer tail across the heading+list answer: %d rows (cap %d), at …%q", worst, maxLiveRows, worstAt)
	// Non-vacuity: blocks must actually flush (so a stuck flush is what we'd catch).
	if maxFlushed == 0 {
		t.Fatal("nothing ever flushed — the trace is vacuous")
	}
	// The flush must keep advancing section by section; if the heading-reflow guard-trip froze it,
	// the whole rest of the answer would pile up far past the cap (≈22 rows without the fix).
	if worst > maxLiveRows {
		t.Errorf("un-committed footer tail peaked at %d rows (> maxLiveRows=%d) — the flush froze on a heading-before-list and the rest of the answer churned", worst, maxLiveRows)
	}
}

func max0(v int) int {
	if v < 0 {
		return 0
	}
	return v
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
	// The footer's live budget must hold the whole withheld list, so it never tail-truncates/churns
	// its head.
	if worst > m.liveBudget() {
		t.Errorf("un-committed footer tail peaked at %d rows (> liveBudget=%d) — the bullet list would hide/churn its head", worst, m.liveBudget())
	}
}

// TestStreaming_TallListNotTruncated is the regression for the user's FINAL residual: a "really long"
// bullet list (the real 7-item "Key architecture" list from session ses_34766218 reaches ~17 rows at
// an 80-col pane — OVER the old static 16-row cap) had its head tail-truncated by lastLines and
// scrolled off the live footer. The fix replaces the static cap with a dynamic budget
// (liveBudgetFor = the height available above the bottom band), so a list up to the terminal height
// renders WHOLE. This streams the full real answer and asserts that at the frame where the list is
// tallest, the footer actually SHOWS the list's HEAD (the first item) — not just that the tail fits a
// recomputed number. Under the old 16-row cap the head is truncated and this fails.
func TestStreaming_TallListNotTruncated(t *testing.T) {
	const (
		archHead = "ARCHHEADGO" // marker in the FIRST Key-architecture item (top of the tall list)
		archTail = "ARCHTAILTESTS"
	)
	full := "Here's the summary:\n\n" +
		"## Daintree Assistant\n\n" +
		"This is **Daintree's local operations officer** — a single native **Go** binary that acts as a command-line orchestration assistant for the Daintree platform. It lives at `/Users/x/assistant` on `main`.\n\n" +
		"### What it does\n\n" +
		"- **Orchestrates Daintree operations** — spawns and supervises visible agent terminals, manages worktrees, runs recipes, schedules timers\n" +
		"- **Never edits files directly** — when file changes are needed it spawns a visible agent inside Daintree and supervises it\n" +
		"- **Powered by DeepSeek AI** (`deepseek-v4-flash`) for all model tiers — orchestration, watchers, summaries, classification\n\n" +
		"### Key architecture\n\n" +
		"- **" + archHead + " 1.25.8+**, pure Go with no CGO — SQLite via `modernc.org/sqlite`, Bubble Tea v2 for the TUI cockpit\n" +
		"- **Three run modes**: interactive cockpit (inline TUI, not full-screen), classic REPL, one-shot with optional JSONL\n" +
		"- **Embedded host** (`host --stdio`) — a stdio NDJSON transport that Daintree drives the runtime through\n" +
		"- **Foreground-only scheduler** — timers and watchers tick in-process only while the assistant is open\n" +
		"- **Skill system** — procedural runbooks injected into context on demand via `skill.find`/`skill.load`\n" +
		"- **Permission tiers**: supervisor (read-only) → operator (+spawn) → system (+git/destructive)\n" +
		"- **~980+ " + archTail + "** across 44 packages, no network dependencies in tests\n\n" +
		"### Current state\n\nSingle worktree (`main`), 3 terminals, no open inbox items."

	turn := &TurnCell{ID: "turn_1", UserText: "Q", State: TurnActive, Phase: domain.PhaseGenerating, Steps: []TurnStep{proseStep("", true)}}
	m := armedModel(turn)
	m.footerRows = new(int) // armedModel/testModel don't allocate it; scrollbackChunkRows is nil-safe anyway
	m.rows = 40             // budget ≈ 32 > the ~17-row list; the old static 16-cap would truncate it

	worst, worstFooter := 0, ""
	for i := 1; i <= len(full); i++ {
		turn.Steps[0].Text = full[:i]
		_ = m.flushActiveTurn()
		if unc := len(m.activeTurnRows(turn)) - turn.FlushedRows; unc > worst {
			worst = unc
			worstFooter = ansi.Strip(m.footer())
		}
	}
	t.Logf("tall list: worst un-committed tail = %d rows, liveBudget = %d", worst, m.liveBudget())
	// Non-vacuity: the list must genuinely exceed the OLD static 16-row cap, or the dynamic budget
	// isn't being exercised (this is what made the user's real answer still churn).
	if worst <= maxLiveRows {
		t.Fatalf("fixture too short: tall list only reached %d rows (need > maxLiveRows=%d to exercise the dynamic budget)", worst, maxLiveRows)
	}
	// THE assertion: at the tallest frame the footer SHOWS the list HEAD. Under the old static 16-row
	// cap the head scrolled off (lastLines kept only the bottom 16 rows) — the user-visible churn.
	// With the dynamic budget the whole list is shown, so the head is present.
	if !strings.Contains(worstFooter, archHead) {
		t.Errorf("the list HEAD %q is not visible in the footer at the tallest frame (%d rows, budget %d) — it was tail-truncated/churned off the top:\n%s",
			archHead, worst, m.liveBudget(), worstFooter)
	}
}
