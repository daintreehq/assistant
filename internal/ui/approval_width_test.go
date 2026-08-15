package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// approval_width_test.go pins the approval controls as EXPLICITLY measured rows. They used
// to be one long join ("Y approve  N decline  A allow 5×  F always  V inspect  Esc", 60
// cells) handed to the host terminal, which soft-wrapped it on a narrow pane. That is worse
// than ugly: the footer's height budget counts "\n"-delimited rows, so a host-created wrap
// makes the fixed bottom band physically taller than the model believes.

// approvalWidths spans the cramped end (below the core approve/decline pair) through a
// wide pane, crossing every layout boundary.
var approvalWidths = []int{16, 20, 22, 32, 40, 56, 66, 68, 80, 120}

func TestApprovalActions_NeverExceedTheirWidth(t *testing.T) {
	risks := []domain.RiskClass{
		domain.RiskTerminal, domain.RiskProject, domain.RiskExternal, // rememberable
		domain.RiskGit, domain.RiskSystem, // typed-confirm
	}
	for _, risk := range risks {
		for _, w := range approvalWidths {
			req := confirmReq("some.tool", risk, "")
			for _, tc := range []struct {
				name string
				p    *pendingConfirm
			}{
				{"single-key", &pendingConfirm{req: req}},
				{"inspecting", &pendingConfirm{req: req, showArgs: true}},
				{"typed-confirm", &pendingConfirm{req: req, requireType: true}},
			} {
				out := renderApproval(darkTheme(), tc.p, w)
				for i, line := range strings.Split(out, "\n") {
					if got := ansi.StringWidth(line); got > w {
						t.Errorf("%s %s@%d: row %d is %d cells: %q",
							risk, tc.name, w, i, got, ansi.Strip(line))
					}
				}
			}
		}
	}
}

// The width contract holds all the way down. The root floors content width at 1 but has
// only a HEIGHT floor, so a tall three-column terminal really does reach these widths —
// and a single overrun row there soft-wraps and inflates the fixed band exactly as it
// would at 80 columns.
func TestApprovalSheet_HoldsItsWidthContractAtTheFloor(t *testing.T) {
	for _, w := range []int{0, 1, 2, 3, 5, 8} {
		for _, p := range []*pendingConfirm{
			{req: confirmReq("terminal.sendInput", domain.RiskTerminal, "")},
			{req: confirmReq("terminal.sendInput", domain.RiskTerminal, ""), showArgs: true},
			{req: confirmReq("git.push", domain.RiskGit, ""), requireType: true, confirmInput: "conf"},
		} {
			for i, line := range strings.Split(renderApproval(darkTheme(), p, w), "\n") {
				if got := ansi.StringWidth(line); got > w {
					t.Errorf("@%d: row %d is %d cells: %q", w, i, got, ansi.Strip(line))
				}
			}
		}
	}
}

// Every control stays reachable at every width — a layout may move them between rows, but
// never drop one.
func TestApprovalActions_AllControlsSurviveEveryLayout(t *testing.T) {
	// Below the core pair the fallback truncates, which is the documented last resort;
	// above it, nothing may go missing.
	for _, w := range []int{22, 32, 40, 56, 66, 68, 80, 120} {
		rememberableOut := ansi.Strip(renderActionRows(darkTheme(),
			confirmReq("terminal.sendInput", domain.RiskTerminal, ""), w))
		for _, want := range []string{"Y approve", "N decline", "A allow", "F always", "V inspect", "Esc decline"} {
			if !strings.Contains(rememberableOut, want) {
				t.Errorf("@%d: rememberable layout dropped %q:\n%s", w, want, rememberableOut)
			}
		}
		plainOut := ansi.Strip(renderActionRows(darkTheme(),
			confirmReq("git.push", domain.RiskGit, ""), w))
		for _, want := range []string{"Y approve", "N decline", "V inspect", "Esc decline"} {
			if !strings.Contains(plainOut, want) {
				t.Errorf("@%d: non-rememberable layout dropped %q:\n%s", w, want, plainOut)
			}
		}
		// A non-rememberable risk must never advertise the session allow-list.
		if strings.Contains(plainOut, "A allow") || strings.Contains(plainOut, "F always") {
			t.Errorf("@%d: non-rememberable risk offered a remembered approval:\n%s", w, plainOut)
		}
	}
}

