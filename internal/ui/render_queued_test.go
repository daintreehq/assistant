package ui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/daintreehq/assistant/internal/domain"
)

// render_queued_test.go covers the footer card that shows messages typed while the model is
// working — buffered by the Session, not yet folded into the running turn.

// The reported gap: submitting mid-stream only bumped a count under the input, so the user
// had to trust their message had landed. The queued TEXT itself must be on screen.
func TestQueuedCard_ShowsTheQueuedTextItself(t *testing.T) {
	m := harnessModel()
	m.inFlight = true
	m.pendingInjects = []string{"Please close all the terminals"}
	m.syncComposer()
	out := ansi.Strip(m.footer())
	if !strings.Contains(out, "Please close all the terminals") {
		t.Errorf("the queued message text must be visible in the footer:\n%s", out)
	}
	if !strings.Contains(out, "1 follow-up queued for this turn") {
		t.Errorf("the queued card must anchor on the cue naming the turn it belongs to:\n%s", out)
	}
	// It is a PENDING cue, not transcript: nothing was committed, and the card must sit
	// above the composer where the user just typed.
	iCard := strings.Index(out, "Please close all the terminals")
	iInput := strings.Index(out, "Add a follow-up")
	if iCard < 0 || iInput < 0 || iCard > iInput {
		t.Errorf("the queued card must render ABOVE the composer (card=%d input=%d):\n%s", iCard, iInput, out)
	}
}

// The card clears the instant the Session folds the message in — from there the transcript's
// own mid-turn card owns it, and two copies on screen would read as two messages.
func TestQueuedCard_ClearsOnDelivery(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{ID: "turn_q", State: TurnActive}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true
	m.pendingInjects = []string{"first", "second"}

	m.applyPumpEvent(pumpEvent{kind: pumpInterject, text: "first"})
	// The Session delivers in send order, so the head is what left the queue.
	if len(m.pendingInjects) != 1 || m.pendingInjects[0] != "second" {
		t.Fatalf("pendingInjects = %+v after delivering the first, want [second]", m.pendingInjects)
	}
	m.syncComposer()
	out := ansi.Strip(m.footer())
	if strings.Contains(out, "1 follow-up queued") == false {
		t.Errorf("the card must still show the message still waiting:\n%s", out)
	}
	if !strings.Contains(out, "second") {
		t.Errorf("the still-queued message must remain visible:\n%s", out)
	}

	m.applyPumpEvent(pumpEvent{kind: pumpInterject, text: "second"})
	if len(m.pendingInjects) != 0 {
		t.Fatalf("pendingInjects = %+v after delivering both, want empty", m.pendingInjects)
	}
	m.syncComposer()
	if out := ansi.Strip(m.footer()); strings.Contains(out, "queued") {
		t.Errorf("the queued card must vanish once everything is delivered:\n%s", out)
	}
}

// The card rides the FIXED bottom band, which is never truncated — so its height is a
// contract, not a preference. However many messages stack up, it stays at the bound.
func TestQueuedCard_HeightIsBounded(t *testing.T) {
	th := darkTheme()
	for _, n := range []int{1, 2, 3, 4, 12, 50} {
		var texts []string
		for i := 0; i < n; i++ {
			texts = append(texts, "follow-up message number that is fairly long")
		}
		rows := strings.Split(renderQueuedInjections(th, texts, 72, 99), "\n")
		if want := 1 + queuedPreviewMax; len(rows) > want {
			t.Errorf("%d queued: card is %d rows, must never exceed %d", n, len(rows), want)
		}
		if n > queuedPreviewMax {
			out := stripAnsi(strings.Join(rows, "\n"))
			// The overflow count must account for every message the preview does not show.
			hidden := n - (queuedPreviewMax - 1)
			if !strings.Contains(out, "+"+itoa(hidden)+" more") {
				t.Errorf("%d queued: overflow row must read \"+%d more\":\n%s", n, hidden, out)
			}
		}
	}
}

// A queued paste costs ONE row, not its full height: the fixed band has no room to grow,
// and a 200-line log pasted mid-turn would otherwise push the composer off screen.
func TestQueuedCard_MultilinePasteCollapsesToOneRow(t *testing.T) {
	th := darkTheme()
	paste := "here is the log:\npanic: runtime error\n\tgoroutine 1 [running]\n\tmain.main()"
	rows := strings.Split(renderQueuedInjections(th, []string{paste}, 72, 99), "\n")
	if len(rows) != 2 {
		t.Fatalf("a multi-line paste must render as anchor + one row, got %d:\n%s", len(rows), stripAnsi(strings.Join(rows, "\n")))
	}
	body := stripAnsi(rows[1])
	if !strings.Contains(body, "here is the log: panic: runtime error") {
		t.Errorf("the flattened row must keep the message's leading text: %q", body)
	}
}

func TestQueuedCard_EmptyRendersNothing(t *testing.T) {
	if got := renderQueuedInjections(darkTheme(), nil, 72, 99); got != "" {
		t.Errorf("nothing queued must render nothing, got %q", got)
	}
}

