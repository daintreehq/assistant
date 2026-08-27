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
		name   string
		mutate func(in *FinishPreFilterInput)
		want   FinishDecision
	}{
		{"completed is a hard accept, no judge", func(in *FinishPreFilterInput) { in.AgentState = string(AgentCompleted) }, FinishAccept},
		{"exited is a hard accept, no judge", func(in *FinishPreFilterInput) { in.AgentState = string(AgentExited) }, FinishAccept},
		{"working is hard not-done", func(in *FinishPreFilterInput) { in.AgentState = string(AgentWorking) }, FinishKeepWaiting},
		{"idle is never judged", func(in *FinishPreFilterInput) { in.AgentState = string(AgentIdle) }, FinishKeepWaiting},
		{"unknown/empty state (e.g. failed status read) is never judged", func(in *FinishPreFilterInput) { in.AgentState = "" }, FinishKeepWaiting},
		{"unknown state is never judged even on the final attempt", func(in *FinishPreFilterInput) {
			in.AgentState = ""
			in.IsFinalAttempt = true
		}, FinishKeepWaiting},
		{"waiting + seenWorking + quiet → judge", func(in *FinishPreFilterInput) {}, FinishJudge},
		{"a question is blocked, not merely unfinished", func(in *FinishPreFilterInput) { in.WaitingReason = WaitingQuestion }, FinishBlocked},
		// approval and error used to fall straight through the question guard and be
		// JUDGED — on a tail still showing real output from earlier in the turn, judged
		// finished. An agent sitting on "Do you trust this folder?" is not done.
		{"an approval dialog is blocked", func(in *FinishPreFilterInput) { in.WaitingReason = WaitingApproval }, FinishBlocked},
		{"a blocking error is blocked", func(in *FinishPreFilterInput) { in.WaitingReason = WaitingError }, FinishBlocked},
		// "prompt" and "" are the ORDINARY settled-at-a-prompt case — the one the judge
		// exists to confirm. Blocking those would strand every normal finish.
		{"an ordinary prompt is still judged", func(in *FinishPreFilterInput) { in.WaitingReason = WaitingPrompt }, FinishJudge},
		{"waiting but tail empty → wait for evidence", func(in *FinishPreFilterInput) { in.TailEmpty = true }, FinishKeepWaiting},
		// The seenWorking RACE: a fast agent that finished between polls (never seen
		// working) must still get judged once the spawn grace elapsed.
		{"waiting, never seen working, but past grace → judge", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = FinishSettleGraceMS + 1
		}, FinishJudge},
		{"waiting, never seen working, inside grace → not yet", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = 1_000
		}, FinishKeepWaiting},
		// Quiet DOMINATES grace: an actively-printing agent is never "settled".
		{"waiting, tail still advancing → not yet, even past grace", func(in *FinishPreFilterInput) {
			in.MsSinceSpawn = FinishSettleGraceMS + 10_000
			in.MsSinceOutput = 200 // < quiet threshold
		}, FinishKeepWaiting},
		{"on cooldown → not yet", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = 1_000 // < SettleFinishCooldownMS
		}, FinishKeepWaiting},
		{"past cooldown → judge", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = SettleFinishCooldownMS + 1
		}, FinishJudge},
		{"final attempt bypasses cooldown so a fresh completion isn't starved", func(in *FinishPreFilterInput) {
			in.MsSinceLastJudge = 1_000 // < cooldown, would normally defer
			in.IsFinalAttempt = true
		}, FinishJudge},
		// The final attempt bypasses grace AND quiet so a short poll budget can still
		// confirm a fast agent never seen working — the judge is still the backstop.
		{"final attempt bypasses grace+quiet", func(in *FinishPreFilterInput) {
			in.SeenWorking = false
			in.MsSinceSpawn = 1_000
			in.MsSinceOutput = 100
			in.IsFinalAttempt = true
		}, FinishJudge},
		{"the final attempt still won't judge a blocked agent", func(in *FinishPreFilterInput) {
			in.WaitingReason = WaitingQuestion
			in.IsFinalAttempt = true
		}, FinishBlocked},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mutate(&in)
			if got := FinishPreFilter(in); got != tc.want {
				t.Fatalf("FinishPreFilter() = %v, want %v", got, tc.want)
			}
		})
	}
}
