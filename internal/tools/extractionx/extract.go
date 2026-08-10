package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// extractResult is the runExtract outcome.
type extractResult struct {
	text      string
	json      any
	truncated bool
}

// runExtract runs the server-owned extraction task against the gathered tail. The
// backend owns the prompt and the token cap, so the CLI passes only structured
// data: the JSON path forwards the (best-effort parsed) schema and carries the
// returned `result` value; the text path carries the returned text + its truncated
// flag. The JSON path can't report extractor truncation today (a far smaller risk —
// JSON extracts pull small fields and a length-truncated object usually fails to
// parse and is retried).
func runExtract(ctx context.Context, deps Deps, a *extractCore, tail string) (extractResult, error) {
	if a.format == "json" {
		// The backend's terminal_extract_json task takes an optional JSON-schema
		// object. Parse the caller's schema string best-effort: a malformed schema
		// leaves it nil (the task infers a reasonable value) rather than failing.
		var schema map[string]any
		if strings.TrimSpace(a.jsonSchema) != "" {
			_ = json.Unmarshal([]byte(a.jsonSchema), &schema)
		}
		out, err := deps.Router.ExtractJSON(ctx, a.instruction, a.terminalIDs, tail, schema)
		if err != nil {
			return extractResult{}, err
		}
		return extractResult{json: out, truncated: false}, nil
	}
	text, truncated, err := deps.Router.ExtractText(ctx, a.instruction, a.terminalIDs, tail)
	if err != nil {
		return extractResult{}, err
	}
	return extractResult{text: strings.TrimSpace(text), truncated: truncated}, nil
}

// runVerdict judges an extracted result against a pass/fail condition via the
// server-owned extraction_verdict task (the CLI sends only the result + the
// condition). Keeps the "" -> "(empty)" guard so an empty result is judged, not
// blank.
func runVerdict(ctx context.Context, deps Deps, verdictInstruction, resultText string) (bool, string, error) {
	rt := resultText
	if rt == "" {
		rt = "(empty)"
	}
	return deps.Router.Verdict(ctx, rt, verdictInstruction)
}

// confirmFinished asks the shared small-model judge whether the agent has genuinely
// finished its turn, using the terminal tail — the SAME byte-stable question the
// watcher's judgeAgentFinished asks (domain.FinishedJudgeQuestion), and now with the
// SAME inputs: the agent state, the waiting reason, AND lastOutputAt (the silence
// duration). Feeding lastOutputAt is the fix that lets this verdict flip NO→YES as
// the agent goes quiet, exactly like the watcher — without it (the prior bug) the
// verdict was frozen and the poll timed out on an agent the watcher had confirmed
// done. It is the settle gate's defense against a false "waiting": an agent that
// paused mid-task or whose window was backgrounded reads as "waiting" but is NOT done.
//
// Returns (finished, confident):
//   - finished=true: a CONFIDENT yes — resolve the poll and extract.
//   - finished=false, confident=true: a CONFIDENT no — re-judged next cooldown window.
//   - finished=false, confident=false: blank tail / model error / low confidence —
//     the caller should keep polling and re-ask.
func confirmFinished(ctx context.Context, deps Deps, r *readResult) (finished bool, confident bool) {
	if strings.TrimSpace(r.combinedTail) == "" {
		return false, false // no evidence yet — let the tail fill in, then re-ask
	}
	ans, err := deps.Router.Judge(ctx, JudgeInput{
		Tier:          domain.ModelSmall,
		Question:      domain.FinishedJudgeQuestion,
		AgentState:    r.signals.AgentState,
		WaitingReason: r.signals.WaitingReason,
		RuntimeStatus: r.signals.RuntimeStatus,
		LastOutputAt:  lastOutputAtLabel(r.signals.MsSinceOutput),
		Tail:          r.combinedTail,
	})
	if err != nil || ans.Confidence < domain.FinishJudgeConfidenceFloor {
		return false, false // unsure → keep polling
	}
	return ans.Matched, true
}

// lastOutputAtLabel humanizes msSinceOutput as "<floor(ms/1000)>s ago" for the judge
// prompt — mirrors daemon.lastOutputAtLabel so both finish judges see silence the
// same way. 0 (just changed / unobserved) renders as "0s ago".
func lastOutputAtLabel(msSinceOutput int64) string {
	return fmt.Sprintf("%ds ago", msSinceOutput/1000)
}

// rejectModelJudge returns a failed result when the wait carries a modelJudge leaf
// (re-running the classifier every tick is unsupported). NOTE: this is NOT the same
// as the settle gate's finished confirmation — modelJudge is rejected because it
// would re-run a classifier on EVERY poll tick; the finished confirmation runs only
// when the deterministic settle already matched AND the seenWorking/grace pre-filter
// passed (and is deduped on the tail hash), so it is a bounded, one-shot check.
// Returns (zero, false) when the wait is clean.
func rejectModelJudge(wait *domain.WatchCondition) (tools.ToolResult, bool) {
	if wait != nil && len(collectModelJudges(wait)) > 0 {
		return tools.Fail("UNSUPPORTED_CONDITION",
			"modelJudge is not supported in terminal extraction wait conditions; use contains, regex, noOutputForMs, runtimeStatusIs, or stateIs.",
			tools.Unrecoverable()), true
	}
	return tools.ToolResult{}, false
}
