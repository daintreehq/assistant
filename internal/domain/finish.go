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

// FinishDecision is what the cheap pre-filter concluded for one terminal at one tick.
// A typed verdict rather than a pair of booleans because there are now FOUR outcomes and
// the fourth (Blocked) is the one a caller most easily forgets: silently treating it as
// "not yet" is exactly the bug that made a blocked agent burn a whole wait budget.
type FinishDecision int

const (
	// FinishKeepWaiting: not done and not blocked — working, pre-start, still
	// printing, on cooldown, or a blank tail. Poll again.
	FinishKeepWaiting FinishDecision = iota
	// FinishJudge: a "waiting" agent past the seenWorking/grace gate, quiet long
	// enough, off cooldown, with a non-empty tail. Ask the small-model finished judge,
	// which is AUTHORITATIVE over the bare "waiting".
	FinishJudge
	// FinishAccept: completed/exited — a hard terminal fact. Accept WITHOUT a judge.
	FinishAccept
	// FinishBlocked: the agent is parked on something only a human or the orchestrator
	// can clear (a question, an approval dialog, a blocking error). It is settled and
	// it is NOT finished, and no amount of further polling changes that — so a bounded
	// wait must stop and SAY so rather than grind to its attempt cap and report a bare
	// timeout. Read WaitingReason for which of the three it is.
	FinishBlocked
)

// String names the decision so a failed assertion reads "FinishBlocked" rather than "3".
func (d FinishDecision) String() string {
	switch d {
	case FinishKeepWaiting:
		return "FinishKeepWaiting"
	case FinishJudge:
		return "FinishJudge"
	case FinishAccept:
		return "FinishAccept"
	case FinishBlocked:
		return "FinishBlocked"
	default:
		return "FinishDecision(?)"
	}
}

// FinishPreFilterInput is the cheap, deterministic context for one terminal at one
// tick. All durations are milliseconds.
type FinishPreFilterInput struct {
	AgentState string // "working" | "waiting" | "completed" | "exited" | ""
	// WaitingReason is Daintree's classification of WHY an agent is waiting, and its
	// full vocabulary is "prompt" | "question" | "approval" | "error" | "" (only
	// meaningful while waiting). This used to be documented, and treated, as only
	// question/prompt: an agent parked on a tool-APPROVAL dialog, or stopped dead on a
	// blocking ERROR (rate limit, auth failure, network), therefore fell through the
	// question guard and was judged — and, on a tail that still showed real output from
	// earlier in the turn, judged FINISHED. Blocked is blocked, whichever of the three
	// it is.
	WaitingReason    string
	SeenWorking      bool  // latched once output advanced OR a live "working" was read
	MsSinceSpawn     int64 // now - spawn/creation (NOT a witnessed transition)
	MsSinceOutput    int64 // now - last tail change (0 = just changed / unknown)
	MsSinceLastJudge int64 // now - last judge call; 0 = never judged
	CooldownMS       int64 // caller's judge cooldown (WatcherFinishCooldownMS / SettleFinishCooldownMS)
	GraceMS          int64 // caller's spawn grace (FinishSettleGraceMS)
	QuietMS          int64 // caller's quiet threshold (FinishQuietThresholdMS)
	IsFinalAttempt   bool  // bounded poll only: bypass grace/quiet on the very last attempt
	TailEmpty        bool  // trimmed tail == ""
}

// FinishPreFilter decides what to do with the cheap FSM hint THIS tick. See
// FinishDecision for the four outcomes.
//
// A "waiting" agent parked on a question, an approval dialog, or a blocking error is
// NEVER "finished" — it is blocked on the human/orchestrator and must be surfaced as
// needs-attention, not judged as done (else a relay sends it the next round while it
// sits on a prompt nobody answered). It returns FinishBlocked rather than
// FinishKeepWaiting so a bounded caller can stop immediately: the condition cannot
// become true by waiting, so every remaining poll is spent proving nothing, and the
// bare "condition not met" that follows names neither the cause nor the remedy.
func FinishPreFilter(in FinishPreFilterInput) FinishDecision {
	switch in.AgentState {
	case string(AgentCompleted), string(AgentExited):
		return FinishAccept // hard terminal facts — accept without a model call
	case string(AgentWaiting):
		// The only judgeable soft state. Fall through to the gate below.
	default:
		// working, idle, directing, OR unknown/"" (e.g. a failed status read) — NOT a
		// settle signal. Never judge it: judging an unknown-state tail could falsely
		// complete a terminal we could not even read the state of.
		return FinishKeepWaiting
	}
	if IsBlockingWaitingReason(in.WaitingReason) {
		return FinishBlocked
	}
	gateOK := in.SeenWorking || in.MsSinceSpawn >= in.GraceMS || in.IsFinalAttempt
	if !gateOK || in.TailEmpty {
		return FinishKeepWaiting
	}
	// Still printing → still working: quiet dominates grace (bypassed on the last attempt).
	if !in.IsFinalAttempt && in.QuietMS > 0 && in.MsSinceOutput < in.QuietMS {
		return FinishKeepWaiting
	}
	// Rate-limit: re-judge at most once per cooldown window — EXCEPT on the final
	// attempt, which always gets one last judge so a completion that appears within a
	// cooldown of the previous NO is not starved into a false timeout.
	if !in.IsFinalAttempt && in.MsSinceLastJudge != 0 && in.MsSinceLastJudge < in.CooldownMS {
		return FinishKeepWaiting
	}
	return FinishJudge
}

// Waiting reasons, as Daintree classifies them (WaitingReasonClassifier). Kept here
// beside the policy that branches on them so the two cannot drift again: the CLI knew
// only two of the four, and the two it did not know were both BLOCKED states that its
// settle logic scored as finished.
const (
	// WaitingPrompt is an ordinary idle prompt — the agent is done and ready for input.
	WaitingPrompt = "prompt"
	// WaitingQuestion is the agent asking the orchestrator something.
	WaitingQuestion = "question"
	// WaitingApproval is a tool/permission dialog waiting on a yes/no.
	WaitingApproval = "approval"
	// WaitingError is the agent stopped on a blocking error (rate limit, auth failure,
	// network) rather than at a ready prompt.
	WaitingError = "error"
)

// IsBlockingWaitingReason reports whether a "waiting" agent is parked on something that
// will not clear on its own. An empty reason and "prompt" are NOT blocking: those are
// the ordinary settled-at-a-prompt case the finish judge exists to confirm.
func IsBlockingWaitingReason(reason string) bool {
	switch reason {
	case WaitingQuestion, WaitingApproval, WaitingError:
		return true
	default:
		return false
	}
}

// BlockedReasonText renders a blocking waiting reason as the clause a model-facing
// message appends after the terminal id. One home for the wording so the in-turn wait,
// the cohort wait and the async coordinator describe the same state identically.
func BlockedReasonText(reason string) string {
	switch reason {
	case WaitingQuestion:
		return "asking a question"
	case WaitingApproval:
		return "waiting on an approval prompt"
	case WaitingError:
		return "stopped on a blocking error"
	default:
		return "blocked"
	}
}
