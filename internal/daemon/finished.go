package daemon

import (
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// judgeAgentFinished asks the small model the single byte-stable question
// "has this agent finished its turn?" (domain.FinishedJudgeQuestion) against the
// supplied tail/signals, reusing the existing judge plumbing (runModelJudges →
// ctx.Model.Judge → JudgeSystemPrompt) verbatim — no new prompt, no new decode, and
// the tier is already pinned to small inside runModelJudges.
//
// It is FAIL-CLOSED: an absent/low-confidence/errored answer reports finished=false
// (not done). "waiting" is a soft signal, so the bar to declare an agent finished is
// a CONFIDENT yes — never the mere fact that it parked at a prompt. Returns the raw
// answer (for evidence text + the caller's confident/transient dedupe decision) and
// the finished bool.
func judgeAgentFinished(ctx *CheckContext, rec domain.WatcherRecord, signals WatcherSignals) (domain.ModelJudgeAnswer, bool) {
	// No evidence to judge → unknown → fail-closed. (An empty tail can't prove an
	// agent finished; judging nothing risks hardening a transport hiccup into a false
	// completion.)
	if !ctx.MCP.Connected() || strings.TrimSpace(signals.Tail) == "" {
		return domain.ModelJudgeAnswer{}, false
	}
	results := runModelJudges(ctx, []string{domain.FinishedJudgeQuestion}, rec, signals)
	a, ok := results[domain.FinishedJudgeQuestion]
	confident := ok && a.Confidence >= judgeConfidenceFloor
	return a, confident && a.Matched
}

// finishJudgeCooldownMS bounds how often the finished judge runs for one terminal.
// The supervisor cadence is ~3s, so without a floor a parked explore agent whose
// tail keeps repainting (a spinner, an elapsed-time counter, an ANSI cursor redraw)
// would defeat the tail-hash dedupe and burn a small-model call every tick. The
// cooldown caps that to ~once per window regardless of tail churn; completion is
// still detected within one window. It also gates the absent-path terminal.getOutput
// read so a stable, idle-but-not-finished agent isn't re-read every tick.
const finishJudgeCooldownMS = 15_000

// finishJudgeOnCooldown reports whether the finished judge ran recently enough that
// we should skip even READING the tail this tick (used by the absent path, whose
// tail read is an MCP round-trip). LastFinishJudgeAt advances every time the judge
// actually runs, so this self-clears once the window elapses.
func finishJudgeOnCooldown(prevState *TerminalState, now int64) bool {
	return prevState != nil && prevState.LastFinishJudgeAt != 0 &&
		now-prevState.LastFinishJudgeAt < finishJudgeCooldownMS
}

// confirmExploreFinished gates an explore agent's apparent settle (it is sitting at
// "waiting" after exploreSettledComplete already passed the deterministic
// SeenWorking/grace pre-filter) behind the small-model finished judge, throttled two
// ways so it can't burn tokens on a hot tick:
//   - COOLDOWN: skip entirely (no model) if the judge ran within finishJudgeCooldownMS
//     — bounds a churning tail and a stable parked tail alike to ~once per window.
//   - HASH LATCH: past the cooldown, if the tail is byte-identical to one we already
//     got a CONFIDENT not-finished on (FinishJudgeKey), skip the model — the verdict
//     can't change for an unchanged tail at temperature 0.
//
// LastFinishJudgeAt advances on every PAST-cooldown invocation (judge or hash-skip),
// so the absent path's read gate (finishJudgeOnCooldown) also stays fresh and stops
// re-reading a stable tail. A confident not-finished latches FinishJudgeKey; a
// transient model error (confidence below the floor) does NOT latch, so it retries
// after the next cooldown. Returns (finished, usedModel, answer).
func confirmExploreFinished(
	ctx *CheckContext, rec domain.WatcherRecord, signals WatcherSignals,
	prevState *TerminalState, outHash string, perTerminal map[string]TerminalState, terminalID string, now int64,
) (finished bool, usedModel bool, answer domain.ModelJudgeAnswer) {
	if finishJudgeOnCooldown(prevState, now) {
		return false, false, domain.ModelJudgeAnswer{}
	}
	// Past the cooldown — we are doing a check; advance the timer regardless of how it
	// resolves so the next window is honored (and the absent read gate self-clears).
	base := perTerminal[terminalID]
	base.LastFinishJudgeAt = now
	perTerminal[terminalID] = base

	if prevState != nil && prevState.FinishJudgeKey != "" && prevState.FinishJudgeKey == outHash {
		// Stable tail already confidently judged not-finished; skip the model call.
		return false, false, domain.ModelJudgeAnswer{}
	}
	ans, ok := judgeAgentFinished(ctx, rec, signals)
	if ok {
		return true, true, ans
	}
	// Latch ONLY a confident not-finished so a model blip (fallback confidence 0)
	// retries after the cooldown instead of permanently latching this tail.
	if ans.Confidence >= judgeConfidenceFloor {
		base = perTerminal[terminalID]
		base.FinishJudgeKey = outHash
		perTerminal[terminalID] = base
	}
	return false, true, ans
}

// finishedEvidence folds a judge answer's reason into a one-line evidence string,
// or returns "" when there is nothing useful to attach.
func finishedEvidence(prefix string, a domain.ModelJudgeAnswer) string {
	if strings.TrimSpace(a.Reason) == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", prefix, a.Reason)
}
