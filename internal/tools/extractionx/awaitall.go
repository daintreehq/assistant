package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// terminal.awaitAll is the IN-TURN cohort finish-wait: the orchestrator spawns
// several agents and calls this ONCE to block (bounded) until every one has
// genuinely finished its turn. It polls all terminals together and judges each
// independently through the SAME finish-detection policy (domain.FinishPreFilter +
// the small-model finished judge) the watcher uses — so a bare "waiting" is never
// mistaken for done. It returns booleans + per-terminal status only (NO content):
// the large model then reads the outputs itself (one terminal.extract over the
// cohort, or a bounded terminal.read), keeping content-judgement on the capable tier
// and finish-detection on the cheap one.
//
// It replaces the N-stacked-waits anti-pattern: terminal.extract wait:{} is
// single-agent, so a cohort needed one waited extract per agent, and those dispatch
// SERIALLY — three 60s timeouts in a row. awaitAll polls the whole cohort at once and
// fans the per-terminal finished judges out concurrently.

const awaitTailBytes = 6_000

type awaitArgs struct {
	TerminalIDs    []string `json:"terminalIds"`
	PollIntervalMs *int     `json:"pollIntervalMs,omitempty"`
	MaxAttempts    *int     `json:"maxAttempts,omitempty"`
}

func (a *awaitArgs) Validate() error {
	if len(a.TerminalIDs) == 0 {
		return fmt.Errorf("terminalIds must have at least 1 entry")
	}
	if len(a.TerminalIDs) > 16 {
		return fmt.Errorf("terminalIds must have at most 16 entries")
	}
	seen := make(map[string]struct{}, len(a.TerminalIDs))
	for _, id := range a.TerminalIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("terminalIds entries must be non-empty")
		}
		// Reject duplicates: the per-terminal poll state dedupes by id, but the result
		// loops the original slice, so a repeated id would be counted more than once.
		if _, dup := seen[id]; dup {
			return fmt.Errorf("terminalIds must not contain duplicates (%q appears more than once)", id)
		}
		seen[id] = struct{}{}
	}
	if a.PollIntervalMs != nil && (*a.PollIntervalMs < 0 || *a.PollIntervalMs > 60_000) {
		return fmt.Errorf("pollIntervalMs must be between 0 and 60000")
	}
	// Capped TIGHTER than terminal.extract (max 60, not 120): awaitAll is an in-turn
	// blocking call, so its worst case is 60 × 2s = 120s — already generous. The whole
	// complaint that started this was a multi-minute hang.
	if a.MaxAttempts != nil && (*a.MaxAttempts < 1 || *a.MaxAttempts > 60) {
		return fmt.Errorf("maxAttempts must be between 1 and 60")
	}
	return nil
}

var awaitSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalIds": { "type": "array", "items": { "type": "string" }, "description": "All the agent terminals to wait on (1-16, no duplicates). Polls them CONCURRENTLY and returns when EVERY one has genuinely finished its turn — confirmed by the small-model finished check, not a bare 'waiting'. Each result's status is one of \"finished\" | \"failed\" | \"question\" | \"working\". Use ONE awaitAll for the whole cohort, never one wait per agent." },
    "pollIntervalMs": { "type": "number", "description": "Delay between poll rounds in ms (default 2000)." },
    "maxAttempts": { "type": "number", "description": "Hard cap on poll rounds (default 30 ≈ 60s, max 60 ≈ 120s). Bounded so it cannot hang." }
  },
  "required": ["terminalIds"]
}`)

func newAwaitAllTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.awaitAll",
		Description: "Wait (bounded) for a COHORT of agent terminals to all finish their turn, polling them concurrently and " +
			"confirming each with the small-model finished check (not a bare 'waiting'). Returns allFinished plus a perTerminal " +
			"array whose status is exactly one of \"finished\" | \"failed\" | \"question\" | \"working\" — and NO content; read " +
			"outputs yourself afterward with terminal.extract or terminal.read. Use this ONCE for the whole cohort instead of " +
			"one wait per agent. Read-only; requires Daintree MCP.",
		Risk:   domain.RiskRead,
		Schema: awaitSchema,
		Decode: tools.StrictDecoder(func() any { return &awaitArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a awaitArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			pollIntervalMs := intOr(a.PollIntervalMs, 2000)
			maxAttempts := intOr(a.MaxAttempts, 30)

			startedAt := time.Now().UnixMilli()
			outcomes, attempts := awaitCohort(ctx, deps, a.TerminalIDs, pollIntervalMs, maxAttempts, startedAt, nil)
			elapsedMs := time.Now().UnixMilli() - startedAt

			return buildAwaitResult(a.TerminalIDs, outcomes, attempts, elapsedMs)
		},
	}
}

// awaitOutcome is one terminal's settled-for-await verdict (nil until settled).
type awaitOutcome struct {
	status   string // "finished" | "failed" | "question"
	finished bool
	exitCode *int
	reason   string
}

// awaitTerminal is the per-terminal poll memory for a cohort wait.
type awaitTerminal struct {
	state     *terminalState
	lastJudge int64
	outcome   *awaitOutcome
}

// awaitCohort runs the per-terminal poll loop. It returns once every terminal has
// SETTLED for the await (finished / failed / asking a question) or the attempt cap is
// hit. nowFn seams the clock for tests (nil ⇒ time.Now). Each terminal is judged
// INDEPENDENTLY and the per-tick judge calls fan out concurrently, so one slow agent
// never serializes the others.
func awaitCohort(ctx context.Context, deps Deps, ids []string, pollIntervalMs, maxAttempts int, startedAt int64, nowFn func() int64) (map[string]*awaitOutcome, int) {
	nowMS := nowFn
	if nowMS == nil {
		nowMS = func() int64 { return time.Now().UnixMilli() }
	}
	term := make(map[string]*awaitTerminal, len(ids))
	for _, id := range ids {
		term[id] = &awaitTerminal{}
	}

	attempts := 0
	for attempts < maxAttempts {
		if ctx.Err() != nil {
			break
		}
		attempts++
		now := nowMS()
		isFinal := attempts >= maxAttempts
		statuses := deps.Reader.ReadStatuses(ctx, ids, true)

		type pending struct {
			id   string
			tail string
			sig  signals
		}
		var toJudge []pending

		for _, id := range ids {
			t := term[id]
			if t.outcome != nil {
				continue // already settled — never re-poll
			}
			entry, present := statuses.ByID[id]
			// A terminal is "gone" only when OTHER terminals came back but not this one
			// (the namespace is confirmed live). A TOTAL miss is a transport hiccup, not exit.
			absent := statuses.OK && !present && len(statuses.ByID) > 0
			agentState, waitingReason := "", ""
			var exitCode *int
			if present {
				agentState, waitingReason, exitCode = entry.AgentState, entry.WaitingReason, entry.ExitCode
			}
			if absent {
				agentState = "exited"
			}

			tail := awaitReadTail(ctx, deps, id, entry, present, absent)
			st, ms := nextOutputState(t.state, tail, now)
			if agentState == string(domain.AgentWorking) {
				st.seenWorking = true
			}
			if t.state != nil && st.outHash != t.state.outHash && strings.TrimSpace(tail) != "" {
				st.seenWorking = true
			}
			t.state = &st

			// A question blocks on the human/orchestrator — settle it as needs-attention so
			// the caller can answer it, never wait it out to the cap.
			if agentState == string(domain.AgentWaiting) && waitingReason == "question" {
				t.outcome = &awaitOutcome{status: "question", finished: false, reason: "asking a question"}
				continue
			}

			lastJudgeAge := int64(0)
			if t.lastJudge != 0 {
				lastJudgeAge = now - t.lastJudge
			}
			shouldJudge, terminalAccept := domain.FinishPreFilter(domain.FinishPreFilterInput{
				AgentState:       agentState,
				WaitingReason:    waitingReason,
				SeenWorking:      st.seenWorking,
				MsSinceSpawn:     now - startedAt,
				MsSinceOutput:    ms,
				MsSinceLastJudge: lastJudgeAge,
				CooldownMS:       domain.SettleFinishCooldownMS,
				GraceMS:          domain.FinishSettleGraceMS,
				QuietMS:          domain.FinishQuietThresholdMS,
				IsFinalAttempt:   isFinal,
				TailEmpty:        strings.TrimSpace(tail) == "",
			})
			if terminalAccept {
				// completed (success) or exited (success unless nonzero code). An absent
				// terminal is reported exited with an unknown code.
				o := &awaitOutcome{status: "finished", finished: true, exitCode: exitCode}
				if agentState == string(domain.AgentExited) && exitCode != nil && *exitCode != 0 {
					o.status, o.finished = "failed", true
					o.reason = fmt.Sprintf("exited with code %d", *exitCode)
				}
				t.outcome = o
				continue
			}
			if shouldJudge {
				toJudge = append(toJudge, pending{id: id, tail: tail, sig: signals{
					AgentState:    agentState,
					WaitingReason: waitingReason,
					RuntimeStatus: runtimeFromAgentState(agentState),
					Tail:          tail,
					MsSinceOutput: ms,
				}})
			}
		}

		// Fan the per-terminal finished judges out concurrently — the whole point of a
		// cohort wait. Each writes only its own terminal's fields, under the mutex.
		if len(toJudge) > 0 {
			var wg sync.WaitGroup
			var mu sync.Mutex
			for _, p := range toJudge {
				wg.Add(1)
				go func(p pending) {
					defer wg.Done()
					fin, _ := confirmFinished(ctx, deps, &readResult{combinedTail: p.tail, signals: p.sig})
					mu.Lock()
					t := term[p.id]
					t.lastJudge = now
					if fin {
						t.outcome = &awaitOutcome{status: "finished", finished: true}
					}
					mu.Unlock()
				}(p)
			}
			wg.Wait()
		}

		settled := true
		for _, id := range ids {
			if term[id].outcome == nil {
				settled = false
				break
			}
		}
		if settled {
			break
		}
		if !isFinal && pollIntervalMs > 0 {
			delay(ctx, pollIntervalMs)
		}
	}

	out := make(map[string]*awaitOutcome, len(ids))
	for _, id := range ids {
		out[id] = term[id].outcome
	}
	return out, attempts
}

// awaitReadTail returns the most recent bounded tail for one terminal. Finish
// DETECTION only needs the recent tail, so it PREFERS the inline recentOutput already
// fetched by the batched ReadStatuses(includeOutput) — keeping each poll tick to that
// one batched call rather than a serial deep terminal.getOutput per terminal (which,
// at the MCP client's default per-call timeout, could otherwise stretch a tick's
// wall-clock). It falls back to a deep read when the batched read carried no USABLE
// inline output — recentOutput nil OR whitespace-only. An absent terminal has no tail.
func awaitReadTail(ctx context.Context, deps Deps, id string, entry TerminalStatusEntry, present, absent bool) string {
	if absent {
		return ""
	}
	// Use the inline tail ONLY when it actually carries content. A present-but-blank
	// recentOutput is a real failure mode, not "no output": some agent TUIs bottom-pad
	// their viewport (Codex parks its composer high and fills the rest of the screen with
	// blank lines), so the screen-grid grab Daintree returns inline is all whitespace even
	// though the agent has finished and its answer sits just ABOVE the window. Trusting
	// that blank tail makes finish-detection see TailEmpty and never judge the agent — it
	// then rides the wait to the attempt cap as "still working" (the exact awaitAll hang
	// this fixes). So when the inline tail is whitespace-only, fall back to the deep
	// terminal.getOutput read, which returns output history (the real content).
	if present && entry.RecentOutput != nil && strings.TrimSpace(*entry.RecentOutput) != "" {
		return lastRunes(*entry.RecentOutput, awaitTailBytes)
	}
	read := deps.Reader.ReadOutput(ctx, id, awaitTailBytes)
	if read.OK {
		return read.Value
	}
	// Deep read failed (transport hiccup) — fall back to whatever inline we held, even if
	// blank, rather than losing a tail we already had.
	if present && entry.RecentOutput != nil {
		return lastRunes(*entry.RecentOutput, awaitTailBytes)
	}
	return ""
}

// buildAwaitResult folds the per-terminal outcomes into the tool envelope. allFinished
// is true iff every terminal is DONE (finished or failed) — a question or a
// timed-out-still-working terminal makes it false so the caller knows to act.
func buildAwaitResult(ids []string, outcomes map[string]*awaitOutcome, attempts int, elapsedMs int64) tools.ToolResult {
	perTerminal := make([]map[string]any, 0, len(ids))
	var okCount, failCount, questionCount, workingCount int
	allFinished := true

	for _, id := range ids {
		o := outcomes[id]
		if o == nil {
			allFinished = false
			workingCount++
			perTerminal = append(perTerminal, map[string]any{
				"terminalId": id, "status": "working", "finished": false,
				"reason": "still working when the wait budget ran out",
			})
			continue
		}
		entry := map[string]any{"terminalId": id, "status": o.status, "finished": o.finished}
		if o.exitCode != nil {
			entry["exitCode"] = *o.exitCode
		}
		if o.reason != "" {
			entry["reason"] = o.reason
		}
		perTerminal = append(perTerminal, entry)
		switch o.status {
		case "finished":
			okCount++
		case "failed":
			failCount++
		case "question":
			questionCount++
			allFinished = false
		}
	}

	total := len(ids)
	parts := make([]string, 0, 4)
	if okCount > 0 {
		parts = append(parts, fmt.Sprintf("%d finished", okCount))
	}
	if failCount > 0 {
		parts = append(parts, fmt.Sprintf("%d failed", failCount))
	}
	if questionCount > 0 {
		parts = append(parts, fmt.Sprintf("%d asking a question", questionCount))
	}
	if workingCount > 0 {
		parts = append(parts, fmt.Sprintf("%d still working", workingCount))
	}
	summary := fmt.Sprintf("%s (of %d agent(s)).", strings.Join(parts, ", "), total)
	if len(parts) == 0 {
		summary = fmt.Sprintf("No agents (of %d).", total)
	}

	return tools.Ok(summary, map[string]any{
		"allFinished": allFinished,
		"perTerminal": perTerminal,
		"attempts":    attempts,
		"elapsedMs":   elapsedMs,
		"terminalIds": ids,
	})
}
