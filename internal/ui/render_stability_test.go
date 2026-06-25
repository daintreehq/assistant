package ui

import (
	"math/rand"
	"strings"
	"testing"
)

// committablePrefix returns the rows the REAL renderProse would commit to scrollback this frame for
// a single growing paragraph (the flush path, withholdGrowing=true). It calls the production
// function directly so the oracle can never drift from the implementation it is validating.
func committablePrefix(m Model, text string) []string {
	out := renderProse(m.md, proseStep(text, true), m.contentW(), true)
	if out == "" {
		return nil
	}
	return strings.Split(out, "\n")
}

// TestFuzz_CommittedRowsNeverChange is the SAFETY oracle for the line-level commit. It streams
// thousands of random WELL-FORMED markdown paragraphs token by token — the shape LLMs actually emit:
// plain words, punctuation, snake_case, and ATOMIC closed spans (**bold**, *italic*, `code`, _em_).
// Whenever proseTailCommittable + the row-level guard let us commit, it records the committed rows
// (monotonic, like FlushedRows) and asserts that EVERY later render still reproduces those exact rows
// byte-for-byte. A committed row that ever changes would be frozen-stale-bytes corruption in native
// scrollback. This validates the line-commit against REAL glamour behaviour.
//
// LIMITATION (documented, not tested here): CommonMark emphasis pairing is GLOBALLY unstable under
// append for pathological *raw delimiter soup* (e.g. "_a_b_c" mid-stream) — a consumed span can
// re-pair when more delimiters arrive. No cheap local check is sound there, and LLMs never emit it;
// if it ever occurred, flushActiveTurn's flushedRowsText reflow guard HOLDS and sealTail reconciles
// by row count, so the worst case is one row of cosmetically-stale styling — no dup, no text loss.
func TestFuzz_CommittedRowsNeverChange(t *testing.T) {
	m := testModel(80)
	cw := m.contentW()
	// Well-formed markdown tokens: closed spans are ATOMIC (as an LLM emits a finished **bold**),
	// plus plenty of plain words / spaces / punctuation / snake_case. Includes nested, adjacent, and
	// punctuation-flanked spans (the trickier well-formed shapes) to stress the row-level oracle.
	tokens := []string{
		"alpha", "beta", "gamma", "delta", "the", "a", "value", "result", "status", "agent",
		" ", " ", " ", " ", " ", " ", // bias toward spaces so paragraphs wrap
		"**bold**", "*italic*", "`code`", "`terminal.getStatus`", "_em_", "__strong__",
		"***bolditalic***", "**a phrase here**", "**bold with `code` inside**", "*a*", "*b*",
		"(**bold**)", "**bold**,", "`code`.", "well-formed", "snake_case_name", "agent_id",
		"terminal_state", "C", "value42",
		".", ",", "!", "?", "(", ")", ":", ";", "-", "—",
	}
	for iter := 0; iter < 1500; iter++ {
		var sb strings.Builder
		var committed []string // the monotonic committed prefix (already "in scrollback")
		nTok := 6 + rng4000(iter)%60
		for k := 0; k < nTok; k++ {
			tok := tokens[seedRand(iter, k).Intn(len(tokens))]
			// Join atomic word/span tokens with a space so spans stay well-formed and separated
			// (avoids synthesising raw delimiter soup the realistic corpus is meant to exclude).
			if sb.Len() > 0 && tok != " " && !strings.HasPrefix(tok, ".") && !strings.HasPrefix(tok, ",") &&
				tok != "!" && tok != "?" && tok != ":" && tok != ";" && tok != ")" && tok != "-" && tok != "—" {
				sb.WriteByte(' ')
			}
			sb.WriteString(tok)
			text := sb.String()
			cand := committablePrefix(m, text)
			// SAFETY: the previously committed rows must still be reproduced exactly by this render.
			if len(committed) > 0 {
				full := strings.TrimRight(m.md.Render(text, cw, false).ANSI, "\n")
				allRows := strings.Split(full, "\n")
				if len(committed) > len(allRows) {
					t.Fatalf("iter=%d: render shrank below committed prefix\ntext=%q\ncommitted(%d rows)=%q",
						iter, text, len(committed), committed)
				}
				if got := strings.Join(allRows[:len(committed)], "\n"); got != strings.Join(committed, "\n") {
					t.Fatalf("iter=%d: a COMMITTED row changed in a later render (scrollback corruption)\ntext=%q\ncommitted=%q\nnow     =%q",
						iter, text, committed, got)
				}
			}
			// Advance the committed frontier monotonically (flushActiveTurn only ever grows it).
			if len(cand) > len(committed) {
				// The new candidate must extend the old committed prefix exactly (it always does when
				// the predicate is sound — same render, more rows).
				if len(committed) > 0 && strings.Join(cand[:len(committed)], "\n") != strings.Join(committed, "\n") {
					t.Fatalf("iter=%d: candidate prefix does not extend committed prefix\ntext=%q", iter, text)
				}
				committed = append([]string(nil), cand...)
			}
		}
	}
}