// Interjection events cross a goroutine boundary through the pump, so one can still be in
// flight when the mirror is force-cleared (a retract the Session can no longer honour, a
// cancel, a /clear) and a NEW message is typed on top. Acknowledging by position would then
// retire the newcomer — taking its card AND its Escape hint off screen while the Session
// still holds it, so the next Esc cancels the turn instead of retracting it.
func TestQueuedCard_DelayedDeliveryDoesNotRetireANewerMessage(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{ID: "turn_race", State: TurnActive}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true

	// "A" was submitted and the Session already drained it; its event has not been reduced.
	// Esc finds nothing retractable and clears the stale mirror, then "B" is submitted.
	m.pendingInjects = nil
	m.pendingInjects = append(m.pendingInjects, "B, typed after the clear")

	// The delayed event for A finally lands.
	m.applyPumpEvent(pumpEvent{kind: pumpInterject, text: "A, already delivered"})

	if len(m.pendingInjects) != 1 || m.pendingInjects[0] != "B, typed after the clear" {
		t.Fatalf("a delayed delivery retired an unrelated queued message: %+v", m.pendingInjects)
	}
	m.syncComposer()
	out := ansi.Strip(m.footer())
	if !strings.Contains(out, "B, typed after the clear") {
		t.Errorf("the still-queued message must stay on screen:\n%s", out)
	}
	if !strings.Contains(out, "Esc edit follow-up") {
		t.Errorf("Esc must still offer the retract, not a turn cancel:\n%s", out)
	}
}

// Two identical follow-ups are legal and must retire ONE at a time.
func TestQueuedCard_DuplicateTextsRetireOneAtATime(t *testing.T) {
	m := harnessModel()
	m.inFlight = true
	m.pendingInjects = []string{"retry it", "retry it"}
	m.applyPumpEvent(pumpEvent{kind: pumpInterject, text: "retry it"})
	if len(m.pendingInjects) != 1 {
		t.Fatalf("a duplicate delivery must retire exactly one entry: %+v", m.pendingInjects)
	}
}

// The card's height is a HARD constraint, not a preference: the fixed band is never
// truncated, and a band that exactly fills the viewport leaves no row for a scrollback
// insert (bubbletea#1613). Out of room it degrades to its anchor, then vanishes.
func TestQueuedCard_DegradesOnAShortTerminal(t *testing.T) {
	th := darkTheme()
	texts := []string{"alpha", "beta", "gamma", "delta"}
	for maxRows, wantRows := range map[int]int{0: 0, 1: 1, 2: 2, 3: 3, 4: 4, 9: 4} {
		out := renderQueuedInjections(th, texts, 72, maxRows)
		got := 0
		if out != "" {
			got = len(strings.Split(out, "\n"))
		}
		if got != wantRows {
			t.Errorf("maxRows=%d: card is %d rows, want %d:\n%s", maxRows, got, wantRows, stripAnsi(out))
		}
		// Whatever survives, the anchor does — a preview with no count would not say what it is.
		if got > 0 && !strings.Contains(stripAnsi(out), "queued") {
			t.Errorf("maxRows=%d: the anchor must be the last row to go:\n%s", maxRows, stripAnsi(out))
		}
	}
}

// The whole point of the height cap: the fixed bottom band must always leave a row spare
// for the live region and a scrollback insert, however many follow-ups are stacked up.
func TestQueuedCard_NeverFillsTheViewport(t *testing.T) {
	for _, rows := range []int{8, 9, 10, 11, 12, 14, 20, 40} {
		m := harnessModel()
		m.rows = rows
		m.inFlight = true
		m.pendingInjects = []string{"alpha", "beta", "gamma", "delta"}
		m.syncComposer()
		band := lineCount(m.bottomBand(m.contentW()))
		// bandFits' own floor plus one row kept free for the tea.Println insert.
		if band+2 > rows {
			t.Errorf("rows=%d: the fixed band is %d rows — no room left for an insertAbove", rows, band)
		}
	}
}

// A message submitted between the turn's last fold boundary and inFlight going false stays
// buffered for the NEXT turn. With nothing running, a card reading "queued for this turn"
// would name a turn that already ended — so the card is gated on a live turn, and comes
// back when the next one starts.
func TestQueuedCard_HiddenWhenNoTurnIsRunning(t *testing.T) {
	m := harnessModel()
	m.inFlight = false
	m.pendingInjects = []string{"left over from the last turn"}
	m.syncComposer()
	if out := ansi.Strip(m.footer()); strings.Contains(out, "queued") || strings.Contains(out, "left over") {
		t.Errorf("the queued card must not claim a turn that is not running:\n%s", out)
	}

	m.inFlight = true
	m.syncComposer()
	if out := ansi.Strip(m.footer()); !strings.Contains(out, "left over from the last turn") {
		t.Errorf("the still-buffered message must reappear once a turn is running:\n%s", out)
	}
}

// A turn already tearing down counts as over. The composer deliberately says "Enter send"
// during PhaseCancelling because the text belongs to the NEXT turn — a card claiming it is
// "queued for this turn" would contradict the input box one row below it.
func TestQueuedCard_HiddenWhileTheTurnIsCancelling(t *testing.T) {
	m := harnessModel()
	active := &TurnCell{ID: "turn_x", State: TurnActive, Phase: domain.PhaseCancelling, PhaseStartedAt: domain.NowMS()}
	m.transcript = append(m.transcript, TranscriptCell{Turn: active})
	m.activeTurn = active.ID
	m.inFlight = true
	m.pendingInjects = []string{"and then run the tests"}
	m.syncComposer()
	if out := ansi.Strip(m.footer()); strings.Contains(out, "queued for this turn") {
		t.Errorf("the card must not claim a turn that is being cancelled:\n%s", out)
	}
}
