package composer

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// hints_test.go pins the composer's state-truth copy: the Escape hint must describe what
// the NEXT Escape press actually does, and the buffered-follow-up cue must say what is
// queued and where it lands. The hint row used to promote a flat "Esc cancel" for the
// whole of a running turn, which was wrong in two of the three busy states.

// escapeHintFixture builds a composer in the requested state. Both SetBusy and
// ViewParams.Cancellable are driven from the same flag, mirroring the cockpit (which
// feeds both from m.inFlight).
func escapeHintFixture(busy bool, draft string, queue int) (Model, ViewParams) {
	m := newModel()
	m.SetBusy(busy)
	if draft != "" {
		m.insert(draft)
	}
	cancellable := busy
	return m, ViewParams{Width: 80, QueueDepth: queue, Cancellable: &cancellable}
}

func TestComposerHints_EscapeMatchesNextAction(t *testing.T) {
	cases := []struct {
		name       string
		busy       bool
		draft      string
		queue      int
		wantMode   EscapeHintMode
		want       []string
		wantAbsent []string
	}{
		{
			// Escape is a no-op here, so advertising it would promise nothing.
			name: "idle empty", wantMode: EscapeHintHidden,
			wantAbsent: []string{"Esc clear draft", "Esc cancel turn", "Esc edit follow-up", "Esc edit latest"},
		},
		{
			name: "idle draft", draft: "hello", wantMode: EscapeHintClearDraft,
			want:       []string{"Esc clear draft"},
			wantAbsent: []string{"Esc cancel turn", "Esc edit follow-up", "Esc edit latest"},
		},
		{
			name: "busy empty no queue", busy: true, wantMode: EscapeHintCancelTurn,
			want:       []string{"Esc cancel turn"},
			wantAbsent: []string{"Esc clear draft", "Esc edit follow-up", "Esc edit latest"},
		},
		{
			name: "busy draft no queue", busy: true, draft: "hello", wantMode: EscapeHintClearDraft,
			want:       []string{"Esc clear draft"},
			wantAbsent: []string{"Esc cancel turn", "Esc edit follow-up", "Esc edit latest"},
		},
		{
			name: "busy empty one queued", busy: true, queue: 1, wantMode: EscapeHintEditFollowup,
			want:       []string{"Esc edit follow-up"},
			wantAbsent: []string{"Esc cancel turn", "Esc clear draft", "Esc edit latest"},
		},
		{
			name: "busy empty several queued", busy: true, queue: 3, wantMode: EscapeHintEditLatest,
			want:       []string{"Esc edit latest"},
			wantAbsent: []string{"Esc cancel turn", "Esc clear draft", "Esc edit follow-up"},
		},
		{
			// The important interaction: a live draft means Escape clears THAT first, even
			// though follow-ups are queued behind it — handleKey checks the buffer before it
			// reports the gesture up.
			name: "busy draft while queued", busy: true, draft: "hello", queue: 2, wantMode: EscapeHintClearDraft,
			want:       []string{"Esc clear draft"},
			wantAbsent: []string{"Esc cancel turn", "Esc edit follow-up", "Esc edit latest"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, p := escapeHintFixture(tc.busy, tc.draft, tc.queue)
			if got := m.escapeHintMode(tc.busy, tc.queue); got != tc.wantMode {
				t.Fatalf("escapeHintMode = %v, want %v", got, tc.wantMode)
			}
			frame := ansi.Strip(m.View(p))
			for _, w := range tc.want {
				if !strings.Contains(frame, w) {
					t.Errorf("hint %q missing:\n%s", w, frame)
				}
			}
			for _, w := range tc.wantAbsent {
				if strings.Contains(frame, w) {
					t.Errorf("competing hint %q present:\n%s", w, frame)
				}
			}
		})
	}
}

