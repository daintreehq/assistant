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
