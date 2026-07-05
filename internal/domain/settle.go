package domain

// The pure-FSM agent settle policy — the SINGLE source of truth for "has this
// agent terminal settled?" decided from its FSM state ALONE (no tail read, no
// model call). Shared by terminal.awaitAll (the in-turn cohort wait) and the
// async coordinator (the out-of-turn durable-futures poll) so the two waits can
// never drift, exactly as FinishPreFilter unifies the judge-gated policy for
// the watcher and the extract settle wait. It lives in domain (pure, stdlib
// only) because both consumers sit in packages that must not import each other.

// Agent settle outcome statuses (the per-terminal verdicts both waits report).
const (
	SettleStatusFinished = "finished"
	SettleStatusFailed   = "failed"
	SettleStatusQuestion = "question"
	// SettleStatusWorking is the caller-side label for a terminal that never
	// settled before the wait budget/deadline ran out. SettleAgentFSM never
	// returns it (an unsettled verdict is Settled=false); it exists so both
	// consumers report the timeout case with one vocabulary.
	SettleStatusWorking = "working"
)

// AgentSettleVerdict is the pure-FSM settle decision for one terminal at one
// tick. Status/Finished are valid only when Settled.
type AgentSettleVerdict struct {
	Settled  bool
	Status   string // SettleStatusFinished | SettleStatusFailed | SettleStatusQuestion
	Finished bool
}

// SettleAgentFSM decides a terminal's settle verdict from its FSM state ALONE —
// no tail, no model call. completed/exited are hard terminal facts (exited is
// "failed" on a nonzero code). A "waiting" agent is the only soft case: a
// question blocks on the orchestrator (settled, NOT finished); any other
// "waiting" is finished IF we caught it working first (a real working→idle
// transition) OR a stable idle outlasted the spawn grace (a fast agent we never
// caught mid-work). A never-worked "waiting" before the grace is a possible
// pre-start prompt — NOT settled: with zero positive evidence the agent ever did
// anything, "still working" is more honest than a fabricated "finished".
// working/idle/directing/unknown never settle either.
func SettleAgentFSM(agentState, waitingReason string, exitCode *int, seenWorking bool, msSinceSpawn, graceMS int64) AgentSettleVerdict {
	switch agentState {
	case string(AgentCompleted):
		return AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true}
	case string(AgentExited):
		if exitCode != nil && *exitCode != 0 {
			return AgentSettleVerdict{Settled: true, Status: SettleStatusFailed, Finished: true}
		}
		return AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true}
	case string(AgentWaiting):
		if waitingReason == "question" {
			return AgentSettleVerdict{Settled: true, Status: SettleStatusQuestion, Finished: false}
		}
		if seenWorking || msSinceSpawn >= graceMS {
			return AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true}
		}
		return AgentSettleVerdict{} // never-worked pre-start 'waiting' before grace — keep polling
	default:
		return AgentSettleVerdict{}
	}
}

// AsyncTerminalOutcome is one watched terminal's settled verdict inside an async
// invocation's outcome ledger (AsyncInvocationRecord.OutcomesJson maps
// terminalId → this). Status uses the SettleStatus* vocabulary, with
// SettleStatusWorking marking a terminal the deadline caught still unsettled.
type AsyncTerminalOutcome struct {
	Status   string `json:"status"`
	ExitCode *int   `json:"exitCode,omitempty"`
	Reason   string `json:"reason,omitempty"`
}
