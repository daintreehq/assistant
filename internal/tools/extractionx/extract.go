package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Extractor prompts. Byte-stable.
const extractorSystemPrompt = `You extract specific information from terminal output for a developer's supervisor. You are a small, cheap sub-agent: you do NOT talk to the user and you cannot run tools. Read the provided terminal tail and return ONLY what the caller's instruction asks for — nothing else, no preamble, no commentary. The very FIRST characters you emit must be the extracted value itself — no narration, no restating the instruction.

When asked for plain text, return the extracted value as terse text. When asked for json, return ONLY a single JSON object of the shape { "result": <value> } where <value> matches the caller's requested schema. Do not wrap the json in markdown fences and do not add fields the caller did not ask for.

Never invent content that is not present in the terminal output. If the requested information is genuinely absent, return an empty/"null" result (for text, an empty string; for json, { "result": null }) rather than guessing.`

const verdictSystemPrompt = `You judge whether an extracted result satisfies a caller's pass/fail condition. Return ONLY a JSON object { "pass": boolean, "reason": "<one short sentence>" }. Be strict and literal; do not invent facts beyond the provided result.`

func buildExtractorUserPrompt(instruction, format, jsonSchema, tail string, terminalIDs []string) string {
	header := fmt.Sprintf("Source terminal: %s", "unknown")
	if len(terminalIDs) > 1 {
		header = "Source terminals: " + strings.Join(terminalIDs, ", ")
	} else if len(terminalIDs) == 1 {
		header = "Source terminal: " + terminalIDs[0]
	}
	var shape string
	if format == "json" {
		schema := jsonSchema
		if schema == "" {
			schema = "(no schema provided — infer a reasonable JSON value)"
		}
		shape = fmt.Sprintf("\n\nReturn a JSON object { \"result\": <value> } where <value> conforms to this schema:\n\"\"\"\n%s\n\"\"\"", schema)
	} else {
		shape = "\n\nReturn the extracted value as plain text."
	}
	body := tail
	if body == "" {
		body = "(no output captured)"
	}
	return fmt.Sprintf("%s\nExtraction instruction: %s%s\n\nTerminal output (most recent, bounded):\n\"\"\"\n%s\n\"\"\"\n\nExtract now.",
		header, instruction, shape, body)
}

// extractResult is the runExtract outcome.
type extractResult struct {
	text      string
	json      any
	truncated bool
}

// runExtract runs the extraction model against the gathered tail. The JSON path
// routes through router.JSON (the model emits {"result": <value>}); the text path
// reports truncated=(finishReason=="length"). The JSON path can't report
// extractor truncation today (a far smaller risk — JSON extracts pull small fields
// and a length-truncated object usually fails to parse and is retried).
func runExtract(ctx context.Context, deps Deps, a *extractCore, tail string) (extractResult, error) {
	userPrompt := buildExtractorUserPrompt(a.instruction, a.format, a.jsonSchema, tail, a.terminalIDs)
	messages := []ChatMessage{
		{Role: "system", Content: extractorSystemPrompt},
		{Role: "user", Content: userPrompt},
	}
	if a.format == "json" {
		out, err := deps.Router.JSON(ctx, domain.ModelSmall, messages, a.maxTokens)
		if err != nil {
			return extractResult{}, err
		}
		return extractResult{json: out, truncated: false}, nil
	}
	res, err := deps.Router.Chat(ctx, domain.ModelSmall, messages, a.maxTokens)
	if err != nil {
		return extractResult{}, err
	}
	return extractResult{text: strings.TrimSpace(res.Content), truncated: res.FinishReason == "length"}, nil
}

// runVerdict judges an extracted result against a pass/fail condition.
func runVerdict(ctx context.Context, deps Deps, verdictInstruction, resultText string) (bool, string, error) {
	rt := resultText
	if rt == "" {
		rt = "(empty)"
	}
	out, err := deps.Router.JSON(ctx, domain.ModelSmall, []ChatMessage{
		{Role: "system", Content: verdictSystemPrompt},
		{Role: "user", Content: fmt.Sprintf("Pass/fail condition: %s\n\nExtracted result:\n\"\"\"\n%s\n\"\"\"\n\nReturn the json verdict now.", verdictInstruction, rt)},
	}, 200)
	if err != nil {
		return false, "", err
	}
	// The router returns the parsed `result`; verdict callers parse {pass, reason}.
	m, _ := out.(map[string]any)
	pass, _ := m["pass"].(bool)
	reason, _ := m["reason"].(string)
	return pass, reason, nil
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

// serializeJSONResult mirrors JSON.stringify(value): a nil value becomes "null".
func serializeJSONResult(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
