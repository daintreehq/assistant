package extractionx

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// readResult is one read across all target terminals folded into a single
// aggregate signal.
type readResult struct {
	signals      signals
	combinedTail string // terminal-labelled tail fed to the model
	finished     bool   // every target terminal exited (or is gone)
	seenWorking  bool   // every target terminal has been observed working at least once
}

// readSignals performs one read across all target terminals, folded into a single
// aggregate signal:
//   - tail: every terminal's tail concatenated UNLABELLED (so contains/regex match
//     real output, not the [id] headers);
//   - runtimeStatus "exited" only when ALL terminals exited/are gone;
//   - msSinceOutput the MIN across terminals (the most recently active one);
//   - agentState the single terminal's state (only meaningful for one target).
//
// Mutates states with the per-terminal output-tracking memory for noOutputForMs.
// A failed deep read must NOT advance noOutputForMs (a transport hiccup is not
// silence) — preserve prior state and skip its contribution.
func readSignals(ctx context.Context, deps Deps, terminalIDs []string, tailBytes int, states map[string]*terminalState, now int64) readResult {
	statuses := deps.Reader.ReadStatuses(ctx, terminalIDs, true)
	// "All exited" requires a successful read that ACTUALLY returned terminals. A
	// total miss (ok:true, empty byId) is the #108 empty-read symptom, NOT a clean
	// exit — require ByID non-empty before trusting exit.
	allExited := statuses.OK && len(statuses.ByID) > 0
	minMsSinceOutput := math.MaxInt64

	type part struct {
		terminalID string
		tail       string
		agentState string
		exitCode   *int
	}
	parts := make([]part, 0, len(terminalIDs))

	for _, id := range terminalIDs {
		entry, present := statuses.ByID[id]
		// A terminal is "gone" only when the read returned OTHER terminals but not
		// this one (the namespace is confirmed live, so the omission is a real exit).
		// A TOTAL miss is the #108 symptom, NOT a clean exit.
		absent := statuses.OK && !present && len(statuses.ByID) > 0
		agentState := ""
		if present {
			agentState = entry.AgentState
		}
		if absent {
			agentState = "exited"
		}

		prev := states[id]
		var tail string
		readFailed := false
		switch {
		case absent:
			tail = ""
		case entry.RecentOutput != nil && len([]rune(*entry.RecentOutput)) >= tailBytes:
			// The inline tail already covers the requested window — use it.
			tail = lastRunes(*entry.RecentOutput, tailBytes)
		default:
			read := deps.Reader.ReadOutput(ctx, id, tailBytes)
			readFailed = !read.OK
			if read.OK {
				tail = read.Value
			} else if entry.RecentOutput != nil {
				tail = *entry.RecentOutput
			} else {
				tail = ""
			}
		}

		if readFailed {
			// Preserve prior state; skip this terminal's noOutputForMs contribution.
			if prev != nil {
				states[id] = prev
			}
		} else {
			out, ms := nextOutputState(prev, tail, now)
			// Latch the settle gate the first time this agent is seen working — a
			// "waiting" that follows working is a real settle; a "waiting" before it has
			// ever worked is the pre-start prompt (or a backgrounded window) and must NOT
			// end the poll.
			if agentState == string(domain.AgentWorking) {
				out.seenWorking = true
			}
			states[id] = &out
			if ms < int64(minMsSinceOutput) {
				minMsSinceOutput = int(ms)
			}
		}
		if agentState != "exited" {
			allExited = false
		}
		var ec *int
		if present {
			ec = entry.ExitCode
		}
		parts = append(parts, part{terminalID: id, tail: tail, agentState: agentState, exitCode: ec})
	}

	// Labelled tail goes to the model; the raw, unlabelled tail drives contains/regex.
	labelled := make([]string, len(parts))
	raw := make([]string, len(parts))
	for i, p := range parts {
		if len(terminalIDs) > 1 {
			labelled[i] = fmt.Sprintf("[%s]\n%s", p.terminalID, p.tail)
		} else {
			labelled[i] = p.tail
		}
		raw[i] = p.tail
	}

	var agentState string
	var exitCode *int
	runtime := "running"
	if len(terminalIDs) == 1 {
		agentState = parts[0].agentState
		exitCode = parts[0].exitCode
		runtime = runtimeFromAgentState(parts[0].agentState)
	}
	if allExited {
		runtime = "exited"
	}
	ms := int64(0)
	if minMsSinceOutput != math.MaxInt64 {
		ms = int64(minMsSinceOutput)
	}

	// Aggregate the settle latch: only honor a settle once EVERY target has been seen
	// working at least once (a single not-yet-started target keeps the poll going).
	seenWorkingAll := len(terminalIDs) > 0
	for _, id := range terminalIDs {
		if st := states[id]; st == nil || !st.seenWorking {
			seenWorkingAll = false
			break
		}
	}

	return readResult{
		signals: signals{
			AgentState:    agentState,
			RuntimeStatus: runtime,
			ExitCode:      exitCode,
			Tail:          strings.Join(raw, "\n\n"),
			MsSinceOutput: ms,
		},
		combinedTail: strings.Join(labelled, "\n\n"),
		finished:     allExited,
		seenWorking:  seenWorkingAll,
	}
}

// pollResult is the outcome of pollUntil.
type pollResult struct {
	matched      bool
	attempts     int
	combinedTail string
	finished     bool
}

