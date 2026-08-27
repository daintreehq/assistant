package domain

import "testing"

// ptrInt is a local pointer helper for the exitCode argument (nil means "no exit
// code reported", which is distinct from 0).
func ptrInt(v int) *int { return &v }

// SettleAgentFSM is the SINGLE source of truth for "has this agent terminal
// settled?", shared by terminal.awaitAll (the in-turn cohort wait) and the async
// coordinator (the out-of-turn durable-futures poll). A drift between those two
// waits is invisible until it strands a finished agent or reports a working one as
// done, so every branch is pinned here.
//
// The load-bearing subtlety is the "waiting" arm: a bare waiting state is NOT
// evidence of completion (pre-start, paused, and backgrounded agents all read
// waiting), so it settles only with positive evidence — either we caught the agent
// working first, or a stable idle outlasted the spawn grace.
func TestSettleAgentFSM(t *testing.T) {
	const grace int64 = 10_000

	cases := []struct {
		name          string
		agentState    string
		waitingReason string
		exitCode      *int
		seenWorking   bool
		msSinceSpawn  int64
		want          AgentSettleVerdict
	}{
		// --- hard terminal facts ---
		{
			name:       "completed is finished regardless of anything else",
			agentState: string(AgentCompleted),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			name:       "exited with no exit code is finished",
			agentState: string(AgentExited),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			name:       "exited zero is finished",
			agentState: string(AgentExited),
			exitCode:   ptrInt(0),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			name:       "exited nonzero is failed but still Finished (it is terminal)",
			agentState: string(AgentExited),
			exitCode:   ptrInt(1),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFailed, Finished: true},
		},
		{
			name:       "exited negative code is failed too (signal death)",
			agentState: string(AgentExited),
			exitCode:   ptrInt(-9),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFailed, Finished: true},
		},
		{
			// completed never consults the exit code — a stale nonzero code attached to
			// a completed terminal must not downgrade it to failed.
			name:       "completed ignores a nonzero exit code",
			agentState: string(AgentCompleted),
			exitCode:   ptrInt(3),
			want:       AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},

		// --- the soft "waiting" arm ---
		// The three BLOCKED reasons. Only "question" was ever handled; approval and
		// error fell through to the seenWorking/grace branch below and were scored
		// FINISHED, so a cohort wait reported an agent parked on a trust prompt or dead
		// on a rate limit as done, and the relay sent it the next round.
		{
			name:          "an approval dialog blocks like a question does",
			agentState:    string(AgentWaiting),
			waitingReason: WaitingApproval,
			seenWorking:   true,
			msSinceSpawn:  grace + 1,
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusQuestion, Finished: false},
		},
		{
			// A blocking error is settled and FAILED, but NOT Finished: unlike a nonzero
			// exit the process is alive and the work is undone, which is what keeps its
			// supervisor watcher in place instead of being retired.
			name:          "a blocking error is failed and not finished",
			agentState:    string(AgentWaiting),
			waitingReason: WaitingError,
			seenWorking:   true,
			msSinceSpawn:  grace + 1,
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusFailed, Finished: false},
		},
		{
			// The ordinary settled-at-a-prompt case must be untouched by the widened
			// vocabulary — blocking it would strand every normal completion.
			name:          "an ordinary prompt still finishes",
			agentState:    string(AgentWaiting),
			waitingReason: WaitingPrompt,
			seenWorking:   true,
			msSinceSpawn:  grace + 1,
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			// A blocked agent is blocked whether or not it was ever seen working, and
			// whether or not the spawn grace has elapsed — those gates exist to avoid a
			// FALSE finish, and this is not a finish at all.
			name:          "an approval blocks even before the grace, never having worked",
			agentState:    string(AgentWaiting),
			waitingReason: WaitingApproval,
			seenWorking:   false,
			msSinceSpawn:  1,
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusQuestion, Finished: false},
		},
		{
			// A question blocks on the ORCHESTRATOR, so it settles the wait (stop
			// polling, hand back to the model) without being finished work.
			name:          "waiting on a question settles but is NOT finished",
			agentState:    string(AgentWaiting),
			waitingReason: "question",
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusQuestion, Finished: false},
		},
		{
			// Ordering guard: the question branch must win over the seenWorking
			// shortcut, or an agent that worked and then asked would be reported
			// finished and its question silently dropped.
			name:          "question wins over seenWorking",
			agentState:    string(AgentWaiting),
			waitingReason: "question",
			seenWorking:   true,
			msSinceSpawn:  grace * 10,
			want:          AgentSettleVerdict{Settled: true, Status: SettleStatusQuestion, Finished: false},
		},
		{
			name:         "waiting after a real working->waiting transition is finished",
			agentState:   string(AgentWaiting),
			seenWorking:  true,
			msSinceSpawn: 0,
			want:         AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			name:         "never-worked waiting past the grace is finished (fast agent we never caught mid-work)",
			agentState:   string(AgentWaiting),
			msSinceSpawn: grace + 1,
			want:         AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			// The comparison is >=, so the exact boundary settles. Pinned because an
			// off-by-one here silently adds a whole poll interval to every fast agent.
			name:         "never-worked waiting exactly at the grace boundary is finished",
			agentState:   string(AgentWaiting),
			msSinceSpawn: grace,
			want:         AgentSettleVerdict{Settled: true, Status: SettleStatusFinished, Finished: true},
		},
		{
			// With zero positive evidence the agent ever did anything, "still working"
			// is more honest than a fabricated "finished" — this is the guard against
			// reporting a pre-start prompt as a completed task.
			name:         "never-worked waiting before the grace does NOT settle",
			agentState:   string(AgentWaiting),
			msSinceSpawn: grace - 1,
			want:         AgentSettleVerdict{},
		},
		{
			// A non-"question" reason is not special — it takes the same evidence path.
			name:          "waiting with an unrecognized reason still needs evidence",
			agentState:    string(AgentWaiting),
			waitingReason: "paused",
			msSinceSpawn:  grace - 1,
			want:          AgentSettleVerdict{},
		},

		// --- never-settling states ---
		{name: "working never settles", agentState: string(AgentWorking), seenWorking: true, msSinceSpawn: grace * 10, want: AgentSettleVerdict{}},
		{name: "idle never settles", agentState: string(AgentIdle), seenWorking: true, msSinceSpawn: grace * 10, want: AgentSettleVerdict{}},
		{name: "directing never settles", agentState: string(AgentDirecting), seenWorking: true, msSinceSpawn: grace * 10, want: AgentSettleVerdict{}},
		{
			// A failed status read yields an empty state; it must never be mistaken
			// for a terminal one.
			name:         "empty state (failed status read) never settles",
			agentState:   "",
			seenWorking:  true,
			msSinceSpawn: grace * 10,
			want:         AgentSettleVerdict{},
		},
		{name: "unknown state never settles", agentState: "bogus", seenWorking: true, want: AgentSettleVerdict{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SettleAgentFSM(tc.agentState, tc.waitingReason, tc.exitCode, tc.seenWorking, tc.msSinceSpawn, grace)
			if got != tc.want {
				t.Fatalf("SettleAgentFSM(%q, %q, %v, %v, %d, %d) = %+v, want %+v",
					tc.agentState, tc.waitingReason, tc.exitCode, tc.seenWorking, tc.msSinceSpawn, grace, got, tc.want)
			}
		})
	}
}

