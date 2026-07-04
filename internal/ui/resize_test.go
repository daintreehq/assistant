package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// resize_test.go locks the re-activated nuclear redraw on terminal resize: the first size
// just records geometry, a later resize schedules a debounced redraw, and onRedraw wipes +
// re-commits the masthead/transcript fresh at the new width (so no stale rule fragments are
// stranded). A stale-nonce redraw (superseded by a newer resize) is ignored.

func TestResize_FirstSizeRecordsGeometryNoRedraw(t *testing.T) {
	m := harnessModel()
	next, cmd := step(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	mm := asModel(t, next)
	if !mm.sizedOnce {
		t.Fatal("first WindowSizeMsg did not flip sizedOnce")
	}
	if cmd != nil {
		t.Fatal("the FIRST size must not trigger a redraw (nothing committed yet)")
	}
	if mm.columns != 90 || mm.rows != 30 {
		t.Fatalf("geometry not recorded: %dx%d", mm.columns, mm.rows)
	}
}

// The pre-program hand-off frame is real committed content painted at specific
// dims (run.go). When BT's startup size probe agrees with those dims the first
// WindowSizeMsg stays a pure geometry record; when it DISAGREES, the terminal
// was resized during the splash (an embedded host hydrating layout mid-boot)
// and the stale frame must get the same nuclear redraw a later resize gets —
// swallowing it strands a frozen mis-wrapped cockpit above the live footer.
func TestResize_FirstSizeMatchingHandoffDimsIsSwallowed(t *testing.T) {
	m := harnessModel()
	m.handoffCols = 90
	m.handoffRows = 30
	next, cmd := step(t, m, tea.WindowSizeMsg{Width: 90, Height: 30})
	mm := asModel(t, next)
	if !mm.sizedOnce {
		t.Fatal("first WindowSizeMsg did not flip sizedOnce")
	}
	if cmd != nil {
		t.Fatal("a first size MATCHING the hand-off dims must not redraw (the painted frame is correct)")
	}
}

func TestResize_FirstSizeMismatchingHandoffDimsSchedulesRedraw(t *testing.T) {
	m := harnessModel()
	m.handoffCols = 90
	m.handoffRows = 30
	m.commitArmed = true
	next, cmd := step(t, m, tea.WindowSizeMsg{Width: 74, Height: 30})
	mm := asModel(t, next)
	if !mm.sizedOnce {
		t.Fatal("first WindowSizeMsg did not flip sizedOnce")
	}
	if cmd == nil {
		t.Fatal("a first size that DISAGREES with the hand-off dims must schedule the nuclear redraw")
	}
	if mm.commitArmed {
		t.Fatal("the mismatch path must disarm commits for the debounce window like any real resize")
	}
	if mm.resizePending == 0 {
		t.Fatal("the mismatch path must arm a resize nonce so the debounced RedrawMsg is accepted")
	}

	// Drive the scheduled redraw: it must perform the full nuclear sequence.
	beforeNonce := mm.redrawNonce
	next2, _ := step(t, mm, RedrawMsg{Nonce: mm.resizePending})
	mm2 := asModel(t, next2)
	if mm2.redrawNonce == beforeNonce {
		t.Fatal("the hand-off-mismatch redraw must bump redrawNonce so the masthead re-commits")
	}
}

func TestResize_LaterResizeSchedulesNuclearRedraw(t *testing.T) {
	m := bootToSteadyState(t) // consumes the first size (sizedOnce=true), masthead committed
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, tea.WindowSizeMsg{Width: 60, Height: 24})
	mm := asModel(t, next)
	if mm.columns != 60 || mm.rows != 24 {
		t.Fatalf("resize geometry not recorded: %dx%d", mm.columns, mm.rows)
	}
	if cmd == nil {
		t.Fatal("a later resize must schedule a debounced nuclear redraw (RedrawMsg)")
	}

	// Fire the redraw with the matching nonce → nuclear redraw: disarm commits + reset queue.
	next2, _ := step(t, mm, RedrawMsg{Nonce: mm.resizePending})
	mm2 := asModel(t, next2)
	if mm2.commitArmed {
		t.Fatal("onRedraw must disarm commits (they re-arm one render cycle out)")
	}
	if mm2.redrawNonce == beforeNonce {
		t.Fatal("onRedraw must bump redrawNonce so the commit queue re-commits from scratch")
	}
}

// A CommitArmMsg that fires INSIDE a resize's disarm window (Init's 60ms arm
// tick racing the 150ms redraw debounce) must not re-arm commits: a commit
// landing before the nuclear redraw would Println against pre-redraw geometry
// (#1613). The pending redraw's own sequence carries the arm that matters.
func TestCommitArm_DroppedWhileRedrawPending(t *testing.T) {
	m := harnessModel()
	m.handoffCols = 90
	m.handoffRows = 30
	// First size mismatches the hand-off dims → disarm + debounced redraw.
	m, cmd := step(t, m, tea.WindowSizeMsg{Width: 74, Height: 30})
	if cmd == nil {
		t.Fatal("mismatch must schedule the debounced redraw")
	}
	// The Init-scheduled arm tick lands mid-window: it must be dropped.
	m, _ = step(t, m, CommitArmMsg{})
	if m.commitArmed {
		t.Fatal("CommitArmMsg inside the redraw disarm window must not re-arm commits")
	}
	// The redraw runs, then ITS arm tick re-arms.
	m, _ = step(t, m, RedrawMsg{Nonce: m.resizePending})
	m, _ = step(t, m, CommitArmMsg{})
	if !m.commitArmed {
		t.Fatal("the redraw's own CommitArmMsg must re-arm commits after the wipe")
	}
}

// Ctrl+L is the manual recovery key: it must run the FULL nuclear redraw (host
// screen + scrollback wipe, masthead + transcript re-committed), not a bare
// footer repaint — a bare tea.ClearScreen cannot remove corrupted rows above
// the inline origin or in native scrollback (a hand-off painted at stale dims).
func TestCtrlL_PerformsNuclearRedraw(t *testing.T) {
	m := bootToSteadyState(t)
	beforeNonce := m.redrawNonce

	next, cmd := step(t, m, tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	mm := asModel(t, next)
	if cmd == nil {
		t.Fatal("Ctrl+L must return the redraw command sequence")
	}
	if mm.commitArmed {
		t.Fatal("Ctrl+L must disarm commits (they re-arm one render cycle out)")
	}
	if mm.redrawNonce == beforeNonce {
		t.Fatal("Ctrl+L must bump redrawNonce so the masthead + transcript re-commit from scratch")
	}
	if mm.queue.headerDone {
		t.Fatal("Ctrl+L must reset the commit queue so the masthead re-commits")
	}
}

func TestResize_StaleNonceRedrawIgnored(t *testing.T) {
	m := bootToSteadyState(t)
	// Two resizes in quick succession: only the latest nonce should redraw.
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 70, Height: 28})
	m, _ = step(t, m, tea.WindowSizeMsg{Width: 50, Height: 20})
	latest := m.resizePending
	afterNonce := m.redrawNonce

	// The EARLIER redraw (stale nonce) must be a no-op.
	next, _ := step(t, m, RedrawMsg{Nonce: latest - 1})
	mm := asModel(t, next)
	if mm.redrawNonce != afterNonce {
		t.Fatal("a stale-nonce redraw (superseded by a newer resize) must be ignored")
	}
}
