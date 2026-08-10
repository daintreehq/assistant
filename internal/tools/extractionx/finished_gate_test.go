package extractionx

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// seqReader returns a scripted SEQUENCE of statuses, one per ReadStatuses call
// (clamping to the last once exhausted), so a test can drive working→waiting across
// poll attempts. ReadOutput is never needed (recentOutput covers the small tailBytes
// the tests use), so it just reports a miss.
type seqReader struct {
	seq  []StatusReadResult
	call int
}

func (r *seqReader) Connected() bool                                  { return true }
func (r *seqReader) ListTerminals(_ context.Context) ([]string, bool) { return nil, false }
func (r *seqReader) ReadStatuses(_ context.Context, _ []string, _ bool) StatusReadResult {
	i := r.call
	if i >= len(r.seq) {
		i = len(r.seq) - 1
	}
	r.call++
	return r.seq[i]
}
func (r *seqReader) ReadOutput(_ context.Context, _ string, _ int) OutputReadResult {
	return OutputReadResult{OK: false}
}

func status(state, out string) StatusReadResult {
	return StatusReadResult{OK: true, ByID: map[string]TerminalStatusEntry{
		"t1": {AgentState: state, RecentOutput: strp(out)},
	}}
}

func settlePollArgs(reader *seqReader, maxAttempts int) (Deps, *routeRouter, pollArgs) {
	r := &routeRouter{}
	deps := Deps{Reader: reader, Router: r}
	w := settledWait()
	return deps, r, pollArgs{
		terminalIDs:  []string{"t1"},
		wait:         &w,
		isSettleWait: true,
		maxAttempts:  maxAttempts,
		tailBytes:    4, // < every recentOutput below → inline path, no deep read / readFailed
	}
	// pollIntervalMs 0 ⇒ no delay; the wall-clock stays inside extractSettleGraceMS so
	// the grace fallback never fires — the latch/judge alone drive the outcome.
}

// A bare "waiting" on the FIRST read (the agent has never been seen working — the
// real-world pre-start / backgrounded-window case) must NOT settle: this is exactly
// the att=1 false-settle the gate closes. The poll resolves only once the agent has
// worked, its tail has gone QUIET (the shared quiet threshold — an agent whose output
// is still moving is still working), AND the model confirms it finished. So the final
// answer printed on attempt 3 is confirmed on attempt 4, once the tail has been quiet.
func TestPollUntil_SettleWaitRequiresSeenWorkingThenConfirm(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{
		status("waiting", "ready to start ▷"), // attempt 1: never worked → must defer
		status("working", "reading files…"),   // attempt 2: latches seenWorking
		status("waiting", "all done here."),   // attempt 3: final answer JUST printed → quiet defers
		status("waiting", "all done here."),   // attempt 4: tail quiet → settle + confirm
	}}
	deps, router, args := settlePollArgs(reader, 6)
	// att1..3 at t=0 (the answer's outAt latches at 0); att4 at t=2000 so the tail has
	// been quiet > FinishQuietThresholdMS and the finished judge runs.
	args.nowFn = clockSeq(0, 0, 0, 2000, 4000, 6000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9, Reason: "done"}
	}

	res := pollUntil(context.Background(), deps, args)
	if !res.matched {
		t.Fatalf("should settle once worked+quiet+confirmed, got matched=false after %d attempts", res.attempts)
	}
	if res.attempts != 4 {
		t.Fatalf("should settle on attempt 4 (att1 pre-start defer, att3 tail-not-yet-quiet defer), got %d", res.attempts)
	}
	if router.judgeCalls != 1 {
		t.Fatalf("judge should be consulted exactly once (only on the quiet post-working waiting), got %d", router.judgeCalls)
	}
}

