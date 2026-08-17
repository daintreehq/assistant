package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// render_skill_test.go pins the ABSENCE of a skill cue in the cockpit.
//
// Backend skill selection used to render an inline "Skill loaded" / "Skills loaded"
// card folded into the running turn. It was removed, and this file exists so it is not
// reintroduced by reflex. The reasoning (see agent.Session.emitSkillLoads):
//
//   - It named only the NewlyLoaded DELTA — never what was retained across rounds,
//     dropped by the max-active cap, or paired in automatically as a domain foundation.
//     Across a multi-round turn that reads as the assistant changing its mind while
//     hiding what it changed from.
//   - Its original job was explaining the ~2s selector call that sat on the first-byte
//     path. Selection is now an in-process ~10ms classifier, so there is no wait left to
//     explain, and PhaseAnalyzing already covers the one that remains — transiently,
//     without committing a row to scrollback.
//   - The user cannot accept, reject, unload, or replace a backend skill, so the name was
//     information with no matching affordance — which is also why there is no /skills
//     command (an on-demand reveal has the same gap). Selector tuning reads the debug
//     trace, where backend.respond.meta logs the active and newly-loaded sets per round.
//
// The "skill" VOCABULARY — a visible "Skill loaded" event, the /skills name — is
// deliberately held in reserve for user-authored ASSISTANT skills, which are
// intent-driven and will want it.

// sentinelText marks the end of the pump events a test cares about. Unique, so a stray
// event of the same kind cannot masquerade as the sentinel.
const sentinelText = "sentinel-2f9c"

// drainToSentinel returns every event the pump carried BEFORE the sentinel, in order.
// The pump preserves FIFO order and its sole sender goroutine (started by newEventPump)
// drains into the buffered channel, so trailing a sentinel is what makes "nothing was
// emitted" deterministic — an emptiness check on p.pending would race that goroutine and
// pass even when an event HAD been queued.
func drainToSentinel(t *testing.T, p *eventPump) []pumpEvent {
	t.Helper()
	p.AssistantCancelled(sentinelText)
	var got []pumpEvent
	for {
		select {
		case ev := <-p.ch:
			if ev.kind == pumpCancelled && ev.text == sentinelText {
				return got
			}
			got = append(got, ev)
		case <-time.After(2 * time.Second):
			t.Fatal("sentinel never arrived — the pump stalled, so this test proves nothing")
		}
	}
}

// The pump drops SkillLoaded entirely: no pumpEvent is queued, so the reducer never sees
// one and no step can reach a turn.
func TestPump_SkillLoadedEmitsNothing(t *testing.T) {
	p := newEventPump()
	p.SkillLoaded([]string{"Supervise a fleet of agents", "Daintree orchestration foundation"})

	if got := drainToSentinel(t, p); len(got) != 0 {
		t.Fatalf("SkillLoaded queued %d pump event(s) (first kind %v); backend skill loads "+
			"must be inert in the cockpit", len(got), got[0].kind)
	}
}

// Inert, NOT emit-with-no-render — a distinct property worth its own case, because emit()
// drains the token coalescer: a forwarded skill load would flush buffered prose early and
// move the flush boundary for something that draws nothing. With a token mid-flight, the
// ONLY thing that may reach the reducer is that token flush (which the sentinel's own
// emit triggers here) — never a skill-shaped event.
func TestPump_SkillLoadedDoesNotDisturbStreamingProse(t *testing.T) {
	p := newEventPump()
	p.AssistantToken("streaming prose ")
	p.SkillLoaded([]string{"Supervise a fleet of agents"})

	for _, ev := range drainToSentinel(t, p) {
		if ev.kind != pumpTokens {
			t.Fatalf("a skill load put a %v event into a streaming turn; it must be inert", ev.kind)
		}
		if strings.Contains(ev.text, "Supervise a fleet") {
			t.Fatalf("a skill title reached the reducer as prose: %q", ev.text)
		}
	}
}

// A skill load reaching the sink mid-turn leaves the rendered turn byte-identical: no
// card, and no prose seal (which would split one paragraph into two steps and change
// spacing even if the card itself drew nothing).
func TestSkillLoad_LeavesRenderedTurnUnchanged(t *testing.T) {
	m := liveModel(80)
	cell := &TurnCell{ID: "turn_1", State: TurnActive, Phase: domain.PhaseGenerating}
	m.transcript = []TranscriptCell{{Turn: cell}}
	m.activeTurn = "turn_1"
	cell.appendProse("Checking the worktrees now.")

	render := func() string {
		return renderTurn(m.theme, m.md, cell, m.chromeW(), m.contentW(), m.expanded, m.spinnerFrame, domain.NowMS())
	}
	before, steps := render(), len(cell.Steps)

	// Drive the REAL path: emit through the pump, then REDUCE everything it carries. A
	// bare m.pump.SkillLoaded() with no applyPumpEvent would pass even against the old
	// card implementation, since a queued event mutates nothing until it is reduced.
	m.pump.SkillLoaded([]string{"Supervise a fleet of agents"})
	for _, ev := range drainToSentinel(t, m.pump) {
		m.applyPumpEvent(ev)
	}

	if after := render(); after != before {
		t.Errorf("skill load perturbed the rendered turn.\nbefore: %q\nafter:  %q", stripAnsi(before), stripAnsi(after))
	}
	if got := len(cell.Steps); got != steps {
		t.Fatalf("skill load changed the step count %d -> %d; it must not seal prose or add a step", steps, got)
	}
	// Prose that resumes after the load still merges into the SAME step — the old card
	// sealed the live prose to fold itself in chronologically, splitting one paragraph in
	// two. Nothing renders now, so nothing may seal.
	cell.appendProse(" Two are ready.")
	if got := len(cell.Steps); got != steps {
		t.Fatalf("prose after a skill load opened a new step (%d -> %d): the load still seals prose", steps, got)
	}
	for _, banned := range []string{"Skill loaded", "Skills loaded", "Supervise a fleet of agents"} {
		if strings.Contains(stripAnsi(render()), banned) {
			t.Errorf("rendered turn carries %q — the skill cue is back", banned)
		}
	}
}
