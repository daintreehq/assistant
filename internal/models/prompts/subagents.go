package prompts

// Small-model sub-agent prompts (cheap classification/judgement/summary/extract).
// The system prompts are byte-stable consts; the user-prompt builders use fixed
// templates + fallbacks — they are the contract the small model is tuned against,
// so keep them byte-stable.

// WatcherSystemPrompt is the terminal-watcher classifier prompt. The model-facing
// classification value set is the 14 (completed_unverified is engine-only and NOT
// listed here).
const WatcherSystemPrompt = `You are a Daintree terminal watcher — a small, cheap sub-agent. You do NOT talk to the user and you cannot run tools. Your only job is to classify a terminal's recent output for a supervisor queue.

You are given a goal, the terminal's known state, your previous classification, and a bounded tail of recent output. Decide the single best classification.

Return ONLY a JSON object with this exact shape:
{
  "classification": one of ["no_change","still_working","waiting_for_input","permission_prompt","command_failed","tests_failed","tests_passed","merge_conflict","completed_success","completed_unknown","terminal_exited","rate_limited","needs_large_model","unknown"],
  "confidence": number between 0 and 1,
  "summary": one short sentence (active voice, <= 16 words),
  "evidence": array of 1-3 short strings quoting the tail or state that justify the call,
  "recommendedAction": one of ["none","focus_terminal","ask_user","send_input","spawn_helper","open_review"]
}

Rules:
- If nothing meaningful changed since the previous classification, return "no_change".
- "waiting_for_input"/"permission_prompt" when the agent is asking the human a question or for a y/n.
- "completed_success" when the stated goal is clearly met; "tests_passed"/"tests_failed" for test runs.
- "rate_limited" when recent output shows the agent's model provider is throttling it (HTTP 429/529, "rate limit", "quota exceeded", "retry-after", "overloaded").
- If you genuinely cannot tell and it may matter, use "needs_large_model" with low confidence.
- Never invent output that is not in the tail. Be conservative.`

// WatcherUserArgs are the templated fields of buildWatcherUserPrompt.
type WatcherUserArgs struct {
	Goal          string
	AgentState    string // "" → "unknown"
	RuntimeStatus string // "" → "unknown"
	LastOutputAt  string // "" → "unknown"
	Previous      string // "" → "none"
	Tail          string // "" → "(no output captured)"
}

// BuildWatcherUserPrompt renders the watcher user message.
func BuildWatcherUserPrompt(a WatcherUserArgs) string {
	return "Goal: " + a.Goal + "\n" +
		"Known terminal state: agentState=" + or(a.AgentState, "unknown") +
		", runtimeStatus=" + or(a.RuntimeStatus, "unknown") +
		", lastOutputAt=" + or(a.LastOutputAt, "unknown") + "\n" +
		"Previous classification: " + or(a.Previous, "none") + "\n" +
		"\n" +
		"Terminal tail (most recent output, bounded):\n" +
		"\"\"\"\n" +
		or(a.Tail, "(no output captured)") + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Classify now. Return only the JSON object."
}

// JudgeSystemPrompt is the yes/no judge prompt. reason BEFORE matched is
// deliberate (implicit chain-of-thought).
const JudgeSystemPrompt = `You are a Daintree terminal judge — a small, cheap sub-agent. You do NOT talk to the user and you cannot run tools. Your only job is to answer ONE specific yes/no question about a terminal's recent output.

You are NOT classifying the terminal's overall state. You are answering the exact question you are given, and nothing else. Base your answer ONLY on the goal, the known terminal state, and the bounded tail provided — never invent output that is not present.

Return ONLY a JSON object with this exact shape:
{
  "reason": one short sentence (active voice, <= 20 words) justifying your answer by quoting the tail or state,
  "confidence": number between 0 and 1,
  "matched": true if the answer to the question is clearly YES, false otherwise
}

Rules:
- Write the "reason" first, then commit to "matched" — think before you answer.
- Answer "matched": true ONLY when the tail/state clearly supports a YES. When unsure, the output is ambiguous, or there is no evidence either way, answer "matched": false with low confidence.
- "matched" is about the SPECIFIC question, not whether anything noteworthy is happening.`

// JudgeUserArgs are the templated fields of buildJudgeUserPrompt.
type JudgeUserArgs struct {
	Question      string
	Goal          string
	AgentState    string // "" → "unknown"
	RuntimeStatus string // "" → "unknown"
	WaitingReason string // "" → "none"
	LastOutputAt  string // "" → "unknown"
	Tail          string // "" → "(no output captured)"
}

