package extractionx

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools/terminalobs"
)

// The cross-call settle memory (Deps.Observations) is the fix for the re-await
// tax: a wait on an agent this session already watched work must not start from
// zero evidence and stall out the 20s spawn grace (ses_49ca848d: a 20.1s
// re-await of an agent that had been idle for 16s). These tests pin both
// directions — seeding IN and marking OUT — and the send-invalidation guard
// that keeps the seed from producing pre-start false settles.

// A re-await of an already-idle agent settles on the FIRST poll when the memory
// carries a working observation — no grace wait, still pure FSM.
func TestAwaitCohort_SeededSeenWorkingSettlesFirstPoll(t *testing.T) {
	mem := terminalobs.NewMemory()
	mem.MarkWorking("t1", 100)
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("waiting", "", "final answer printed")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}, Observations: mem}

	out, attempts, _ := awaitCohort(context.Background(), deps, []string{"t1"}, 0, 5, 0, clockSeq(0, 0, 0, 0, 0))

	if attempts != 1 {
		t.Fatalf("a seeded seenWorking should settle the first poll, got %d attempts", attempts)
	}
	if o := out["t1"]; o == nil || o.status != domain.SettleStatusFinished {
		t.Fatalf("want finished on the seed, got %+v", o)
	}
}

// An input injection AFTER the last working observation invalidates the seed:
// the injected command may start a new task, so a bare pre-start "waiting" must
// keep polling inside the grace exactly as before.
func TestAwaitCohort_CommandSentInvalidatesSeed(t *testing.T) {
	mem := terminalobs.NewMemory()
	mem.MarkWorking("t1", 100)
	mem.MarkCommandSent("t1", 200)
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("waiting", "", "old finished output still on screen")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}, Observations: mem}

	// The clock stays inside the grace window, so only the (invalidated) seed
	// could have settled this — it must run to the cap still working.
	out, attempts, _ := awaitCohort(context.Background(), deps, []string{"t1"}, 0, 3, 0, clockSeq(0, 0, 0, 0))

	if attempts != 3 {
		t.Fatalf("an invalidated seed must keep polling to the cap, got %d attempts", attempts)
	}
	if out["t1"] != nil {
		t.Fatalf("must NOT settle a never-worked waiting inside the grace, got %+v", out["t1"])
	}
}

// Live working observations flow INTO the memory, so the NEXT wait on the same
// terminal starts pre-latched (the first await funds the re-await).
func TestAwaitCohort_MarksWorkingIntoMemory(t *testing.T) {
	mem := terminalobs.NewMemory()
	reader := &cohortReader{seq: []map[string]TerminalStatusEntry{
		{"t1": ent("working", "", "busy…")},
		{"t1": ent("waiting", "", "done")},
	}}
	deps := Deps{Reader: reader, Router: &safeRouter{}, Observations: mem}

	out, _, _ := awaitCohort(context.Background(), deps, []string{"t1"}, 0, 5, 0, clockSeq(0, 1000, 2000, 3000))

	if o := out["t1"]; o == nil || !o.finished {
		t.Fatalf("cohort should finish, got %+v", o)
	}
	if !mem.SeenWorkingSinceLastCommand("t1") {
		t.Fatal("the live working observation must be recorded for later waits")
	}
}

// The settle-wait poll (terminal.extract wait:{}) seeds the FinishPreFilter gate
// the same way: a seeded agent reaches the finished judge after one quiet window
// instead of stalling to the 20s grace.
func TestPollUntil_SettleSeededSkipsGrace(t *testing.T) {
	mem := terminalobs.NewMemory()
	mem.MarkWorking("t1", 100)
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "all done here.")}}
	deps, router, args := settlePollArgs(reader, 4)
	deps.Observations = mem
	// The clock never reaches FinishSettleGraceMS (20s) past startedAt — only the
	// seed can open the gate. It starts non-zero because outAt==0 means "unset" to
	// nextOutputState. Attempt 1 defers on the quiet threshold (the tail was just
	// baselined); attempt 2 is quiet and judges.
	args.nowFn = clockSeq(1000, 1000, 3000, 5000, 7000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9, Reason: "done"}
	}

	res := pollUntil(context.Background(), deps, args)
	if !res.matched || res.attempts != 2 {
		t.Fatalf("seeded settle should confirm on attempt 2 (one quiet window), got matched=%v attempts=%d", res.matched, res.attempts)
	}
	if router.judgeCalls != 1 {
		t.Fatalf("the judge still gates the settle (exactly once), got %d calls", router.judgeCalls)
	}
}

// Without the seed the same scenario defers until the final-attempt escape:
// pinning that the seed (not some other change) is what unlocks the EARLY settle
// above. (The last attempt always gets one judge by design, so the unseeded poll
// matches only at the cap.)
func TestPollUntil_SettleUnseededStillWaitsForGrace(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "all done here.")}}
	deps, router, args := settlePollArgs(reader, 4)
	args.nowFn = clockSeq(1000, 1000, 3000, 5000, 7000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9, Reason: "done"}
	}

	res := pollUntil(context.Background(), deps, args)
	if res.attempts != 4 {
		t.Fatalf("an unseeded never-worked waiting must defer to the final-attempt escape (attempt 4), got matched=%v attempts=%d", res.matched, res.attempts)
	}
}
