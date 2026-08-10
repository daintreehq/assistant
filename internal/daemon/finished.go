package daemon

import (
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
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
// The returned `judged` bool reports whether the model was ACTUALLY consulted — false
// on the fail-closed early-return (no tail / disconnected). Callers use it to avoid
// stamping the judge cooldown (or claiming a model spend) for a judge that never ran.
func judgeAgentFinished(ctx *CheckContext, rec domain.WatcherRecord, signals WatcherSignals) (answer domain.ModelJudgeAnswer, finished, judged bool) {
	// No evidence to judge → unknown → fail-closed WITHOUT a model call. (An empty tail
	// can't prove an agent finished; judging nothing risks hardening a transport hiccup
	// into a false completion. judged=false so the caller doesn't poison the cooldown — a
	// blank tail this tick, e.g. a failed deep read, must not suppress the next REAL judge.)
	if !ctx.MCP.Connected() || strings.TrimSpace(signals.Tail) == "" {
		return domain.ModelJudgeAnswer{}, false, false
	}
	results := runModelJudges(ctx, []string{domain.FinishedJudgeQuestion}, rec, signals)
	a, ok := results[domain.FinishedJudgeQuestion]
	confident := ok && a.Confidence >= judgeConfidenceFloor
	return a, confident && a.Matched, true
}

// finishJudgeCooldownMS bounds how often the finished judge runs for one terminal.
// The supervisor cadence is ~3s, so without a floor a parked explore agent whose
// tail keeps repainting (a spinner, an elapsed-time counter, an ANSI cursor redraw)
// would burn a small-model call every tick. The cooldown caps that to ~once per
// window regardless of tail churn; completion is still detected within one window. It
// also gates the absent-path terminal.getOutput read so a stable, idle-but-not-
// finished agent isn't re-read every tick.
//
// We deliberately do NOT also dedupe on the tail hash here (the extract poll does,
// safely). The WATCHER judge's prompt includes lastOutputAt (the silence duration),
// which keeps GROWING while the tail bytes stay identical — so a byte-stable tail can
// legitimately flip from "not finished" to "finished" as the agent goes quiet. A
// permanent tail-hash latch would suppress that flip forever and strand a genuinely
// finished agent until timeout; re-judging each cooldown window catches it.
const finishJudgeCooldownMS = 15_000

// finishJudgeOnCooldown reports whether the finished judge ran recently enough that
// we should skip even READING the tail this tick (used by the absent path, whose
// tail read is an MCP round-trip). LastFinishJudgeAt advances every time the judge
// runs, so this self-clears once the window elapses.
func finishJudgeOnCooldown(prevState *TerminalState, now int64) bool {
	return prevState != nil && prevState.LastFinishJudgeAt != 0 &&
		now-prevState.LastFinishJudgeAt < finishJudgeCooldownMS
}

// confirmExploreFinished gates an explore agent's apparent settle (it is sitting at
// "waiting" after exploreSettledComplete already passed the deterministic
// SeenWorking/grace pre-filter) behind the small-model finished judge, throttled by a
// cooldown so it can't burn tokens on a hot tick: within finishJudgeCooldownMS of the
// last judge we skip entirely (no model). Past the cooldown we re-judge — even on a
// byte-identical tail — because the judge's lastOutputAt input keeps changing, so the
// verdict can flip as the agent goes quiet. LastFinishJudgeAt advances on every judge
// so the absent path's read gate also stays fresh. Returns (finished, usedModel,
// answer); a transient model error / low confidence simply reports not-finished and
// retries after the next cooldown.
func confirmExploreFinished(
	ctx *CheckContext, rec domain.WatcherRecord, signals WatcherSignals,
	prevState *TerminalState, perTerminal map[string]TerminalState, terminalID string, now int64,
) (finished bool, usedModel bool, answer domain.ModelJudgeAnswer) {
	if finishJudgeOnCooldown(prevState, now) {
		return false, false, domain.ModelJudgeAnswer{}
	}
	ans, ok, judged := judgeAgentFinished(ctx, rec, signals)
	// Advance the cooldown ONLY when a model judge actually ran. judgeAgentFinished
	// fail-closes WITHOUT a model call on a blank tail (e.g. a failed deep read this
	// tick); stamping then would suppress the next REAL judge for a whole window and
	// strand a genuinely-finished agent. usedModel mirrors `judged` so the outcome's
	// epistemic kind isn't mislabeled as model-derived when no model was consulted.
	if judged {
		base := perTerminal[terminalID]
		base.LastFinishJudgeAt = now
		perTerminal[terminalID] = base
	}
	return ok, judged, ans
}

// finishedEvidence folds a judge answer's reason into a one-line evidence string,
// or returns "" when there is nothing useful to attach.
func finishedEvidence(prefix string, a domain.ModelJudgeAnswer) string {
	if strings.TrimSpace(a.Reason) == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", prefix, a.Reason)
}
