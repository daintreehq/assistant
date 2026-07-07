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
    "terminalIds": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "maxItems": 16, "uniqueItems": true, "description": "All the agent terminals to wait on — full terminal-<uuid> ids exactly as listed (a unique prefix resolves as a fallback, but never invent or abbreviate ids). Polls their agentState (NO model call, NO output read) and returns when EVERY one has returned to an idle prompt. Each result's status is one of \"finished\" | \"failed\" | \"question\" | \"working\". Use ONE awaitAll for the whole cohort, never one wait per agent. The result also carries top-level stillWorking and askingQuestion arrays of terminal IDs — re-await stillWorking directly (no need to scan perTerminal) and route answers to askingQuestion. AFTER it returns, peek the tail (a no-wait terminal.extract/read) to confirm — a terminal can briefly read 'waiting' while still working; if a 'finished' one still looks busy, re-await or watch it. BOUND the re-await loop: re-await stillWorking IDs at most twice (three awaitAll calls total on the same terminal). After that a still-working terminal is HUNG — escalate (publish a blocked inbox item with queue.publish and attach a watcher with watcher.terminal.create) and end the turn rather than awaiting it again." },
    "pollIntervalMs": { "type": "integer", "minimum": 0, "maximum": 60000, "default": 2000, "description": "Delay between poll rounds in ms." },
    "maxAttempts": { "type": "integer", "minimum": 1, "maximum": 240, "default": 30, "description": "Hard cap on poll rounds (default 30 ≈ 60s, max 240 ≈ 480s — durations assume the default 2s pollIntervalMs). Bounded so it cannot hang. Raise it only for a known-slow cohort whose agents need a single round past 120s — most waits should leave it at the default." }
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
			"ONCE for the whole cohort instead of one wait per agent. Terminals that settle finished/failed automatically RETIRE their " +
			"spawn-attached supervising watcher (watchersRetired in the result) — the completion is in your hands now, so do NOT " +
			"watcher.cancel them yourself and do NOT expect a later completion notification for those agents. IMPORTANT: a bare 'waiting' is an imperfect signal — an agent " +
			"can momentarily read idle while still working. So AFTER awaitAll returns, read each output yourself (a no-wait " +
			"terminal.extract/read of the last few lines) to confirm the result makes sense; if a terminal reported 'finished' but " +
			"its tail shows it is still mid-work, re-await just that one or set a watcher on it and poll. Bound the outer loop: re-await stillWorking IDs at most twice (three awaitAll calls total on the same terminal); after that a still-working terminal is hung — escalate via queue.publish (severity 'blocked') + watcher.terminal.create and end the turn instead of awaiting it again. INTERRUPTIBLE: if the user sends a message while you are waiting, awaitAll returns EARLY with interruptedByUser:true plus whatever has settled so far — their message is already folded into the conversation, so READ IT and adapt (they may want to redirect, e.g. 'that agent errored, re-spawn it') before deciding whether to re-await the stillWorking agents. Read-only; requires Daintree MCP.",
		Risk:   domain.RiskRead,
		Schema: awaitSchema,
		Decode: tools.StrictDecoder(func() any { return &awaitArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a awaitArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			// Per-call copy so the interactive-injection hook (which depends on the
			// live ToolContext) never mutates the startup-shared deps. nil tctx (tests)
			// leaves the hook unset ⇒ the wait is never interrupted.
			d := deps
			if tctx != nil {
				d.InjectionsPending = tctx.InjectionsPending
			}
			// Canonicalize the ids against the live roster ONCE, up front. A truncated/prefix
			// id (the model abbreviates Daintree's full terminal-<uuid> ids) would otherwise
			// match nothing, so the cohort would never settle and the wait would grind to its
			// cap reporting "still working" for agents that already finished. A definitively
			// unknown id now fails loud-and-fast instead.
			resolvedIDs, fail := resolveTerminalIDs(ctx, deps, a.TerminalIDs)
			if fail != nil {
				return *fail
			}
			a.TerminalIDs = resolvedIDs

			pollIntervalMs := intOr(a.PollIntervalMs, 2000)
			maxAttempts := intOr(a.MaxAttempts, 30)

			startedAt := time.Now().UnixMilli()
			outcomes, attempts, interrupted := awaitCohort(ctx, d, a.TerminalIDs, pollIntervalMs, maxAttempts, startedAt, nil)
			elapsedMs := time.Now().UnixMilli() - startedAt

			// The turn just consumed these completions directly, so the spawn-attached
			// supervisor watchers on the DONE terminals are redundant — retire them now,
			// before they re-announce an already-reported completion as a stale attention
			// event. Only finished/failed retire: a question or a still-working straggler
			// leaves its watcher in place (background supervision may still be needed).
			// Deliberate trade-off: a SOFT settle (working→waiting) retires too, even
			// though "waiting" can rarely be a false finish. Those soft settles ARE the
			// clutter case this exists for, and the tool's own guidance already routes
			// the false positive to safety — peek the tail, and if the agent still looks
			// busy re-await it or attach a fresh watcher. Keeping the watcher instead
			// would re-announce EVERY true completion to guard the rare uncertain one.
			retired := 0
			if d.Supervisors != nil {
				for _, id := range a.TerminalIDs {
					if o := outcomes[id]; o != nil && (o.status == domain.SettleStatusFinished || o.status == domain.SettleStatusFailed) {
						retired += d.Supervisors.RetireForTerminal(ctx, id, o.status)
					}
				}
			}

			return buildAwaitResult(a.TerminalIDs, outcomes, attempts, elapsedMs, interrupted, retired)
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
func awaitCohort(ctx context.Context, deps Deps, ids []string, pollIntervalMs, maxAttempts int, startedAt int64, nowFn func() int64) (map[string]*awaitOutcome, int, bool) {
	nowMS := nowFn
	if nowMS == nil {
		nowMS = func() int64 { return time.Now().UnixMilli() }
	}
	term := make(map[string]*awaitTerminal, len(ids))
	for _, id := range ids {
		// Seed the settle gate from the session's cross-call observation memory: an
		// agent this process already watched work (since the last input injection)
		// starts pre-latched, so a RE-await of a since-finished straggler settles on
		// its first poll instead of waiting out the full spawn grace (20s) for
		// evidence a previous await already collected.
		seen := false
		if deps.Observations != nil {
			seen = deps.Observations.SeenWorkingSinceLastCommand(id)
		}
		term[id] = &awaitTerminal{seenWorking: seen}
	}

	attempts := 0
	interrupted := false
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
			// A terminal is "gone" when OTHER terminals came back but not this one (the
			// namespace is confirmed live; a TOTAL miss is a transport hiccup, not exit) —
			// OR when Daintree said so per-entry (a dropped id comes back as a PRESENT
			// entry marked NotFound, never as an omission). The ids were roster-resolved
			// when the wait started, so a mid-wait NotFound is a genuine close — without
			// this the FSM never settles on the entry's empty agentState and the one dead
			// terminal strands the whole cohort until the attempt cap.
			absent := statuses.OK && ((!present && len(statuses.ByID) > 0) || (present && entry.NotFound))
			agentState, waitingReason := "", ""
			var exitCode *int
			if present && !entry.NotFound {
				agentState, waitingReason, exitCode = entry.AgentState, entry.WaitingReason, entry.ExitCode
			}
			if absent {
				agentState = string(domain.AgentExited)
			}
			if agentState == string(domain.AgentWorking) {
				t.seenWorking = true
				if deps.Observations != nil {
					deps.Observations.MarkWorking(id, now)
				}
			}

			v := domain.SettleAgentFSM(agentState, waitingReason, exitCode, t.seenWorking, now-startedAt, domain.FinishSettleGraceMS)
			if !v.Settled {
				continue
			}
			o := &awaitOutcome{status: v.Status, finished: v.Finished, exitCode: exitCode}
			switch {
			case absent:
				// Same wording as the async coordinator's gone outcome — the model must
				// see the terminal vanished (closed/removed) rather than a clean finish.
				o.reason = "terminal is gone (closed or exited)"
			case v.Status == domain.SettleStatusFailed && exitCode != nil:
				o.reason = fmt.Sprintf("exited with code %d", *exitCode)
			case v.Status == domain.SettleStatusQuestion:
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
		// The human typed a message mid-wait. Stop here (don't burn the rest of the
		// budget) and hand control back with whatever has settled so far — the turn
		// loop folds their message in at the next boundary, so they can redirect (e.g.
		// "that agent crashed, re-spawn it") instead of waiting out a multi-minute
		// block. Checked AFTER the settle test so an all-finished tick still reports a
		// clean finish rather than an interruption.
		if deps.InjectionsPending != nil && deps.InjectionsPending() {
			interrupted = true
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
	return out, attempts, interrupted
}

// The pure-FSM settle decision itself is domain.SettleAgentFSM — promoted to
// domain so this in-turn cohort wait and the async coordinator (the out-of-turn
// durable-futures poll) apply ONE settle policy and can never drift. Its
// semantics (question ⇒ settled-not-finished; never-worked pre-start "waiting"
// keeps polling even on the final attempt) are documented there.

// buildAwaitResult folds the per-terminal outcomes into the tool envelope. allFinished
// is true iff every terminal is DONE (finished or failed) — a question or a
// timed-out-still-working terminal makes it false so the caller knows to act.
//
// It also surfaces the two actionable non-finished sets at the TOP LEVEL, as ID-only
// arrays in input order: stillWorking (timed out at the cap) and askingQuestion (blocked
// on the orchestrator). The caller re-awaits stillWorking directly and routes answers to
// askingQuestion without re-scanning perTerminal. Both are non-nil empty slices when there
// are none, so they always serialize as JSON [] (a caller iterating them never hits null).
// retiredSupervisors (>0) is surfaced as watchersRetired so the model knows those
// terminals' spawn-attached watchers are already gone — no watcher.cancel needed, and no
// later completion notification is coming for them.
func buildAwaitResult(ids []string, outcomes map[string]*awaitOutcome, attempts int, elapsedMs int64, interrupted bool, retiredSupervisors int) tools.ToolResult {
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
	agentWord := "agents"
	if total == 1 {
		agentWord = "agent"
	}
	summary := fmt.Sprintf("%s (of %d %s).", strings.Join(parts, ", "), total, agentWord)
	if len(parts) == 0 {
		summary = fmt.Sprintf("No agents (of %d %s).", total, agentWord)
	}
	// An interruption is the headline of the summary: the orchestrator must read the
	// user's new message before doing anything else, so lead with it.
	if interrupted {
		summary = "Paused — you sent a message. " + summary
	}

	result := map[string]any{
		"allFinished":    allFinished,
		"perTerminal":    perTerminal,
		"stillWorking":   stillWorking,
		"askingQuestion": askingQuestion,
		"attempts":       attempts,
		"elapsedMs":      elapsedMs,
		"terminalIds":    ids,
	}
	if retiredSupervisors > 0 {
		// The settled terminals' supervising watchers were retired automatically (their
		// completion is now in-hand). Told explicitly so the model neither cancels them
		// by hand nor waits for a completion notification that will never come.
		result["watchersRetired"] = retiredSupervisors
	}
	if interrupted {
		// The wait stopped early because the human typed — NOT because the budget ran
		// out. The message is already folded into the conversation; the model should
		// read it and adapt (it may redirect the whole plan) before deciding whether to
		// re-await the stillWorking agents.
		result["interruptedByUser"] = true
		result["note"] = "You stopped early because the user sent a message (now folded into the conversation). Read it and adapt before re-awaiting — they may want to change course. The stillWorking agents are still running; re-await them only if their original work is still wanted."
	}
	return tools.Ok(summary, result)
}