// The immediate decision leads. Whatever the width, the first row is the one that answers
// "do I let this happen" — the remembered-approval and inspect affordances move below it.
func TestApprovalActions_ApproveDeclineLeadEveryLayout(t *testing.T) {
	for _, risk := range []domain.RiskClass{domain.RiskTerminal, domain.RiskGit} {
		for _, w := range approvalWidths {
			if w < 22 {
				continue // below the core pair the documented last resort truncates; see the width test
			}
			out := ansi.Strip(renderActionRows(darkTheme(), confirmReq("some.tool", risk, ""), w))
			first := strings.Split(out, "\n")[0]
			if !strings.Contains(first, "Y approve") || !strings.Contains(first, "N decline") {
				t.Errorf("%s@%d: first row must carry the approve/decline decision, got %q", risk, w, first)
			}
			// N stays the visual default: the reverse-video run survives every layout.
			styled := renderActionRows(darkTheme(), confirmReq("some.tool", risk, ""), w)
			if !strings.Contains(styled, "\x1b[7m") && !strings.Contains(styled, ";7m") {
				t.Errorf("%s@%d: decline lost its inverse default treatment", risk, w)
			}
		}
	}
}

// Rows are chosen by measurement, not guessed: as the pane narrows the controls fold onto
// more rows instead of overflowing, and never fold further than they need to.
func TestApprovalActions_FoldOnlyAsNeeded(t *testing.T) {
	req := confirmReq("terminal.sendInput", domain.RiskTerminal, "")
	rowsAt := func(w int) int {
		return len(strings.Split(renderActionRows(darkTheme(), req, w), "\n"))
	}
	if got := rowsAt(80); got != 1 {
		t.Errorf("a wide pane must keep one row, got %d", got)
	}
	if got := rowsAt(40); got != 2 {
		t.Errorf("a 40-cell pane must fold to two rows, got %d", got)
	}
	if got := rowsAt(32); got != 3 {
		t.Errorf("a 32-cell pane must fold to three rows, got %d", got)
	}
	// Monotonic: narrowing never REDUCES the row count.
	prev := 0
	for _, w := range []int{16, 22, 32, 40, 56, 68, 120} {
		n := rowsAt(w)
		if prev != 0 && n > prev {
			t.Errorf("row count grew from %d to %d as the pane widened past %d", prev, n, w)
		}
		prev = n
	}
}

// The sheet must not trip the "terminal too small" fallback at a height its explicit rows
// genuinely fit in — the whole reason the rows are counted rather than soft-wrapped. The
// footer's own floor is lineCount(band) + 1 (the blank separator), so that exact height
// must render the real sheet, and one row less must fall back cleanly rather than overflow.
func TestApprovalSheet_FitsTheFooterBudget(t *testing.T) {
	for _, w := range []int{32, 40, 55, 72, 100} {
		pending := func() *pendingConfirm {
			return &pendingConfirm{
				req:     confirmReq("terminal.sendInput", domain.RiskTerminal, ""),
				resolve: make(chan bool, 1),
				shownAt: domain.NowMS(),
			}
		}
		probe := testModel(w)
		probe.pending = pending()
		need := lineCount(probe.bottomBand(probe.contentW())) + 1

		m := testModel(w)
		m.rows = need
		m.pending = pending()
		v := m.View()
		out := ansi.Strip(v.Content)
		assertNoOverflow(t, "approval-band@"+itoa(w), v.Content, m.usableWidth())
		assertNoHeightOverflow(t, "approval-band@"+itoa(w), v.Content, need)
		if strings.Contains(out, "terminal too small") {
			t.Errorf("%d cols x %d rows: the band fits exactly but collapsed to the fallback:\n%s", w, need, out)
		}
		if !strings.Contains(out, "Y approve") {
			t.Errorf("%d cols x %d rows: the approve control never rendered:\n%s", w, need, out)
		}

		// One row short: collapse, never overflow.
		short := testModel(w)
		short.rows = need - 1
		short.pending = pending()
		sv := short.View()
		assertNoHeightOverflow(t, "approval-short@"+itoa(w), sv.Content, need-1)
	}
}

