package ui

import (
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// liveStatusLabel returns the LiveRunStatus label for a phase, or "" when the
// phase is self-evident (the streaming prose / activity tree already explain it).
// Only the "silent work" gaps get a label, driven by the
// explicit RunPhase — NEVER inferred from "is assistantText empty".
func liveStatusLabel(p domain.RunPhase) string {
	switch p {
	case domain.PhaseAnalyzing:
		return "Analyzing request"
	case domain.PhaseGenerating:
		// Prose streams LIVE in the footer token by token, then settles into scrollback line by
		// line (paragraph by paragraph for a markdown tail — render_turn.go). The spinner carries
		// the SILENT gaps — when the model is composing the next line or thinking with no tokens
		// flowing — so the footer never looks frozen between bursts of streamed prose.
		return "Writing"
	case domain.PhaseIntegrating:
		return "Integrating results"
	case domain.PhaseAwaitingApproval:
		return "Waiting for approval"
	case domain.PhaseAwaitingQuestion:
		return "Waiting for your answer"
	case domain.PhaseCancelling:
		return "Cancelling"
	default:
		// tool_running (activity tree), received (inline on the marker), and the terminal
		// phases carry no separate live line.
		return ""
	}
}

// runStageLabel is the composer's busy cue label. It maps the same silent-work
// phases plus a generating fallback, never "Thinking"/"Generating"-during-tools.
// The generic fallback is "Processing…".
func runStageLabel(p domain.RunPhase) string {
	switch p {
	case domain.PhaseReceived:
		return "Received"
	case domain.PhaseAnalyzing:
		return "Analyzing request…"
	case domain.PhaseIntegrating:
		return "Integrating results…"
	case domain.PhaseAwaitingApproval:
		return "Waiting for approval…"
	case domain.PhaseAwaitingQuestion:
		return "Waiting for your answer…"
	case domain.PhaseToolRunning:
		return "Inspecting project…"
	case domain.PhaseCancelling:
		return "Cancelling…"
	case domain.PhaseGenerating:
		// Match the transcript's LiveRunStatus verb ("Writing") so the composer cue and
		// the live status agree. Returning "" here let the cue fall through to the generic
		// "Processing…" fallback, which disagreed with the "Writing" label above the turn.
		return "Writing…"
	default:
		return "Processing…"
	}
}

// formatDuration renders a millisecond duration:
//
//	ms < 0       → ""
//	ms < 1000    → "<round(ms)>ms"
//	secs < 60    → "<secs>s"   (secs = round(ms/1000))
//	otherwise    → "MM:SS"
func formatDuration(ms int64) string {
	if ms < 0 {
		return ""
	}
	if ms < 1000 {
		return fmt.Sprintf("%dms", ms)
	}
	secs := (ms + 500) / 1000 // round to nearest second
	if secs < 60 {
		return fmt.Sprintf("%ds", secs)
	}
	mins := secs / 60
	rem := secs % 60
	return fmt.Sprintf("%02d:%02d", mins, rem)
}

// formatCost renders a USD cost: <=0 → "$0.000",
// < 0.01 → 4dp, < 1 → 3dp, else 2dp.
func formatCost(usd float64) string {
	switch {
	case usd <= 0:
		return "$0.000"
	case usd < 0.01:
		return fmt.Sprintf("$%.4f", usd)
	case usd < 1:
		return fmt.Sprintf("$%.3f", usd)
	default:
		return fmt.Sprintf("$%.2f", usd)
	}
}

// elapsedToken returns " · <dur>" for a phase that started PhaseStartedAt ms ago,
// shown only once elapsed reaches 300ms (avoids a 0ms flicker). "" otherwise.
func elapsedToken(phaseStartedAt, now int64) string {
	if phaseStartedAt <= 0 {
		return ""
	}
	d := now - phaseStartedAt
	if d < 300 {
		return ""
	}
	return " · " + formatDuration(d)
}