// A settle that the model never confirms (the agent flips to "waiting" but is still
// mid-task) must keep polling to the cap and time out — never grab mid-flight output.
// The identical not-finished tail is judged ONCE (dedupe), not every attempt.
func TestPollUntil_SettleNeverConfirmedTimesOut(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{
		status("working", "still working…"),
		status("waiting", "paused mid-task…"), // same tail repeats → dedupe
	}}
	deps, router, args := settlePollArgs(reader, 4)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "still mid-task"}
	}

	res := pollUntil(context.Background(), deps, args)
	if res.matched {
		t.Fatal("unconfirmed settle must NOT match (no mid-flight grab)")
	}
	if router.judgeCalls != 1 {
		t.Fatalf("confident not-finished on an unchanged tail should be judged once (deduped), got %d", router.judgeCalls)
	}
}

// completed/exited are authoritative terminal states — a settle wait accepts them
// immediately, with NO model confirmation (the judge must not be consulted).
func TestPollUntil_SettleExitedAcceptedWithoutJudge(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("exited", "process exited")}}
	deps, router, args := settlePollArgs(reader, 4)

	res := pollUntil(context.Background(), deps, args)
	if !res.matched || res.attempts != 1 {
		t.Fatalf("exited should settle immediately, got matched=%v attempts=%d", res.matched, res.attempts)
	}
	if router.judgeCalls != 0 {
		t.Fatalf("a hard terminal state must not consult the finished judge, got %d calls", router.judgeCalls)
	}
}

// An EXPLICIT wait condition (not the coerced wait:{}) stays strict and model-free:
// a deterministic match returns immediately with no latch and no judge call.
func TestPollUntil_ExplicitConditionUnchangedNoJudge(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "at prompt")}}
	r := &routeRouter{}
	waiting := domain.AgentWaiting
	args := pollArgs{
		terminalIDs:  []string{"t1"},
		wait:         &domain.WatchCondition{StateIs: &waiting},
		isSettleWait: false, // explicit stateIs:waiting — strict
		maxAttempts:  4,
		tailBytes:    4,
	}
	res := pollUntil(context.Background(), Deps{Reader: reader, Router: r}, args)
	if !res.matched || res.attempts != 1 {
		t.Fatalf("explicit stateIs:waiting should match immediately, got matched=%v attempts=%d", res.matched, res.attempts)
	}
	if r.judgeCalls != 0 {
		t.Fatalf("explicit condition must not consult the finished judge, got %d calls", r.judgeCalls)
	}
}

// clockSeq returns a deterministic clock that yields each value once then clamps to
// the last — seams pollUntil's wall clock so a test can cross extractSettleGraceMS.
func clockSeq(vals ...int64) func() int64 {
	i := 0
	return func() int64 {
		v := vals[i]
		if i < len(vals)-1 {
			i++
		}
		return v
	}
}

// Grace fallback: an agent NEVER observed working (finished between reads, or a
// server that never surfaced "working") still settles once the spawn grace elapses —
// but ONLY because the model confirms it. The grace relaxes the deterministic
// pre-filter, never the "is it done" check.
func TestPollUntil_SettleGraceFallbackConfirms(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "Investigation complete; idle.")}}
	deps, router, args := settlePollArgs(reader, 5)
	// startedAt=0; now reaches 20000 (== grace) on attempt 3, relaxing the pre-filter.
	args.nowFn = clockSeq(0, 0, 10_000, 20_000, 30_000, 40_000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9, Reason: "done"}
	}
	res := pollUntil(context.Background(), deps, args)
	if !res.matched {
		t.Fatalf("never-seen-working agent should settle via grace+confirm once past grace; attempts=%d", res.attempts)
	}
	if res.attempts < 3 {
		t.Fatalf("must DEFER inside the grace window, matched too early on attempt %d", res.attempts)
	}
}

// The grace relaxes the pre-filter but the small model still gates: a never-confirmed
// agent times out even past the grace (no settle on a timer alone).
func TestPollUntil_SettleGraceFallbackStillNeedsConfirm(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "still chugging along…")}}
	deps, router, args := settlePollArgs(reader, 5)
	args.nowFn = clockSeq(0, 0, 10_000, 20_000, 30_000, 40_000, 50_000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "still mid-task"}
	}
	res := pollUntil(context.Background(), deps, args)
	if res.matched {
		t.Fatal("grace must not settle an agent the model never confirms finished")
	}
}

