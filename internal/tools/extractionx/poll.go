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
	}
}

// pollResult is the outcome of pollUntil.
type pollResult struct {
	matched      bool
	attempts     int
	combinedTail string
	finished     bool
}

// pollArgs bundles the poll-loop inputs.
type pollArgs struct {
	terminalIDs    []string
	wait           *domain.WatchCondition
	pollIntervalMs int
	maxAttempts    int
	tailBytes      int
}

// pollUntil reads once, or polls until `wait` is met (or attempts are exhausted).
// Without a `wait` condition it reads a single time and reports matched=true. The
// loop is hard-capped by maxAttempts so a never-satisfied condition cannot hang.
// A cancelled ctx stops polling immediately (reports matched=false).
func pollUntil(ctx context.Context, deps Deps, args pollArgs) pollResult {
	states := make(map[string]*terminalState)
	attempts := 0
	var read *readResult

	for attempts < args.maxAttempts {
		if ctx.Err() != nil {
			break
		}
		attempts++
		r := readSignals(ctx, deps, args.terminalIDs, args.tailBytes, states, time.Now().UnixMilli())
		read = &r
		if args.wait == nil || evaluateCondition(*args.wait, r.signals) {
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
// mid-wait stops the poll loop on its next iteration). Replaces the TS setTimeout
// + unref + abort-listener with a context-aware timer (§4.9).
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
