package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// composer_state_test.go covers the root half of the composer state-truth pass: the
// placeholder describes the field's CURRENT purpose (start a request vs steer the running
// turn), and the Escape hint the cockpit actually renders matches the next Escape press.

func TestComposerPlaceholder_DescribesIdleAndBusyPurpose(t *testing.T) {
	m := harnessModel()
	m.syncComposer()
	idle := ansi.Strip(m.footer())
	if !strings.Contains(idle, "Ask Daintree…") {
		t.Errorf("idle composer must invite a request: %q", idle)
	}
	if strings.Contains(idle, "Add a follow-up") {
		t.Errorf("idle composer must not offer a follow-up: %q", idle)
	}
	// The slash cue lives in the hint row, not doubled inside the input.
	if strings.Contains(idle, "/ for commands") {
		t.Errorf("placeholder must not duplicate the slash hint: %q", idle)
	}
	if !strings.Contains(idle, "/ commands") {
		t.Errorf("hint row still carries the slash cue: %q", idle)
	}

	// A turn in flight repurposes the same field: submitted text folds into THAT turn.
	m.inFlight = true
	m.syncComposer()
	busy := ansi.Strip(m.footer())
	if !strings.Contains(busy, "Add a follow-up…") {
		t.Errorf("active-turn composer must describe the follow-up purpose: %q", busy)
	}
	if strings.Contains(busy, "Ask Daintree…") {
		t.Errorf("active-turn composer must not still read as a fresh request box: %q", busy)
	}
}

// The cockpit-level wiring of the Escape hint: the composer derives the label, but only
// the root knows the turn is in flight and how many follow-ups the Session still holds.
func TestComposerEscapeHint_ReflectsCockpitState(t *testing.T) {
	cases := []struct {
		name       string
		inFlight   bool
		queue      int
		want       string
		wantAbsent []string
	}{
		{name: "idle", wantAbsent: []string{"Esc clear draft", "Esc cancel turn", "Esc edit"}},
		{name: "busy", inFlight: true, want: "Esc cancel turn", wantAbsent: []string{"Esc edit"}},
		{name: "busy one queued", inFlight: true, queue: 1, want: "Esc edit follow-up",
			wantAbsent: []string{"Esc cancel turn"}},
		{name: "busy two queued", inFlight: true, queue: 2, want: "Esc edit latest",
			wantAbsent: []string{"Esc cancel turn"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := harnessModel()
			m.inFlight = tc.inFlight
			m.pendingInject = tc.queue
			m.syncComposer()
			out := ansi.Strip(m.footer())
			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Errorf("hint %q missing:\n%s", tc.want, out)
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(out, absent) {
					t.Errorf("competing hint %q present:\n%s", absent, out)
				}
			}
		})
	}
}

// The buffered follow-ups must be visible AS follow-ups belonging to the running turn —
// the count alone reads as a separate queue of future requests.
func TestComposerQueueCue_NamesTheFollowupAndItsTurn(t *testing.T) {
	m := harnessModel()
	m.inFlight = true
	m.pendingInject = 1
	m.syncComposer()
	one := ansi.Strip(m.footer())
	if !strings.Contains(one, "1 follow-up queued for this turn") {
		t.Errorf("singular queue cue missing:\n%s", one)
	}
	// Escape is described ONCE, by the hint row — the cue is informational.
	if !strings.Contains(one, "Esc edit follow-up") {
		t.Errorf("hint row must own the retract copy:\n%s", one)
	}

	m.pendingInject = 2
	m.syncComposer()
	two := ansi.Strip(m.footer())
	if !strings.Contains(two, "2 follow-ups queued for this turn") {
		t.Errorf("plural queue cue missing:\n%s", two)
	}
	if !strings.Contains(two, "Esc edit latest") {
		t.Errorf("hint row must own the retract copy:\n%s", two)
	}

	// Nothing buffered → no cue at all.
	m.pendingInject = 0
	m.syncComposer()
	if none := ansi.Strip(m.footer()); strings.Contains(none, "queued") {
		t.Errorf("queue cue must vanish when nothing is buffered:\n%s", none)
	}
}