// Short poll budget (maxAttempts * pollIntervalMs < the 20s grace) with an agent
// never observed working: the FINAL attempt bypasses the grace pre-filter so the
// model can still confirm a fast-finishing agent within the budget.
func TestPollUntil_SettleFinalAttemptBypassesGrace(t *testing.T) {
	reader := &seqReader{seq: []StatusReadResult{status("waiting", "Done — wrote the note.")}}
	deps, router, args := settlePollArgs(reader, 3)
	args.nowFn = clockSeq(0, 1, 2, 3) // never crosses the 20s grace
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9}
	}
	res := pollUntil(context.Background(), deps, args)
	if !res.matched || res.attempts != 3 {
		t.Fatalf("never-seen-working agent on a short budget should confirm on the final attempt (grace bypass), got matched=%v attempts=%d", res.matched, res.attempts)
	}
}

// A churning tail must never STARVE a real completion: once a later tail reads as
// done (past the rate-limit cooldown), it is judged and the poll resolves — the
// throttle is a rate limit, not a hard cap.
func TestPollUntil_ChurningTailStillConfirmsWhenDone(t *testing.T) {
	// 4-char tails (== tailBytes) so each is seen whole and they CHURN (distinct hashes).
	reader := &seqReader{seq: []StatusReadResult{
		status("working", "GOGO"),
		status("waiting", "AAAA"),
		status("waiting", "BBBB"),
		status("waiting", "CCCC"),
		status("waiting", "DONE"),
	}}
	deps, router, args := settlePollArgs(reader, 8)
	// Advance well past the 5s cooldown each tick so every distinct tail is judged.
	args.nowFn = clockSeq(0, 1000, 8000, 16000, 24000, 32000, 40000, 48000, 56000)
	router.judgeFn = func(in JudgeInput) domain.ModelJudgeAnswer {
		if strings.Contains(in.Tail, "DONE") {
			return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9}
		}
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9}
	}
	res := pollUntil(context.Background(), deps, args)
	if !res.matched {
		t.Fatal("a completion after several churned not-finished tails must still be confirmed (no starvation)")
	}
}

// resolveBase marks ONLY the coerced wait:{} as the settle default; an explicit,
// equivalent {"stateIs":"waiting"} stays strict (isSettleWait=false).
func TestResolveBase_SettleFlag(t *testing.T) {
	empty, _ := resolveBase(baseArgs{TerminalIDs: []string{"t1"}, Wait: []byte(`{}`)})
	if empty.wait == nil || !empty.isSettleWait {
		t.Fatalf("wait:{} should coerce to the settle default with isSettleWait=true, got %+v", empty.wait)
	}
	explicit, _ := resolveBase(baseArgs{TerminalIDs: []string{"t1"}, Wait: []byte(`{"stateIs":"waiting"}`)})
	if explicit.wait == nil || explicit.isSettleWait {
		t.Fatalf("explicit stateIs:waiting must NOT be treated as the settle default, isSettleWait=%v", explicit.isSettleWait)
	}
}

// A settle wait:{} across multiple terminals is rejected (it would silently time out
// because the aggregate agentState never matches); an explicit content wait is not.
func TestResolveBase_RejectsMultiTerminalSettle(t *testing.T) {
	if _, errMsg := resolveBase(baseArgs{TerminalIDs: []string{"t1", "t2"}, Wait: []byte(`{}`)}); errMsg == "" {
		t.Fatal("wait:{} across multiple terminals should be rejected")
	}
	if _, errMsg := resolveBase(baseArgs{TerminalIDs: []string{"t1", "t2"}, Wait: []byte(`{"contains":"done"}`)}); errMsg != "" {
		t.Fatalf("multi-terminal contains wait should be allowed (matches the combined tail), got %q", errMsg)
	}
	if _, errMsg := resolveBase(baseArgs{TerminalIDs: []string{"t1"}, Wait: []byte(`{}`)}); errMsg != "" {
		t.Fatalf("single-terminal wait:{} must still be allowed, got %q", errMsg)
	}
}
