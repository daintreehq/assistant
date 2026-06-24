package domain

// Finish-detection policy — the SINGLE source of truth for the cheap, deterministic
// decision that precedes the small-model "has this agent finished its turn?" judge
// (FinishedJudgeQuestion). The judge QUESTION and the judge CALL are already shared
// across the watcher daemon and the in-turn extract/await path; this unifies the
// WRAPPER POLICY — WHEN to spend a judge call, and WHEN a hard FSM fact already
// settles it — so the two consumers can no longer drift. (They DID drift: the
// in-turn extract poll fed the judge no silence signal and latched a confident-NO on
// the tail hash, so it timed out at 60s on an agent the watcher had already
// confirmed finished. This function encodes the watcher's proven behaviour.)
//
// FSM agentState is only a HINT for WHEN to look: "completed"/"exited" are hard
// terminal facts (accept without a model call); "working" is a hard "not done"; a
// bare "waiting" is an UNRELIABLE proxy (an agent reads "waiting" parked at a
// pre-start prompt, paused mid-task, or when its window is backgrounded), so it is
// resolved by the small-model judge on the tail — never accepted on its own.

const (
	// FinishJudgeConfidenceFloor is the minimum judge confidence for a YES (or a
	// confident NO) to count. Below it the verdict is "unsure" → keep polling.
	FinishJudgeConfidenceFloor = 0.6

	// FinishSettleGraceMS: accept a stable idle this long after spawn even if a live
	// "working" tick was never witnessed (a fast agent that finished between two
	// polls). The judge still gates acceptance; the grace only relaxes the
	// deterministic seenWorking pre-filter so a missed transition can't stall the poll.
	FinishSettleGraceMS int64 = 20_000

	// FinishQuietThresholdMS: a "waiting" agent whose tail is STILL advancing is not
	// settled, no matter how long since spawn — output in flight means work in flight.
	// Require the tail quiet at least this long before spending a judge call. This
	// DOMINATES the grace (an actively-printing agent is never "done", however old).
	FinishQuietThresholdMS int64 = 1_500

	// WatcherFinishCooldownMS / SettleFinishCooldownMS bound how often the finished
	// judge runs for one terminal: the background watcher loop ticks ~3s, the in-turn
	// poll loop ~2s, so each caps the judge to ~once per window regardless of tail
	// churn. Both RE-JUDGE on the next window even on a byte-identical tail, because
	// the judge's lastOutputAt input keeps growing while the bytes stay fixed — so a
	// quiet agent's verdict can legitimately flip NO→YES. There is deliberately NO
	// permanent tail-hash latch (that stranded a finished-but-static agent until
	// timeout — the exact in-turn bug this overhaul removes).
	WatcherFinishCooldownMS int64 = 15_000
	SettleFinishCooldownMS  int64 = 5_000
)

// FinishPreFilterInput is the cheap, deterministic context for one terminal at one
// tick. All durations are milliseconds.
type FinishPreFilterInput struct {
	AgentState       string // "working" | "waiting" | "completed" | "exited" | ""
	WaitingReason    string // "question" | "prompt" | "" (only meaningful while waiting)
	SeenWorking      bool   // latched once output advanced OR a live "working" was read
	MsSinceSpawn     int64  // now - spawn/creation (NOT a witnessed transition)
	MsSinceOutput    int64  // now - last tail change (0 = just changed / unknown)
	MsSinceLastJudge int64  // now - last judge call; 0 = never judged
	CooldownMS       int64  // caller's judge cooldown (WatcherFinishCooldownMS / SettleFinishCooldownMS)
	GraceMS          int64  // caller's spawn grace (FinishSettleGraceMS)
	QuietMS          int64  // caller's quiet threshold (FinishQuietThresholdMS)
	IsFinalAttempt   bool   // bounded poll only: bypass grace/quiet on the very last attempt
	TailEmpty        bool   // trimmed tail == ""
}

// FinishPreFilter decides what to do with the cheap FSM hint THIS tick:
//   - terminalAccept=true → completed/exited: a hard terminal fact; accept WITHOUT a
//     judge call.
//   - shouldJudge=true    → a "waiting" agent past the seenWorking/grace gate, quiet
//     long enough, off cooldown, with a non-empty tail: ask the small-model judge
//     (which is AUTHORITATIVE over the bare "waiting").
//   - both false          → not yet (working, blocked on a question, pre-start, still
//     printing, on cooldown, or a blank tail).
//
// A "waiting" agent whose WaitingReason is "question" is NEVER "finished" — it is
// blocked on the human/orchestrator and must be surfaced as needs-attention, not
// judged as done (else a relay sends it the next round while it sits on a question).
func FinishPreFilter(in FinishPreFilterInput) (shouldJudge, terminalAccept bool) {
	switch in.AgentState {
	case string(AgentCompleted), string(AgentExited):
		return false, true // hard terminal facts — accept without a model call
	case string(AgentWaiting):
		// The only judgeable soft state. Fall through to the gate below.
	default:
		// working, idle, directing, OR unknown/"" (e.g. a failed status read) — NOT a
		// settle signal. Never judge it: judging an unknown-state tail could falsely
		// complete a terminal we could not even read the state of.
		return false, false
	}
	if in.WaitingReason == "question" {
		return false, false // blocked on a question — never "finished"
	}
	gateOK := in.SeenWorking || in.MsSinceSpawn >= in.GraceMS || in.IsFinalAttempt
	if !gateOK || in.TailEmpty {
		return false, false
	}
	// Still printing → still working: quiet dominates grace (bypassed on the last attempt).
	if !in.IsFinalAttempt && in.QuietMS > 0 && in.MsSinceOutput < in.QuietMS {
		return false, false
	}
	// Rate-limit: re-judge at most once per cooldown window — EXCEPT on the final
	// attempt, which always gets one last judge so a completion that appears within a
	// cooldown of the previous NO is not starved into a false timeout.
	if !in.IsFinalAttempt && in.MsSinceLastJudge != 0 && in.MsSinceLastJudge < in.CooldownMS {
		return false, false
	}
	return true, false
}