// The state-specific Escape action is the row's highest-priority entry, so it must
// outlive generic discovery hints at every width the cockpit can render at.
func TestComposerHints_EscapeSurvivesNarrowWidths(t *testing.T) {
	cases := []struct {
		name  string
		busy  bool
		draft string
		queue int
		want  string
	}{
		{name: "cancel turn", busy: true, want: "Esc cancel turn"},
		{name: "clear draft", busy: true, draft: "hello", want: "Esc clear draft"},
		{name: "edit follow-up", busy: true, queue: 1, want: "Esc edit follow-up"},
		{name: "edit latest", busy: true, queue: 4, want: "Esc edit latest"},
	}
	for _, tc := range cases {
		for _, w := range []int{32, 40, 56, 80} {
			m, p := escapeHintFixture(tc.busy, tc.draft, tc.queue)
			p.Width = w
			frame := ansi.Strip(m.View(p))
			if !strings.Contains(frame, tc.want) {
				t.Errorf("%s@%d: %q did not survive truncation:\n%s", tc.name, w, tc.want, frame)
			}
			for _, line := range strings.Split(frame, "\n") {
				if got := ansi.StringWidth(line); got > w {
					t.Errorf("%s@%d: line %q is %d cells wide", tc.name, w, line, got)
				}
			}
		}
	}
}

// A hint that does not fit is dropped WHOLE. A row truncated mid-token reads as damage,
// and (worse) truncating the joined string could keep a generic hint while eating the
// state-specific Escape action ahead of it.
func TestComposerHints_DropWholeTokensNotMidWord(t *testing.T) {
	m, p := escapeHintFixture(true, "", 0)
	p.Width = 24 // "Esc cancel turn" (15) fits; "· / commands" (13 more) does not.
	var hint string
	for _, l := range strings.Split(ansi.Strip(m.View(p)), "\n") {
		if strings.Contains(l, "Esc") {
			hint = l
			break
		}
	}
	if !strings.Contains(hint, "Esc cancel turn") {
		t.Fatalf("Escape action must survive: %q", hint)
	}
	if strings.Contains(hint, "commands") {
		t.Errorf("a hint that does not fit must be dropped, not squeezed in: %q", hint)
	}
	if strings.Contains(hint, "…") {
		t.Errorf("hint row must drop whole hints, not ellipsise one: %q", hint)
	}
}

// Once one hint does not fit, EVERY lower-priority hint behind it is dropped too — the
// row is priority-ordered, so skipping ahead to a shorter hint would print a less
// important key in place of a more important one. Width 27 is the discriminator: it is
// one cell short of "Esc cancel turn · / commands" (28) but an exact fit for
// "Esc cancel turn · ↑ history" (27), so a `continue` here would render history.
func TestComposerHints_DropTheWholeTailNotJustTheMisfit(t *testing.T) {
	m, p := escapeHintFixture(true, "", 0)
	p.Width = 27
	frame := ansi.Strip(m.View(p))
	if !strings.Contains(frame, "Esc cancel turn") {
		t.Fatalf("Escape action must survive:\n%s", frame)
	}
	for _, gone := range []string{"/ commands", "history", "inspect ops"} {
		if strings.Contains(frame, gone) {
			t.Errorf("%q must be dropped with the rest of the tail:\n%s", gone, frame)
		}
	}
}

// Below the lead hint's own width there is nothing to drop, so the lead is truncated
// instead of overflowing. Without this the row would soft-wrap and inflate the footer.
func TestComposerHints_LeadHintTruncatesBelowItsOwnWidth(t *testing.T) {
	for _, w := range []int{1, 2, 8, 14} {
		m, p := escapeHintFixture(true, "", 0) // lead is "Esc cancel turn" (15 cells)
		p.Width = w
		p.MCPStatus = MCPConnected
		hints := m.renderHints(p)
		if hints == "" {
			t.Errorf("width %d: hint row vanished entirely", w)
			continue
		}
		for _, line := range strings.Split(ansi.Strip(hints), "\n") {
			if got := ansi.StringWidth(line); got > w {
				t.Errorf("width %d: row %q is %d cells", w, line, got)
			}
		}
	}
}

func TestQueuedFollowupLabel(t *testing.T) {
	cases := []struct {
		name  string
		n     int
		width int
		want  string
	}{
		{name: "none", n: 0, width: 80, want: ""},
		{name: "negative", n: -1, width: 80, want: ""},
		{name: "one", n: 1, width: 80, want: "1 follow-up queued for this turn"},
		{name: "two", n: 2, width: 80, want: "2 follow-ups queued for this turn"},
		{name: "twelve", n: 12, width: 80, want: "12 follow-ups queued for this turn"},
		// Width ladder: drop the qualifier, then truncate. 33 is exactly the plural full
		// form, so a real 40-column terminal (38 content cells) still gets it whole.
		{name: "two at 33", n: 2, width: 33, want: "2 follow-ups queued for this turn"},
		{name: "two at 32", n: 2, width: 32, want: "2 follow-ups queued"},
		{name: "two at 19", n: 2, width: 19, want: "2 follow-ups queued"},
		{name: "two at 18", n: 2, width: 18, want: "2 follow-ups queu…"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := queuedFollowupLabel(tc.n, tc.width); got != tc.want {
				t.Errorf("queuedFollowupLabel(%d, %d) = %q, want %q", tc.n, tc.width, got, tc.want)
			}
		})
	}
}