// seedRand returns a deterministic RNG per (iter, step) so the fuzz corpus is reproducible across
// runs (math/rand with a fixed seed; the workflow-script ban on rand does not apply to tests).
func seedRand(iter, step int) *rand.Rand {
	return rand.New(rand.NewSource(int64(iter)*1_000_003 + int64(step)))
}

func rng4000(iter int) int { return int(rand.New(rand.NewSource(int64(iter) * 7)).Int31()) }

// TestInlineSpanStability locks the two facts the line-level commit relies on (the design rests on
// these, so they must hold for the bundled glamour version):
//   - a CLOSED inline span (**bold**, `code`) is width-final: appending text never changes an
//     earlier wrapped row, so its rows are safe to commit.
//   - an OPEN span (**bold, `code) renders its delimiter LITERALLY and then RESTYLES + RE-WRAPS the
//     earlier rows when it closes — so committing an open-span row would corrupt scrollback. This is
//     why rowHasOpenableDelimiter withholds any row that still shows a literal delimiter.
func TestInlineSpanStability(t *testing.T) {
	m := testModel(80)
	cw := m.contentW()
	rowsOf := func(s string) []string {
		return strings.Split(strings.TrimRight(m.md.Render(s, cw, false).ANSI, "\n"), "\n")
	}
	firstNonLastChange := func(a, b []string) int {
		for i := 0; i < len(a)-1 && i < len(b); i++ {
			if a[i] != b[i] {
				return i
			}
		}
		return -1
	}

	// CLOSED bold: earlier rows immutable under append.
	cb := "Intro then **a rather long bold phrase that wraps across more than one visual row for sure** then trailing " + strings.Repeat("more words ", 8)
	if c := firstNonLastChange(rowsOf(cb), rowsOf(cb+"and yet more trailing words appended to grow the paragraph further")); c != -1 {
		t.Errorf("closed-bold: a non-last row changed under append at row %d (expected immutable)", c)
	}
	// CLOSED code: earlier rows immutable under append.
	cc := "Use the `terminal.getStatus` call and then keep writing a long line of prose " + strings.Repeat("padding words ", 6)
	if c := firstNonLastChange(rowsOf(cc), rowsOf(cc+"with still more padding to push the wrap further out yet again")); c != -1 {
		t.Errorf("closed-code: a non-last row changed under append at row %d (expected immutable)", c)
	}
	// OPEN bold: closing it DOES change an earlier row — proving we must withhold open-span rows.
	openTail := "Intro then **a rather long bold phrase that wraps across more than one visual row for sure"
	if c := firstNonLastChange(rowsOf(openTail), rowsOf(openTail+"** done")); c < 0 {
		t.Errorf("open-bold: expected an earlier row to change when the span closes (it must be withheld), but none did")
	}
}