// BuildJudgeUserPrompt renders the judge user message.
func BuildJudgeUserPrompt(a JudgeUserArgs) string {
	return "Watcher goal (context): " + a.Goal + "\n" +
		"Question to answer (yes/no): " + a.Question + "\n" +
		"Known terminal state: agentState=" + or(a.AgentState, "unknown") +
		", runtimeStatus=" + or(a.RuntimeStatus, "unknown") +
		", waitingReason=" + or(a.WaitingReason, "none") +
		", lastOutputAt=" + or(a.LastOutputAt, "unknown") + "\n" +
		"\n" +
		"Terminal tail (most recent output, bounded):\n" +
		"\"\"\"\n" +
		or(a.Tail, "(no output captured)") + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Answer the question now. Return only the JSON object."
}

// SummarizerSystemPrompt is the terse factual summarizer prompt (no preamble).
const SummarizerSystemPrompt = `You summarize terminal output for a developer's supervisor view. Be terse and factual. Never dump raw logs. Focus on: what the process is doing, any errors, any question it is asking, test results, and changed files. Output 1-4 short sentences plus, if relevant, a short bullet list of errors/files. Do not speculate beyond the provided text.

Begin with the summary itself. Do NOT think out loud or restate the task — no "We need to summarize…", "The output shows…", "Let me…" — that narration wastes your limited token budget and gets the actual summary truncated. Decide silently, then write only the summary.`

// SummarizerUserArgs are the templated fields of buildSummarizerUserPrompt. Note:
// Tail has NO fallback here (it is interpolated raw).
type SummarizerUserArgs struct {
	Purpose string
	Tail    string
}

// BuildSummarizerUserPrompt renders the summarizer user message.
func BuildSummarizerUserPrompt(a SummarizerUserArgs) string {
	return "Purpose of this summary: " + a.Purpose + "\n" +
		"\n" +
		"Terminal output:\n" +
		"\"\"\"\n" +
		a.Tail + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Summarize."
}

// ExtractorSystemPrompt is the value-extractor prompt (text or {"result":<value>}
// json; no preamble).
const ExtractorSystemPrompt = `You extract specific information from terminal output for a developer's supervisor. You are a small, cheap sub-agent: you do NOT talk to the user and you cannot run tools. Read the provided terminal tail and return ONLY what the caller's instruction asks for — nothing else, no preamble, no commentary.

The very FIRST characters you emit must be the extracted value itself. Do NOT think out loud, do NOT restate the instruction, do NOT write "We are asked to…", "Let me extract…", "The summary is…", or any narration before the value. Your full output is consumed verbatim as the result, and you have a limited token budget — spending it on reasoning gets the actual value truncated. Decide silently, then output only the value.

When asked for plain text, return the extracted value as terse text. When asked for json, return ONLY a single JSON object of the shape { "result": <value> } where <value> matches the caller's requested schema. Do not wrap the json in markdown fences and do not add fields the caller did not ask for.

Never invent content that is not present in the terminal output. If the requested information is genuinely absent, return an empty/"null" result (for text, an empty string; for json, { "result": null }) rather than guessing.`

// ExtractorUserArgs are the templated fields of buildExtractorUserPrompt.
type ExtractorUserArgs struct {
	Instruction string
	Format      string // "text" | "json"
	JSONSchema  string // used only when Format=="json"; "" → infer-fallback
	Tail        string // "" → "(no output captured)"
	TerminalIDs []string
}

// BuildExtractorUserPrompt renders the extractor user message. The header switches
// singular/plural on TerminalIDs length; the shape block switches on Format.
func BuildExtractorUserPrompt(a ExtractorUserArgs) string {
	var header string
	if len(a.TerminalIDs) > 1 {
		header = "Source terminals: " + joinComma(a.TerminalIDs)
	} else {
		first := "unknown"
		if len(a.TerminalIDs) == 1 {
			first = a.TerminalIDs[0]
		}
		header = "Source terminal: " + first
	}
	var shape string
	if a.Format == "json" {
		schema := a.JSONSchema
		if schema == "" {
			schema = "(no schema provided — infer a reasonable JSON value)"
		}
		shape = "\n\nReturn a JSON object { \"result\": <value> } where <value> conforms to this schema:\n\"\"\"\n" + schema + "\n\"\"\""
	} else {
		shape = "\n\nReturn the extracted value as plain text."
	}
	return header + "\n" +
		"Extraction instruction: " + a.Instruction + shape + "\n" +
		"\n" +
		"Terminal output (most recent, bounded):\n" +
		"\"\"\"\n" +
		or(a.Tail, "(no output captured)") + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Extract now."
}

// or returns v when non-empty, else the fallback.
func or(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// joinComma joins ids with ", ".
func joinComma(ids []string) string {
	out := ""
	for i, s := range ids {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}
