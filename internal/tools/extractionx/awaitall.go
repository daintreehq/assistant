package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// terminal.awaitAll is the IN-TURN cohort finish-wait: the orchestrator spawns
// several agents and calls this ONCE to block (bounded) until every one has
// returned to an idle prompt. It is PURE FSM — it polls only the cheap agentState
// (terminal.getStatus) and settles each terminal on the state machine ALONE:
//   - completed / exited       → finished (failed if a nonzero exit code);
//   - waiting + question       → settled as needs-attention (never "finished");
//   - waiting (other) after a   → finished — a working→waiting transition means the
//     working→waiting transition  turn is done; if the working tick was never caught
//     (or a stable idle past grace) (a fast agent), a stable idle past the spawn grace also settles.
//
// It makes NO model call and reads NO terminal output — so it is fast, light, and
// can't trip Daintree's per-tool getOutput throttle by bursting deep reads. It
// returns booleans + per-terminal status only (NO content): the large model then
// reads the outputs itself (one terminal.extract over the cohort, or a bounded
// terminal.read), keeping content-judgement on the capable tier.
//
// CAVEAT the orchestrator must own: a bare "waiting" is an imperfect proxy — an agent
// can momentarily read "waiting" while still mid-work (a backgrounded window, a paused
// step). awaitAll deliberately does NOT pay a per-tick model judge to chase that;
// instead the large model SELF-HEALS after the wait — it peeks the last few lines
// (a no-wait terminal.extract/read), and if a "finished" terminal still looks busy it
// re-polls/awaits or sets a watcher on that one. The tool Description tells it so.
//
// It replaces the N-stacked-waits anti-pattern: terminal.extract wait:{} is
// single-agent, so a cohort needed one waited extract per agent, and those dispatch
// SERIALLY — three 60s timeouts in a row. awaitAll polls the whole cohort at once.

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
	// Default stays small (30 ≈ 60s) — most cohorts settle fast and a tight budget keeps a
	// stuck wait from hanging the turn. The ceiling is OPT-IN headroom: a known-slow agent
	// can need a single round past 240s, which the old max of 60 (120s) could never cover,
	// forcing a re-await churn. 240 × 2s = 480s gives one wait enough rope for the slow case
	// without going unbounded. Still pure FSM — a higher cap is just more cheap getStatus ticks.
	if a.MaxAttempts != nil && (*a.MaxAttempts < 1 || *a.MaxAttempts > 240) {
		return fmt.Errorf("maxAttempts must be between 1 and 240")
	}
	return nil
}

var awaitSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalIds": { "type": "array", "items": { "type": "string" }, "description": "All the agent terminals to wait on (1-16, no duplicates). Polls their agentState (NO model call, NO output read) and returns when EVERY one has returned to an idle prompt. Each result's status is one of \"finished\" | \"failed\" | \"question\" | \"working\". Use ONE awaitAll for the whole cohort, never one wait per agent. The result also carries top-level stillWorking and askingQuestion arrays of terminal IDs — re-await stillWorking directly (no need to scan perTerminal) and route answers to askingQuestion. AFTER it returns, peek the tail (a no-wait terminal.extract/read) to confirm — a terminal can briefly read 'waiting' while still working; if a 'finished' one still looks busy, re-await or watch it." },
    "pollIntervalMs": { "type": "number", "description": "Delay between poll rounds in ms (default 2000)." },
    "maxAttempts": { "type": "number", "description": "Hard cap on poll rounds (default 30 ≈ 60s, max 240 ≈ 480s — durations assume the default 2s pollIntervalMs). Bounded so it cannot hang. Raise it only for a known-slow cohort whose agents need a single round past 120s — most waits should leave it at the default." }
  },
  "required": ["terminalIds"]
}`)

func newAwaitAllTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.awaitAll",
		Description: "Wait (bounded) for a COHORT of agent terminals to all return to an idle prompt. Pure state-machine: it polls " +
			"agentState only — NO model call, NO output read — so it is fast and light. Returns allFinished plus a perTerminal " +
			"array whose status is exactly one of \"finished\" | \"failed\" | \"question\" | \"working\" — and NO content. It also " +
			"returns top-level stillWorking and askingQuestion arrays of terminal IDs, so when the wait budget runs out you can " +
			"re-await exactly the stillWorking stragglers (and route answers to askingQuestion) without scanning perTerminal. Use this " +
			"ONCE for the whole cohort instead of one wait per agent. IMPORTANT: a bare 'waiting' is an imperfect signal — an agent " +
			"can momentarily read idle while still working. So AFTER awaitAll returns, read each output yourself (a no-wait " +
			"terminal.extract/read of the last few lines) to confirm the result makes sense; if a terminal reported 'finished' but " +
			"its tail shows it is still mid-work, re-await just that one or set a watcher on it and poll. Read-only; requires Daintree MCP.",
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

// awaitTerminal is the per-terminal poll memory for a cohort wait: whether we've
// observed it WORKING yet (so a later "waiting" is a genuine working→idle transition,
// not a never-started pre-start prompt) and its settled outcome (nil until settled).
type awaitTerminal struct {
	seenWorking bool
	outcome     *awaitOutcome
}

// awaitCohort runs the pure-FSM poll loop. It returns once every terminal has SETTLED
// (finished / failed / asking a question) or the attempt cap is hit. nowFn seams the
// clock for tests (nil ⇒ time.Now). Each terminal settles INDEPENDENTLY from its
// agentState alone — no tail read, no model call — so a tick is one cheap getStatus.
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
		// FSM only — includeOutput=false keeps the read cheap and never triggers a deep
		// getOutput fallback (the burst that tripped Daintree's per-tool throttle).
		statuses := deps.Reader.ReadStatuses(ctx, ids, false)

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
				agentState = string(domain.AgentExited)
			}
			if agentState == string(domain.AgentWorking) {
				t.seenWorking = true
			}

			v := awaitSettleFSM(agentState, waitingReason, exitCode, t.seenWorking, now-startedAt, domain.FinishSettleGraceMS)
			if !v.settled {
				continue
			}
			o := &awaitOutcome{status: v.status, finished: v.finished, exitCode: exitCode}
			switch v.status {
			case "failed":
				if exitCode != nil {
					o.reason = fmt.Sprintf("exited with code %d", *exitCode)
				}
			case "question":
				o.reason = "asking a question"
			}
			t.outcome = o
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

// awaitFSMVerdict is the pure-FSM settle decision for one terminal at one tick.
type awaitFSMVerdict struct {
	settled  bool
	status   string // "finished" | "failed" | "question" — valid only when settled
	finished bool
}

// awaitSettleFSM decides a terminal's await verdict from its FSM state ALONE — no tail,
// no model call. completed/exited are hard terminal facts (exited is "failed" on a
// nonzero code). A "waiting" agent is the only soft case: a question blocks on the
// orchestrator (settled, NOT finished); any other "waiting" is finished IF we caught it
// working first (a real working→idle transition) OR a stable idle outlasted the spawn
// grace (a fast agent we never caught mid-work). A never-worked "waiting" before the
// grace is a possible pre-start prompt — NOT settled, even on the final attempt: with
// zero positive evidence the agent ever did anything, "still working" (allFinished=false,
// which steers the caller to read the tail and self-heal) is more honest than a forced
// "finished" that a tiny maxAttempts budget could otherwise fabricate. working/idle/
// directing/unknown never settle either (reported via the caller's nil-outcome path).
func awaitSettleFSM(agentState, waitingReason string, exitCode *int, seenWorking bool, msSinceSpawn, graceMS int64) awaitFSMVerdict {
	switch agentState {
	case string(domain.AgentCompleted):
		return awaitFSMVerdict{settled: true, status: "finished", finished: true}
	case string(domain.AgentExited):
		if exitCode != nil && *exitCode != 0 {
			return awaitFSMVerdict{settled: true, status: "failed", finished: true}
		}
		return awaitFSMVerdict{settled: true, status: "finished", finished: true}
	case string(domain.AgentWaiting):
		if waitingReason == "question" {
			return awaitFSMVerdict{settled: true, status: "question", finished: false}
		}
		if seenWorking || msSinceSpawn >= graceMS {
			return awaitFSMVerdict{settled: true, status: "finished", finished: true}
		}
		return awaitFSMVerdict{} // never-worked pre-start 'waiting' before grace — keep polling
	default:
		return awaitFSMVerdict{}
	}
}

// buildAwaitResult folds the per-terminal outcomes into the tool envelope. allFinished
// is true iff every terminal is DONE (finished or failed) — a question or a
// timed-out-still-working terminal makes it false so the caller knows to act.
//
// It also surfaces the two actionable non-finished sets at the TOP LEVEL, as ID-only
// arrays in input order: stillWorking (timed out at the cap) and askingQuestion (blocked
// on the orchestrator). The caller re-awaits stillWorking directly and routes answers to
// askingQuestion without re-scanning perTerminal. Both are non-nil empty slices when there
// are none, so they always serialize as JSON [] (a caller iterating them never hits null).
func buildAwaitResult(ids []string, outcomes map[string]*awaitOutcome, attempts int, elapsedMs int64) tools.ToolResult {
	perTerminal := make([]map[string]any, 0, len(ids))
	stillWorking := make([]string, 0, len(ids))
	askingQuestion := make([]string, 0, len(ids))
	var okCount, failCount, questionCount, workingCount int
	allFinished := true

	for _, id := range ids {
		o := outcomes[id]
		if o == nil {
			allFinished = false
			workingCount++
			stillWorking = append(stillWorking, id)
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
			askingQuestion = append(askingQuestion, id)
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
		"allFinished":    allFinished,
		"perTerminal":    perTerminal,
		"stillWorking":   stillWorking,
		"askingQuestion": askingQuestion,
		"attempts":       attempts,
		"elapsedMs":      elapsedMs,
		"terminalIds":    ids,
	})
}