// The fold boundaries themselves, not interior samples: an off-by-one in the layout
// chooser (rejecting an exact fit) would leave every interior assertion passing.
func TestApprovalActions_FoldBoundariesAreExact(t *testing.T) {
	rows := func(risk domain.RiskClass, w int) int {
		return len(strings.Split(renderActionRows(darkTheme(), confirmReq("x", risk, ""), w), "\n"))
	}
	cases := []struct {
		name         string
		risk         domain.RiskClass
		fits, folds  int
		want, wantAt int
	}{
		{name: "rememberable one row", risk: domain.RiskTerminal, fits: 68, folds: 67, want: 1, wantAt: 2},
		{name: "rememberable two rows", risk: domain.RiskTerminal, fits: 33, folds: 32, want: 2, wantAt: 3},
		{name: "plain one row", risk: domain.RiskGit, fits: 46, folds: 45, want: 1, wantAt: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rows(tc.risk, tc.fits); got != tc.want {
				t.Errorf("@%d (exact fit) = %d rows, want %d", tc.fits, got, tc.want)
			}
			if got := rows(tc.risk, tc.folds); got != tc.wantAt {
				t.Errorf("@%d (one cell short) = %d rows, want %d", tc.folds, got, tc.wantAt)
			}
		})
	}
	// Typed confirmation stacks its own action row at the same kind of boundary.
	typedRows := func(w int) int {
		p := &pendingConfirm{req: confirmReq("git.push", domain.RiskGit, ""), requireType: true}
		return len(strings.Split(renderTypedConfirm(darkTheme(), p, w), "\n"))
	}
	if a, b := typedRows(28), typedRows(27); a != 3 || b != 4 {
		t.Errorf("typed action row must stack one cell below its fit: rows@28=%d rows@27=%d", a, b)
	}
}

// The inverse run that marks DECLINE as the default must both open and close. A regression
// that kept the opener but lost the reset would bleed the highlight across the frame — and
// a plain "contains SGR 7" check would not notice.
func TestApprovalActions_DeclineInverseRunIsBalanced(t *testing.T) {
	// The typed sheet has two inverse runs (the caret and the decline default), so the
	// invariant is "at least one, and every one of them closes" — checked by walking the
	// openers and requiring a bare reset before the next one begins.
	check := func(label, styled string) {
		t.Helper()
		open := strings.Count(styled, "\x1b[7m")
		if open == 0 {
			t.Fatalf("%s: decline lost its inverse default treatment: %q", label, styled)
		}
		rest := styled
		for i := 0; i < open; i++ {
			at := strings.Index(rest, "\x1b[7m")
			rest = rest[at+len("\x1b[7m"):]
			reset := strings.Index(rest, "\x1b[m")
			next := strings.Index(rest, "\x1b[7m")
			if reset < 0 || (next >= 0 && next < reset) {
				t.Errorf("%s: inverse run %d never closed before the next one: %q", label, i, styled)
				return
			}
		}
	}
	for _, w := range approvalWidths {
		check("single-key@"+itoa(w),
			renderActionRows(darkTheme(), confirmReq("x", domain.RiskTerminal, ""), w))
		check("typed@"+itoa(w), renderTypedConfirm(darkTheme(),
			&pendingConfirm{req: confirmReq("git.push", domain.RiskGit, ""), requireType: true}, w))
	}
}

// Guard the fixture itself: if confirmReq ever stops producing a rememberable risk, the
// layout tests above would silently only exercise the short candidate set.
func TestApprovalWidth_FixturePreconditions(t *testing.T) {
	if !rememberable(domain.RiskTerminal) {
		t.Fatal("precondition: terminal risk must be rememberable for these layouts to be exercised")
	}
	if rememberable(domain.RiskGit) {
		t.Fatal("precondition: git risk must NOT be rememberable")
	}
	var _ tools.ConfirmRequest = confirmReq("some.tool", domain.RiskTerminal, "")
}
