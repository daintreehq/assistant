package domain

import "testing"

// FinishPreFilter is the shared finish-detection policy; these pin every branch so
// the watcher and the in-turn extract/await path can never drift on what "ready to
// judge" / "hard-accept" / "not yet" means.
func TestFinishPreFilter(t *testing.T) {
	base := FinishPreFilterInput{
		AgentState:    string(AgentWaiting),
		SeenWorking:   true,
		MsSinceOutput: 5_000, // quiet
		CooldownMS:    SettleFinishCooldownMS,
		GraceMS:       FinishSettleGraceMS,
		QuietMS:       FinishQuietThresholdMS,
	}

	cases := []struct {
		name            string
		mutate          func(in *FinishPreFilterInput)
		wantJudge       bool
		wantTerminalAcc bool
	}{
		{"completed is a hard accept, no judge", func(in *FinishPreFilterInput) { in.AgentState = string(AgentCompleted) }, false, true},
		{"exited is a hard accept, no judge", func(in *FinishPreFilterInput) { in.AgentState = string(AgentExited) }, false, true},
		{"working is hard not-done", func(in *FinishPreFilterInput) { in.AgentState = string(AgentWorking) }, false, false},
		{"idle is never judged", func(in *FinishPreFilterInput) { in.AgentState = string(AgentIdle) }, false, false},
		{"unknown/empty state (e.g. failed status read) is never judged", func(in *FinishPreFilterInput) { in.AgentState = "" }, false, false},
		{"unknown state is never judged even on the final attempt", func(in *FinishPreFilterInput) {
			in.AgentState = ""
			in.IsFinalAttempt = true
		}, false, false},
		{"waiting + seenWorking + quiet → judge", func(in *FinishPreFilterInput) {}, true, false},
		{"waiting on a question is never finished", func(in *FinishPreFilterInput) { in.WaitingReason = "question" }, false, false},
		{"waiting but tail empty → wait for evidence", func(in *FinishPreFilterInput) { in.TailEmpty = true }, false, false},
		// The seenWorking RACE: a fast agent that finished between polls (never seen
		// working) must still get judged once the spawn grace elapsed.
		{"waiting, never seen working, but past grace → judge", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = FinishSettleGraceMS + 1
		}, true, false},
		{"waiting, never seen working, inside grace → not yet", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = 1_000
		}, false, false},
		// Quiet DOMINATES grace: an actively-printing agent is never "settled".
		{"waiting, tail still advancing → not yet, even past grace", func(in *FinishPreFilterInput) {
			in.MsSinceSpawn = FinishSettleGraceMS + 10_000
			in.MsSinceOutput = 200 // < quiet threshold
		}, false, false},
		{"on cooldown → not yet", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = 1_000 // < SettleFinishCooldownMS
		}, false, false},
		{"past cooldown → judge", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = SettleFinishCooldownMS + 1
		}, true, false},
		{"final attempt bypasses cooldown so a fresh completion isn't starved", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = 1_000 // < cooldown, would normally defer
			in.IsFinalAttempt = true
		}, true, false},
		// The final attempt bypasses grace AND quiet so a short poll budget can still
		// confirm a fast agent never seen working — the judge is still the backstop.
		{"final attempt bypasses grace+quiet", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = 1_000
			in.MsSinceOutput = 100
			in.IsFinalAttempt = true
		}, true, false},
		{"final attempt still won't judge a question", func(in *FinishPreFilterInput) {
			in.WaitingReason = "question"
			in.IsFinalAttempt = true
		}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			gotJudge, gotAcc := FinishPreFilter(in)
			if gotJudge != tc.wantJudge || gotAcc != tc.wantTerminalAcc {
				t.Fatalf("FinishPreFilter() = (judge=%v, accept=%v), want (judge=%v, accept=%v)",
					gotJudge, gotAcc, tc.wantJudge, tc.wantTerminalAcc)
			}
		})
	}
}