// Even at a width no wording fits, the cue stays ONE explicit non-empty row: the composer
// sits in a fixed-height band whose budget counts "\n"-delimited rows, so a soft wrap
// would corrupt it — and silently dropping the cue would hide buffered work entirely.
func TestQueuedFollowupLabel_NeverExceedsWidthWrapsOrVanishes(t *testing.T) {
	for _, n := range []int{1, 2, 9, 137} {
		for _, w := range []int{1, 2, 8, 16, 24, 32, 38, 40, 56, 80} {
			got := queuedFollowupLabel(n, w)
			if got == "" {
				t.Errorf("n=%d w=%d: buffered work must never render as nothing", n, w)
			}
			if strings.Contains(got, "\n") {
				t.Errorf("n=%d w=%d: label must be one row, got %q", n, w, got)
			}
			if cells := ansi.StringWidth(got); cells > w {
				t.Errorf("n=%d w=%d: label is %d cells: %q", n, w, cells, got)
			}
		}
	}
}

// The queue cue and the hint row are on screen together, so the screen must never make
// two Escape claims at once. Codex flagged the original design, where the cue carried its
// own unconditional "Esc edits it": typing a draft on top of a queued follow-up then put
// "Esc clear draft" and "Esc edits it" two rows apart, describing one keypress.
func TestComposerHints_OnlyOneEscapeClaimOnScreen(t *testing.T) {
	cases := []struct {
		name      string
		draft     string
		searching bool
		unfocused bool
		wantClaim string
	}{
		{name: "empty buffer", wantClaim: "Esc edit follow-up"},
		{name: "draft on top of the queue", draft: "and also check the tests", wantClaim: "Esc clear draft"},
		{name: "reverse-i-search", searching: true},
		{name: "composer unfocused (approval above)", unfocused: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, p := escapeHintFixture(true, tc.draft, 1)
			m.searching = tc.searching
			m.SetFocus(!tc.unfocused)
			frame := ansi.Strip(m.View(p))
			if !tc.searching && !strings.Contains(frame, "1 follow-up queued") {
				t.Fatalf("the buffered follow-up must stay visible:\n%s", frame)
			}
			var claims []string
			for _, c := range []string{"Esc clear draft", "Esc cancel turn", "Esc edit follow-up", "Esc edit latest", "Esc edits"} {
				if strings.Contains(frame, c) {
					claims = append(claims, c)
				}
			}
			if tc.wantClaim == "" {
				if len(claims) > 0 {
					t.Errorf("Escape belongs to another surface here, but the frame claims %v:\n%s", claims, frame)
				}
				return
			}
			if len(claims) != 1 || claims[0] != tc.wantClaim {
				t.Errorf("Escape claims = %v, want exactly [%s]:\n%s", claims, tc.wantClaim, frame)
			}
		})
	}
}

// An approval sheet renders ABOVE the composer and takes every key, so the composer's
// hints would be pointing at a surface that will not receive them.
func TestComposerHints_SuppressedWhileUnfocused(t *testing.T) {
	m, p := escapeHintFixture(true, "", 0)
	p.MCPStatus = MCPConnected
	m.SetFocus(false)
	frame := ansi.Strip(m.View(p))
	for _, gone := range []string{"Esc cancel turn", "/ commands", "↑ history", "inspect ops"} {
		if strings.Contains(frame, gone) {
			t.Errorf("unfocused composer still advertises %q:\n%s", gone, frame)
		}
	}
	// The connection light is not a shortcut — it stays, and it must not be preceded by
	// a blank row left behind where the hints used to be.
	if !strings.Contains(frame, "MCP") {
		t.Errorf("the MCP light must survive an unfocused composer:\n%s", frame)
	}
	if strings.Contains(frame, "\n\n\n") {
		t.Errorf("suppressing the hints left an empty row behind:\n%q", frame)
	}
}