// extractSettleGraceMS mirrors the watcher's spawn grace: if a working tick is
// never observed (an agent that finished between two reads, or a server that never
// surfaced "working"), accept a stable settle after this long so the poll can't
// stall until maxAttempts waiting for a transition it already missed. The model
// finished-confirmation still gates acceptance, so the grace only relaxes the
// deterministic pre-filter, never the "is it actually done" check.
const extractSettleGraceMS = 20_000

// maxSettleJudgeCalls bounds the finished confirmation per poll so a tail that
// repaints every tick (defeating the hash dedupe) cannot burn one small-model call
// on every attempt.
const maxSettleJudgeCalls = 6

// pollArgs bundles the poll-loop inputs.
type pollArgs struct {
	terminalIDs    []string
	wait           *domain.WatchCondition
	pollIntervalMs int
	maxAttempts    int
	tailBytes      int
	// nowFn seams the wall clock for tests (the settle grace is time-based). nil ⇒
	// time.Now().UnixMilli. Production never sets it.
	nowFn func() int64
	// isSettleWait is set ONLY when wait is the coerced wait:{} settled default
	// (waiting/completed/exited). For that case a deterministic match is NOT enough:
	// a "waiting" agent must also pass the seenWorking/grace pre-filter and a
	// small-model finished confirmation before the poll resolves. Explicit conditions
	// (contains/regex/noOutputForMs/an explicit stateIs) stay strict and model-free.
	isSettleWait bool
}

// pollUntil reads once, or polls until `wait` is met (or attempts are exhausted).
// Without a `wait` condition it reads a single time and reports matched=true. The
// loop is hard-capped by maxAttempts so a never-satisfied condition cannot hang.
// A cancelled ctx stops polling immediately (reports matched=false).
//
// For the coerced settle wait (isSettleWait), a deterministic "waiting" match does
// NOT resolve the poll on its own — `waiting` is an unreliable proxy for "finished"
// (an agent flips to waiting when paused mid-task or when its window is
// backgrounded). The settle is honored only when the agent has been seen working
// (or the spawn grace elapsed) AND the small model confirms, on the tail, that it
// genuinely finished. completed/exited are authoritative and accepted immediately.
func pollUntil(ctx context.Context, deps Deps, args pollArgs) pollResult {
	states := make(map[string]*terminalState)
	attempts := 0
	var read *readResult
	nowMS := args.nowFn
	if nowMS == nil {
		nowMS = func() int64 { return time.Now().UnixMilli() }
	}
	startedAt := nowMS()
	// lastJudgedHash dedupes the finished confirmation: a temperature-0 judge gives
	// the same verdict for an unchanged tail, so we don't re-ask about a tail we
	// already got a CONFIDENT not-finished on. A changed tail clears it implicitly.
	var lastJudgedHash string
	// judgeCalls caps the finished confirmation so a CHURNING tail (which busts the
	// hash dedupe every tick) can't burn a model call on every one of up to
	// maxAttempts (120) polls. Past the cap the settle simply times out — the agent
	// wasn't confirmed done, which is the correct, safe outcome.
	judgeCalls := 0

	for attempts < args.maxAttempts {
		if ctx.Err() != nil {
			break
		}
		attempts++
		now := nowMS()
		r := readSignals(ctx, deps, args.terminalIDs, args.tailBytes, states, now)
		read = &r

		matched := func() bool {
			if args.wait == nil {
				return true // read-once
			}
			if !evaluateCondition(*args.wait, r.signals) {
				return false
			}
			if !args.isSettleWait {
				return true // explicit condition: a deterministic match is authoritative
			}
			// Coerced settle wait. completed/exited are authoritative terminal states —
			// accept immediately. Only the soft "waiting" needs confirmation.
			if r.signals.AgentState != string(domain.AgentWaiting) {
				return true
			}
			// Pre-filter: the agent must have been seen working (or the grace elapsed)
			// so a pre-start prompt / backgrounded window doesn't read as done. On the
			// FINAL attempt the grace is bypassed so a short poll budget (maxAttempts *
			// pollIntervalMs < the 20s grace) can still confirm a fast-finishing agent
			// that was never observed "working" — the model confirmation still gates it.
			if !r.seenWorking && now-startedAt < extractSettleGraceMS && attempts < args.maxAttempts {
				return false
			}
			h := hashTail(r.combinedTail)
			if h == lastJudgedHash {
				return false // already got a confident not-finished on this exact tail
			}
			if judgeCalls >= maxSettleJudgeCalls {
				return false // churn cap reached — stop re-judging, let the poll time out
			}
			fin, confident := confirmFinished(ctx, deps, &r)
			judgeCalls++
			if fin {
				return true
			}
			if confident {
				lastJudgedHash = h // confident NO — skip re-judging an identical tail
			}
			return false
		}()
		if matched {
			return pollResult{matched: true, attempts: attempts, combinedTail: r.combinedTail, finished: r.finished}
		}

		if attempts < args.maxAttempts && args.pollIntervalMs > 0 {
			delay(ctx, args.pollIntervalMs)
		}
	}

	res := pollResult{matched: false, attempts: attempts}
	if read != nil {
		res.combinedTail = read.combinedTail
		res.finished = read.finished
	}
	return res
}

// delay sleeps ms, resolving early when ctx is cancelled (Escape-to-cancel
// mid-wait stops the poll loop on its next iteration). A context-aware timer.
func delay(ctx context.Context, ms int) {
	t := time.NewTimer(time.Duration(ms) * time.Millisecond)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// lastRunes returns the last n runes of s (matching .slice(-n) on a JS string).
func lastRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}
