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
		terminalID    string
		tail          string
		agentState    string
		waitingReason string
		exitCode      *int
	}
	parts := make([]part, 0, len(terminalIDs))

	for _, id := range terminalIDs {
		entry, present := statuses.ByID[id]
		// A terminal is "gone" only when the read returned OTHER terminals but not
		// this one (the namespace is confirmed live, so the omission is a real exit).
		// A TOTAL miss is the #108 symptom, NOT a clean exit.
		absent := statuses.OK && !present && len(statuses.ByID) > 0
		agentState := ""
		waitingReason := ""
		if present {
			agentState = entry.AgentState
			waitingReason = entry.WaitingReason
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
		case entry.RecentOutput != nil && len([]rune(*entry.RecentOutput)) >= tailBytes && strings.TrimSpace(*entry.RecentOutput) != "":
			// The inline tail already covers the requested window AND carries content — use
			// it. A blank inline (even one long enough to cover the window — e.g. a fully
			// blank-padded screen grab) must NOT short-circuit the deep read, or a finished
			// agent with a bottom-padded TUI reads as empty and never settles. Short inline
			// tails already fall through to the deep read below.
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
			// Also latch when the tail ADVANCES from a prior baseline: output moving
			// proves the agent did work even if no poll caught the live "working" instant
			// (a fast agent that ran between two reads). This closes the round-2 race
			// where a relayed agent went working→waiting before the poll started, so a
			// bare seenWorking latch alone would never fire and the wait would stall to
			// timeout. (prev!=nil so the first read's baseline never counts as "advanced".)
			if prev != nil && out.outHash != prev.outHash && strings.TrimSpace(tail) != "" {
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
		parts = append(parts, part{terminalID: id, tail: tail, agentState: agentState, waitingReason: waitingReason, exitCode: ec})
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
	var waitingReason string
	var exitCode *int
	runtime := "running"
	if len(terminalIDs) == 1 {
		agentState = parts[0].agentState
		waitingReason = parts[0].waitingReason
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
			WaitingReason: waitingReason,
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
	// exitCode is the final read's single-terminal exit code (nil for multi-terminal
	// polls, where readSignals leaves the aggregate blank, and while the process is
	// alive). Carried so a consumed completion can be classified failed on a nonzero
	// exit instead of blindly reported finished (retireConsumedSupervisors).
	exitCode *int
}

// The settle-wait timing knobs (spawn grace, judge cooldown, quiet threshold,
// confidence floor) are the shared domain.Finish* constants so this in-turn poll and
// the background watcher apply ONE finish-detection policy (see domain.FinishPreFilter).

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
	// lastJudgeAt rate-limits the finished confirmation (domain.SettleFinishCooldownMS)
	// so a churning tail can't judge on every poll. There is deliberately NO permanent
	// tail-hash latch: the finished judge's verdict can flip NO→YES as the agent goes
	// quiet (its lastOutputAt input keeps growing while the bytes stay fixed), so a
	// hash latch would strand a genuinely-finished-but-static agent until timeout —
	// the exact divergence that made the watcher confirm done while this poll hung.
	var lastJudgeAt int64

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
			// Coerced settle wait: a deterministic match is NOT enough. Defer to the
			// shared finish policy (domain.FinishPreFilter): completed/exited are hard
			// terminal facts accepted without a model call; a bare "waiting" is resolved
			// by the small-model finished judge on the tail (authoritative over the
			// unreliable "waiting" proxy). This is byte-for-byte the watcher's policy, so
			// the two paths can no longer disagree.
			lastJudgeAge := int64(0)
			if lastJudgeAt != 0 {
				lastJudgeAge = now - lastJudgeAt
			}
			shouldJudge, terminalAccept := domain.FinishPreFilter(domain.FinishPreFilterInput{
				AgentState:       r.signals.AgentState,
				WaitingReason:    r.signals.WaitingReason,
				SeenWorking:      r.seenWorking,
				MsSinceSpawn:     now - startedAt,
				MsSinceOutput:    r.signals.MsSinceOutput,
				MsSinceLastJudge: lastJudgeAge,
				CooldownMS:       domain.SettleFinishCooldownMS,
				GraceMS:          domain.FinishSettleGraceMS,
				QuietMS:          domain.FinishQuietThresholdMS,
				IsFinalAttempt:   attempts >= args.maxAttempts,
				TailEmpty:        strings.TrimSpace(r.combinedTail) == "",
			})
			if terminalAccept {
				return true
			}
			if !shouldJudge {
				return false
			}
			fin, _ := confirmFinished(ctx, deps, &r)
			lastJudgeAt = now
			return fin
		}()
		if matched {
			return pollResult{matched: true, attempts: attempts, combinedTail: r.combinedTail,
				finished: r.finished, exitCode: r.signals.ExitCode}
		}

		if attempts < args.maxAttempts && args.pollIntervalMs > 0 {
			delay(ctx, args.pollIntervalMs)
		}
	}

	res := pollResult{matched: false, attempts: attempts}
	if read != nil {
		res.combinedTail = read.combinedTail
		res.finished = read.finished
		res.exitCode = read.signals.ExitCode
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