// SettleStatusWorking is the caller-side label for "the deadline caught this
// terminal unsettled". SettleAgentFSM must NEVER return it — an unsettled verdict
// is Settled=false with an empty Status — or a consumer that switches on Status
// would treat a still-running agent as a settled outcome.
func TestSettleAgentFSMNeverReturnsWorkingStatus(t *testing.T) {
	states := []string{
		string(AgentIdle), string(AgentWorking), string(AgentWaiting),
		string(AgentDirecting), string(AgentCompleted), string(AgentExited), "", "bogus",
	}
	for _, st := range states {
		for _, reason := range []string{"", "question", "paused"} {
			for _, seen := range []bool{false, true} {
				for _, ms := range []int64{0, 5_000, 100_000} {
					got := SettleAgentFSM(st, reason, nil, seen, ms, 10_000)
					if got.Status == SettleStatusWorking {
						t.Fatalf("SettleAgentFSM(%q, %q, nil, %v, %d) returned SettleStatusWorking", st, reason, seen, ms)
					}
					if !got.Settled && got.Status != "" {
						t.Fatalf("SettleAgentFSM(%q, %q, nil, %v, %d) unsettled but carried Status %q", st, reason, seen, ms, got.Status)
					}
					if !got.Settled && got.Finished {
						t.Fatalf("SettleAgentFSM(%q, %q, nil, %v, %d) unsettled but Finished", st, reason, seen, ms)
					}
				}
			}
		}
	}
}
