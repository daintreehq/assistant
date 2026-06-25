package agent

import (
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/models"
)

// goalAnchorMaxRunes bounds the originating-ask text copied into the goal anchor.
// The ask can be a large pasted block; the footer only needs enough to re-anchor
// the model on what it set out to do, not the whole payload. Rune-bounded (not
// byte-bounded) so a multibyte ask is never split mid-character.
const goalAnchorMaxRunes = 500

// footerSection renders one section of the turn footer from the turn's originating
// goal. It returns ("", false) to omit the section entirely (e.g. an empty goal).
//
// This is the forward-compatibility seam: later waves (memory recall, a
// current-task block, …) register additional sections in footerSections WITHOUT
// re-touching the Router.Stream call in session.go. The signature stays goal-only
// until a concrete section needs more — broaden it here, in one file, when that
// section actually lands, rather than plumbing speculative context now.
type footerSection func(goal string) (string, bool)

// footerSections is the ordered registry of turn-footer sections. Composed in
// declaration order into a single trailing system message. Package-local and
// mutable so tests can swap it (save/restore via t.Cleanup); production registers
// statically here and never mutates it at runtime.
var footerSections = []footerSection{goalAnchorSection}

// composeTurnFooter builds the UNCACHED tail of the model request: zero or one
// system-role message appended AFTER the history snapshot in the Router.Stream
// call. Because it sits at the tail (never in the leading prefix), it is never
// part of the Fireworks prefix cache and is rebuilt fresh every round — editing
// turn-varying facts here can never invalidate the cached prefix. The result is
// ephemeral: it is appended only onto the snapshot slice handed to Stream and is
// NEVER pushed into s.messages, so durable history and token estimates are
// unaffected.
//
// Sections are joined with a blank line into ONE message (simpler than one
// message per section); a future section that genuinely needs its own message can
// be handled when it arrives. Returns nil when no section emits anything, so the
// caller's append is a no-op and the request is byte-identical to the pre-footer
// behaviour.
func composeTurnFooter(goal string) []models.ChatMessage {
	goal = strings.TrimSpace(goal)

	var parts []string
	for _, section := range footerSections {
		body, ok := section(goal)
		if !ok {
			continue
		}
		if body = strings.TrimSpace(body); body == "" {
			continue
		}
		parts = append(parts, body)
	}
	if len(parts) == 0 {
		return nil
	}
	return []models.ChatMessage{models.TextMessage("system", strings.Join(parts, "\n\n"))}
}

// goalAnchorSection emits the `# Current goal` anchor: the turn's originating ask
// (truncated) plus a terse output-discipline line. Seeding the goal at the tail on
// every round counteracts goal drift in long, many-round turns without rewriting
// any cached early control message. Omitted entirely when the goal is blank.
//
// The anchor stays pinned to the ORIGINATING ask for the whole turn: a mid-turn
// redirect (InjectPrompt → foldInInjections) lands as a fresh user message in
// history, which the model weighs over this trailing system reminder — so the
// anchor intentionally does NOT chase injections (it would otherwise thrash the
// footer and lose the turn's through-line). The known asymmetry is acceptable
// because a recent user message outranks trailing system boilerplate.
func goalAnchorSection(goal string) (string, bool) {
	if goal == "" {
		return "", false
	}
	return "# Current goal\n" + sliceChars(goal, goalAnchorMaxRunes) +
		"\n\nStay focused on this goal. Finish it before stopping, and report what you did, not what remains.", true
}
