package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
)

// liveness2_test.go covers the T7/T8 liveness fixes: cumulative turn elapsed, stall
// detection, spinner gating, and the truthful attention badge.

func TestLiveStatus_ElapsedIsCumulativeNotPerPhase(t *testing.T) {
	now := int64(100000)
	// StartedAt is 3s ago but the phase just changed (PhaseStartedAt == now): the elapsed must
	// reflect the whole turn (~3s), not reset to 0 on the transition.
	tc := &TurnCell{State: TurnActive, Phase: domain.PhaseAnalyzing, StartedAt: now - 3000, PhaseStartedAt: now, LastActivityAt: now}
	out := ansi.Strip(renderLiveStatus(darkTheme(), tc, 0, now))
	if !strings.Contains(out, "3s") {
		t.Fatalf("live elapsed is not cumulative over the turn (want ~3s): %q", out)
	}
}

func TestLiveStatus_StalledShowsStillWorking(t *testing.T) {
	now := int64(100000)
	tc := &TurnCell{State: TurnActive, Phase: domain.PhaseAnalyzing, StartedAt: now - 8000, LastActivityAt: now - (stallThresholdMs + 1000)}
	out := ansi.Strip(renderLiveStatus(darkTheme(), tc, 0, now))
	if !strings.Contains(out, "still working") {
		t.Fatalf("a stalled turn did not surface 'still working': %q", out)
	}
}

func TestLiveStatus_FreshDoesNotShowStall(t *testing.T) {
	now := int64(100000)
	tc := &TurnCell{State: TurnActive, Phase: domain.PhaseAnalyzing, StartedAt: now - 2000, LastActivityAt: now - 200}
	out := ansi.Strip(renderLiveStatus(darkTheme(), tc, 0, now))
	if strings.Contains(out, "still working") {
		t.Fatalf("a fresh turn wrongly showed the stall cue: %q", out)
	}
	if !strings.Contains(out, "Analyzing request") {
		t.Fatalf("missing the analyzing label: %q", out)
	}
}

func TestSpinner_GatedOnInFlight(t *testing.T) {
	m := harnessModel()
	if cmd := m.ensureSpinnerForState(); cmd != nil {
		t.Fatal("spinner armed while idle")
	}
	m.inFlight = true
	if cmd := m.ensureSpinnerForState(); cmd == nil {
		t.Fatal("spinner not armed for an in-flight turn")
	}
	if !m.spinnerRunning {
		t.Fatal("spinnerRunning not set after arming")
	}
	if cmd := m.ensureSpinnerForState(); cmd != nil {
		t.Fatal("spinner double-armed while already running")
	}
}

func TestSpinner_TickStopsWhenIdle(t *testing.T) {
	m := harnessModel()
	m.inFlight = false
	m.spinnerRunning = true
	next, cmd := m.Update(spinnerTickMsg{})
	mm := asModel(t, next)
	if cmd != nil {
		t.Fatal("spinner kept ticking while idle (should lapse)")
	}
	if mm.spinnerRunning {
		t.Fatal("spinnerRunning not cleared when idle")
	}
}

func TestAttention_BadgeRecomputedFromSnapshot(t *testing.T) {
	m := harnessModel()
	m.attentionN = 5 // stale-high from earlier events
	snap := Dashboard{Inbox: []domain.QueueEvent{{Title: "a"}, {Title: "b"}}}
	next, _ := m.Update(DashboardSnapshotMsg{Snapshot: snap})
	mm := asModel(t, next)
	if mm.attentionN != 2 {
		t.Fatalf("attention badge not recomputed from the snapshot (decrement-on-resolve); got %d want 2", mm.attentionN)
	}
}

func TestAttention_OnAttentionBumpsOptimistically(t *testing.T) {
	m := harnessModel()
	m.inFlight = true // avoid draining a wake reactor (no app in the harness)
	m.attentionN = 1
	next, _ := m.onAttention(AttentionBatchMsg{Events: []domain.QueueEvent{{Title: "x"}, {Title: "y"}}})
	mm := asModel(t, next)
	if mm.attentionN != 3 {
		t.Fatalf("onAttention should optimistically bump by len(events); got %d want 3", mm.attentionN)
	}
}
