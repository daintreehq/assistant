package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

const (
	codeMCPUnavailable = "MCP_UNAVAILABLE"
	codeWaitTimeout    = "WAIT_TIMEOUT"
	codeExtract        = "EXTRACT"
)

// settledWait is the "agent has finished its turn" signal: it settled into a
// non-working terminal state. The model reliably passes wait:{} to mean this, so
// the inline extract tools coerce a keyless wait object into this default (real
// watchers keep the strict union — a degenerate condition there can never fire).
func settledWait() domain.WatchCondition {
	waiting, completed, exited := domain.AgentWaiting, domain.AgentCompleted, domain.AgentExited
	return domain.WatchCondition{Any: []domain.WatchCondition{
		{StateIs: &waiting}, {StateIs: &completed}, {StateIs: &exited},
	}}
}

// extractCore holds the parsed shared extract fields (used by both runExtract and
// the handlers). Defaults are applied at decode.
type extractCore struct {
	terminalIDs []string
	instruction string
	format      string
	jsonSchema  string
	maxTokens   int
}

// baseArgs is the shared decoded shape (before wait coercion). wait is captured as
// raw JSON so the empty-object → settled coercion can run before strict decode.
// Output shape (text vs JSON) is NOT a base field: it is fixed per tool —
// terminal.extract is text, terminal.extract.json is structured — so the
// json-needs-schema rule is enforced structurally by the tool the model picks,
// never as a conditional runtime check (a model fills a required field far more
// reliably than it remembers "if format=json then jsonSchema").
type baseArgs struct {
	TerminalIDs    []string        `json:"terminalIds"`
	Wait           json.RawMessage `json:"wait,omitempty"`
	PollIntervalMs *int            `json:"pollIntervalMs,omitempty"`
	MaxAttempts    *int            `json:"maxAttempts,omitempty"`
	TailBytes      *int            `json:"tailBytes,omitempty"`
	MaxTokens      *int            `json:"maxTokens,omitempty"`
}

// Validate enforces the Zod numeric bounds on the shared poll/extract knobs so a
// negative maxAttempts can't make the cap arithmetic degenerate and an oversized
// tailBytes/maxTokens can't be silently honored. Promoted onto extractArgs and
// extractAsyncArgs via embedding. Bounds: pollIntervalMs
// 0–60000, maxAttempts 1–120, tailBytes 1–100000, maxTokens 1–2000.
func (b *baseArgs) Validate() error {
	// terminalIds is required + bounded (Zod: array(string.min(1)).min(1).max(16)).
	// An empty list would poll/read nothing and an empty-string id is meaningless;
	// reject both rather than silently no-op.
	if len(b.TerminalIDs) == 0 {
		return fmt.Errorf("terminalIds must have at least 1 entry")
	}
	if len(b.TerminalIDs) > 16 {
		return fmt.Errorf("terminalIds must have at most 16 entries")
	}
	for _, id := range b.TerminalIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("terminalIds entries must be non-empty")
		}
	}
	if b.PollIntervalMs != nil && (*b.PollIntervalMs < 0 || *b.PollIntervalMs > 60_000) {
		return fmt.Errorf("pollIntervalMs must be between 0 and 60000")
	}
	if b.MaxAttempts != nil && (*b.MaxAttempts < 1 || *b.MaxAttempts > 120) {
		return fmt.Errorf("maxAttempts must be between 1 and 120")
	}
	if b.TailBytes != nil && (*b.TailBytes < 1 || *b.TailBytes > 100_000) {
		return fmt.Errorf("tailBytes must be between 1 and 100000")
	}
	if b.MaxTokens != nil && (*b.MaxTokens < 1 || *b.MaxTokens > 2000) {
		return fmt.Errorf("maxTokens must be between 1 and 2000")
	}
	return nil
}

// resolvedBase carries the decoded base with defaults applied. Output shape (text
// vs JSON) is fixed by the calling tool, not carried here.
type resolvedBase struct {
	terminalIDs    []string
	wait           *domain.WatchCondition
	isSettleWait   bool // wait was the coerced wait:{} settled default (see poll gate)
	pollIntervalMs int
	maxAttempts    int
	tailBytes      int
	maxTokens      int
}

// resolveBase applies defaults and coerces/validates the wait condition. Returns
// (resolved, "") on success or (zero, errMsg) for an INVALID_ARGS failure.
func resolveBase(b baseArgs) (resolvedBase, string) {
	r := resolvedBase{
		terminalIDs:    b.TerminalIDs,
		pollIntervalMs: intOr(b.PollIntervalMs, 2000),
		maxAttempts:    intOr(b.MaxAttempts, 30),
		tailBytes:      intOr(b.TailBytes, 12_000),
		maxTokens:      intOr(b.MaxTokens, 1024),
	}
	if len(b.Wait) > 0 {
		// Coerce a keyless wait object to the settled default (the model's natural
		// wait:{} call). Otherwise strictly decode the WatchCondition union.
		trimmed := strings.TrimSpace(string(b.Wait))
		if isEmptyObject(b.Wait) {
			w := settledWait()
			r.wait = &w
			// Mark this as the SETTLE default so the poll gate applies the
			// seenWorking/grace pre-filter + the small-model finished confirmation. An
			// explicit, equivalent {"stateIs":"waiting"} stays strict (no confirmation) —
			// only the model's natural wait:{} opts into the imperfect-signal handling.
			r.isSettleWait = true
		} else if trimmed == "null" {
			// explicit null → no wait
		} else {
			var w domain.WatchCondition
			if err := json.Unmarshal(b.Wait, &w); err != nil {
				return resolvedBase{}, "wait: " + err.Error()
			}
			r.wait = &w
		}
	}
	// A settle wait keys on a SINGLE agent's state (the aggregate agentState is
	// unset for multiple terminals, so settledWait could never match and the call
	// would silently run to maxAttempts). Reject it with guidance rather than waste
	// the budget — issue one waited extract per terminal instead. Explicit
	// content waits (contains/regex) DO work across terminals (they match the
	// combined tail), so this only gates the coerced settle default.
	if r.isSettleWait && len(b.TerminalIDs) > 1 {
		return resolvedBase{}, "wait: {} waits for ONE agent to finish and cannot span terminals. To wait for a whole cohort, call terminal.awaitAll with all the terminalIds (it polls them concurrently and confirms each); then read their output in the NEXT reply — one terminal.extract over the same ids for a single merged answer, or a batch of per-terminal no-wait extract calls in one reply (they dispatch in parallel) when each terminal needs its own question."
	}
	return r, ""
}

// core builds the extraction unit with the tool-chosen output shape: text tools
// pass ("text", "") and the JSON tool passes ("json", schema).
func (r resolvedBase) core(instruction, format, jsonSchema string) *extractCore {
	return &extractCore{
		terminalIDs: r.terminalIDs,
		instruction: instruction,
		format:      format,
		jsonSchema:  jsonSchema,
		maxTokens:   r.maxTokens,
	}
}

func (r resolvedBase) poll() pollArgs {
	return pollArgs{
		terminalIDs:    r.terminalIDs,
		wait:           r.wait,
		isSettleWait:   r.isSettleWait,
		pollIntervalMs: r.pollIntervalMs,
		maxAttempts:    r.maxAttempts,
		tailBytes:      r.tailBytes,
	}
}

// retireConsumedSupervisors retires the spawn-attached supervisor watcher(s) of
// terminals whose completion an extract WAIT just observed, so they don't
// re-announce an already-consumed completion later as a stale attention event
// (terminal.awaitAll does the same per settled terminal). Consumption requires a
// wait that genuinely observed the agent settle: the coerced wait:{} finish gate
// (FSM + small-model confirmation), or any wait that ended with every target
// exited. A read-once extract, an unmatched wait, or an explicit contains/regex
// match on a still-running agent is NOT completion — those watchers stay. A
// single-terminal poll that saw a nonzero exit reports the consumption as failed
// so the linked workflow ledger closes honestly (multi-terminal polls have no
// per-terminal exit aggregate — they report finished; the model relays the real
// outcome in prose either way). Returns the number retired (0 when no retirer is
// wired). Callers with an extraction step invoke this only AFTER the extraction
// succeeded — a matched wait whose extraction then failed has NOT delivered the
// completion to the model, and the watcher must stay for the retry.
func retireConsumedSupervisors(ctx context.Context, deps Deps, base resolvedBase, poll pollResult) int {
	if deps.Supervisors == nil || base.wait == nil || !poll.matched {
		return 0
	}
	if !base.isSettleWait && !poll.finished {
		return 0
	}
	settled := domain.SettleStatusFinished
	if poll.finished && poll.exitCode != nil && *poll.exitCode != 0 {
		settled = domain.SettleStatusFailed
	}
	n := 0
	for _, id := range base.terminalIDs {
		n += deps.Supervisors.RetireForTerminal(ctx, id, settled)
	}
	return n
}

/* ------------------------------ terminal.extract -------------------------- */

type extractArgs struct {
	Instruction string `json:"instruction,omitempty"`
	baseArgs
}

// sharedBaseProps are the JSON-schema properties common to every extract tool. The
// output-shape fields (format/jsonSchema) are NOT here — output shape is fixed per
// tool (terminal.extract = text, terminal.extract.json = structured).
//
// The constraint keywords (enum/minimum/maximum/minItems/maxItems/default/
// maxProperties) are REAL JSON Schema, not decoration: the backend forwards tool
// parameters to the upstream model verbatim, so every bound the handler enforces
// (baseArgs.Validate) is also machine-visible at call-construction time. Keep the
// two in lockstep — a bound stated only in prose is a bound the model will
// eventually cross. The wait object enumerates the WatchCondition union explicitly
// ({} stays valid: zero properties, the coerced settled default); modelJudge is
// deliberately ABSENT from its properties because extraction rejects it
// (rejectModelJudge) — the schema should make it ungenerable, not just documented.
var sharedBaseProps = `
    "terminalIds": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "maxItems": 16, "description": "Daintree terminal id(s) to read and extract from — full terminal-<uuid> ids exactly as listed (a unique prefix resolves as a fallback, but never invent or abbreviate ids)." },
    "wait": {
      "type": "object",
      "maxProperties": 1,
      "additionalProperties": false,
      "description": "Poll until this condition is met before extracting; omit to read once. A WatchCondition object with EXACTLY ONE of the keys below — or the empty object {} to wait until the agent has genuinely FINISHED its turn. Prefer {} for 'wait for the agent to finish': a bare stateIs:'waiting' is an unreliable proxy (a pre-start prompt or a backgrounded window also reads as 'waiting'), while {} prefers a real working->waiting transition (or a stable idle past a short grace) and ALWAYS confirms with a small-model check on the tail before it resolves; completed/exited resolve immediately. {} is single-terminal (one agent's state) — for a COHORT use terminal.awaitAll, then read with a no-wait extract over the same ids. modelJudge is NOT supported here (watcher-only).",
      "properties": {
        "stateIs": { "type": "string", "enum": ["idle", "working", "waiting", "directing", "completed", "exited"], "description": "Fires when the agent state equals this value exactly. Do NOT use stateIs:'waiting' to mean finished — pass {} instead (it confirms a real finish)." },
        "runtimeStatusIs": { "type": "string", "enum": ["running", "exited"], "description": "Fires on the coarse terminal runtime status." },
        "contains": { "type": "string", "minLength": 1, "description": "Fires when the terminal tail contains this literal substring (non-empty). Matches the COMBINED tail across multiple terminalIds." },
        "regex": { "type": "string", "minLength": 1, "description": "Fires when the tail matches this Go/RE2 regular expression (must compile)." },
        "noOutputForMs": { "type": "integer", "minimum": 1, "description": "Fires once no NEW output has appeared for this many ms." },
        "all": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "AND — every nested condition (each the same one-key WatchCondition shape; modelJudge is not supported anywhere in the tree) must hold." },
        "any": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "OR — at least one nested condition (same one-key shape; no modelJudge anywhere) holds." },
        "not": { "type": "object", "minProperties": 1, "maxProperties": 1, "description": "Negates ONE nested condition (same one-key shape; no modelJudge anywhere). This is a property named not, NOT the JSON-Schema keyword." }
      }
    },
    "pollIntervalMs": { "type": "integer", "minimum": 0, "maximum": 60000, "default": 2000, "description": "Delay between polls in wait mode, in ms." },
    "maxAttempts": { "type": "integer", "minimum": 1, "maximum": 120, "default": 30, "description": "Hard cap on poll attempts in wait mode." },
    "tailBytes": { "type": "integer", "minimum": 1, "maximum": 100000, "default": 12000, "description": "Max characters of each terminal's tail fed to the model." },
    "maxTokens": { "type": "integer", "minimum": 1, "maximum": 2000, "default": 1024, "description": "Max tokens the extraction model may produce. For a verbatim/full-reproduction instruction prefer terminal.read (raw scrollback, no model, no cap)." }`

var extractSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "instruction": { "type": "string", "description": "What to extract, as plain TEXT (a number, a name, a yes/no, the agent's answer to relay). Omit to run a wait/finished gate only (no EXTRACTION model call; a wait:{} settle still uses the cheap small-model finished judge). Need several NAMED fields at once, or one answer PER terminal across a cohort? Use terminal.extract.json — a plain-TEXT extract over multiple terminalIds MERGES them into ONE answer, it does not return one per agent." },` + sharedBaseProps + `
  },
  "required": ["terminalIds"]
}`)

func newExtractTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract",
		Description: "Read a bounded tail of one or more Daintree terminals and extract caller-specified content as plain TEXT with " +
			"the small model — the default way to read what an agent said. Over MULTIPLE terminalIds it MERGES every terminal's tail " +
			"into ONE answer (it does NOT return one answer per terminal); to collect a DISTINCT answer per agent — each one's fact/vote/draft " +
			"— use terminal.extract.json with an array schema keyed by terminalId, or extract a single id at a time. PARALLEL: no-wait " +
			"terminal.extract/.json calls batched in ONE reply run CONCURRENTLY — when you need several independent extractions (a different " +
			"question per terminal, or one answer per agent), emit them ALL as one batch of calls instead of one per turn; the total wait is " +
			"roughly the slowest single call. Optionally wait (poll) until a condition is met before extracting (a wait-bearing call is a " +
			"barrier and runs serially). Omit `instruction` to use it as a finished/condition gate (returns booleans, no " +
			"extraction model call). A wait that observes the agent FINISH also auto-retires that terminal's spawn-attached supervising " +
			"watcher (watchersRetired in the result) — the completion is in your hands, so no completion notification will follow. " +
			"For STRUCTURED output (several named fields, or one entry per terminal) use terminal.extract.json instead. " +
			"Read-only; requires Daintree MCP.",
		Risk: domain.RiskRead,
		// Independent per-call snapshot read: a batch of extracts (one per agent) can run
		// concurrently instead of one backend round-trip at a time. Safe because each call
		// reads its own terminal tail and has no ordering dependency on its siblings.
		Parallelizable: true,
		Schema:         extractSchema,
		Decode:         tools.StrictDecoder(func() any { return &extractArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a extractArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			// Canonicalize ids up front so a truncated/prefix id (the model abbreviates
			// Daintree's full terminal-<uuid> ids) resolves, and an unknown id fails fast
			// instead of feeding the extractor a "Terminal not found" tail.
			resolvedIDs, idFail := resolveTerminalIDs(ctx, deps, a.TerminalIDs)
			if idFail != nil {
				return *idFail
			}
			a.TerminalIDs = resolvedIDs
			base, errMsg := resolveBase(a.baseArgs)
			if errMsg != "" {
				return tools.Fail(domain.CodeValidation, "Invalid arguments for terminal.extract: "+errMsg)
			}
			if rejected, ok := rejectModelJudge(base.wait); ok {
				return rejected
			}

			startedAt := time.Now().UnixMilli()
			poll := pollUntil(ctx, deps, base.poll())
			elapsedMs := time.Now().UnixMilli() - startedAt

			// Gate-only mode: no instruction ⇒ no EXTRACTION model call, just report
			// booleans. (A wait:{} settle may still have invoked the cheap finished judge
			// inside pollUntil above — that is the small-model confirmation, not extraction.)
			// The booleans themselves hand the model the completion, so a settled gate
			// retires the terminals' supervisor watchers right here.
			if a.Instruction == "" {
				met := "not met"
				if poll.matched {
					met = "met"
				}
				result := map[string]any{
					"finished": poll.finished, "matched": poll.matched, "attempts": poll.attempts,
					"elapsedMs": elapsedMs, "terminalIds": base.terminalIDs,
				}
				if retired := retireConsumedSupervisors(ctx, deps, base, poll); retired > 0 {
					result["watchersRetired"] = retired
				}
				return tools.Ok(
					fmt.Sprintf("finished=%v, condition %s (%d attempt(s)).", poll.finished, met, poll.attempts),
					result)
			}
			if base.wait != nil && !poll.matched {
				return tools.Fail(codeWaitTimeout,
					fmt.Sprintf("Wait condition not met after %d attempt(s) (%dms).", poll.attempts, elapsedMs),
					tools.WithDetails(map[string]any{"attempts": poll.attempts, "finished": poll.finished}))
			}

			extracted, err := runExtract(ctx, deps, base.core(a.Instruction, "text", ""), poll.combinedTail)
			if err != nil {
				// No retirement on this path: the wait matched but the content never
				// reached the model — the watcher must survive for the retry.
				return tools.Fail(codeExtract, "Extraction failed: "+err.Error())
			}
			// Only now — settle observed AND content delivered — is the completion consumed.
			retired := retireConsumedSupervisors(ctx, deps, base, poll)
			base0 := extracted.text
			if base0 == "" {
				base0 = "(empty result)"
			}
			note := ""
			if extracted.truncated {
				note = fmt.Sprintf("⚠ This result is cut off: the extraction model hit its maxTokens cap (currently %d) — the SOURCE agent's output is not necessarily incomplete. Do NOT re-extract with the same arguments; either raise maxTokens, or to relay text verbatim use terminal.read (raw scrollback, no model, no token cap).\n\n", base.maxTokens)
			}
			result := map[string]any{
				"terminalIds": base.terminalIDs, "format": "text", "attempts": poll.attempts,
				"elapsedMs": elapsedMs, "matched": poll.matched, "finished": poll.finished,
				"truncated": extracted.truncated, "result": extracted.text,
			}
			if retired > 0 {
				result["watchersRetired"] = retired
			}
			return tools.Ok(note+base0, result)
		},
	}
}

/* ---------------------------- terminal.extract.json ----------------------- */

type extractJSONArgs struct {
	Instruction string `json:"instruction"`
	JSONSchema  string `json:"jsonSchema"`
	baseArgs
}

// Validate enforces the json tool's required instruction + jsonSchema (both
// structurally required so the model can't omit either) on top of the shared base
// bounds. Shadows the promoted baseArgs.Validate, so it delegates explicitly.
func (a *extractJSONArgs) Validate() error {
	if err := a.baseArgs.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Instruction) == "" {
		return fmt.Errorf("instruction is required")
	}
	if strings.TrimSpace(a.JSONSchema) == "" {
		return fmt.Errorf("jsonSchema is required")
	}
	return nil
}

var extractJSONSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "instruction": { "type": "string", "description": "What to extract as STRUCTURED JSON (e.g. each player's vote and reasoning)." },
    "jsonSchema": { "type": "string", "description": "Required. A REAL JSON Schema object describing the value to extract (the same kind you put under a tool's \"parameters\") — NOT an example of the value. ONLY these keywords are accepted: type, properties, required, items, enum, const, additionalProperties, anyOf, oneOf, allOf, minimum, maximum, exclusiveMinimum, exclusiveMaximum, minLength, maxLength, minItems, maxItems, uniqueItems. Any OTHER key is REJECTED — in particular no description, title, default, format, examples, $ref, or $schema anywhere in it; put explanation in the instruction argument instead, and there is NO output-size knob (no maxTokens; to make a field optional just leave it out of \"required\"). The small model returns the value under 'result'. For one entry PER terminal across a cohort, make the value an array of objects each carrying its own terminalId. Correct: {\"type\":\"object\",\"properties\":{\"answers\":{\"type\":\"array\",\"items\":{\"type\":\"object\",\"properties\":{\"terminalId\":{\"type\":\"string\"},\"fact\":{\"type\":\"string\"}},\"required\":[\"terminalId\",\"fact\"]}}},\"required\":[\"answers\"]}. WRONG and REJECTED (a value example, not a schema): {\"answers\":[{\"terminalId\":\"string\",\"fact\":\"string\"}]}." },` + sharedBaseProps + `
  },
  "required": ["terminalIds", "instruction", "jsonSchema"]
}`)

func newExtractJSONTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract.json",
		Description: "Read a bounded tail of one or more Daintree terminals and extract STRUCTURED JSON with the small model — use " +
			"this when you need several NAMED fields at once, OR one entry PER terminal when reading a COHORT: the multi-terminal tail is " +
			"labelled by terminalId, so an array schema keyed by terminalId attributes each agent's answer in ONE call (e.g. collecting " +
			"every agent's fact, or tallying a cohort's votes into one object). Both `instruction` and `jsonSchema` are required. PARALLEL: " +
			"no-wait terminal.extract/.json calls batched in ONE reply run CONCURRENTLY — for several INDEPENDENT extractions (a different " +
			"question per terminal), emit them all as one batch of calls; use the single multi-id call above only when one question spans " +
			"the whole cohort. Optionally wait (poll) until a condition is met before extracting (a wait-bearing call is a barrier and runs " +
			"serially); a wait that observes the agent FINISH also auto-retires that terminal's spawn-attached supervising watcher " +
			"(watchersRetired in the result) — no completion notification will follow. For a single value or plain text to relay from ONE terminal, use " +
			"terminal.extract instead. Read-only; requires Daintree MCP.",
		Risk: domain.RiskRead,
		// Independent per-call snapshot read — see terminal.extract: a cohort of these can
		// run concurrently, no ordering dependency between calls.
		Parallelizable: true,
		Schema:         extractJSONSchema,
		Decode:         tools.StrictDecoder(func() any { return &extractJSONArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a extractJSONArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			// Canonicalize ids up front so a truncated/prefix id resolves and an unknown id
			// fails fast (a wrong id otherwise feeds a "Terminal not found" tail into the
			// structured extractor, producing a confident-but-garbage result).
			resolvedIDs, idFail := resolveTerminalIDs(ctx, deps, a.TerminalIDs)
			if idFail != nil {
				return *idFail
			}
			a.TerminalIDs = resolvedIDs
			base, errMsg := resolveBase(a.baseArgs)
			if errMsg != "" {
				return tools.Fail(domain.CodeValidation, "Invalid arguments for terminal.extract.json: "+errMsg)
			}
			if rejected, ok := rejectModelJudge(base.wait); ok {
				return rejected
			}

			startedAt := time.Now().UnixMilli()
			poll := pollUntil(ctx, deps, base.poll())
			elapsedMs := time.Now().UnixMilli() - startedAt

			if base.wait != nil && !poll.matched {
				return tools.Fail(codeWaitTimeout,
					fmt.Sprintf("Wait condition not met after %d attempt(s) (%dms).", poll.attempts, elapsedMs),
					tools.WithDetails(map[string]any{"attempts": poll.attempts, "finished": poll.finished}))
			}

			extracted, err := runExtract(ctx, deps, base.core(a.Instruction, "json", a.JSONSchema), poll.combinedTail)
			if err != nil {
				// No retirement on this path: the wait matched but the content never
				// reached the model — the watcher must survive for the retry.
				return tools.Fail(codeExtract, "Extraction failed: "+err.Error())
			}
			// Only now — settle observed AND content delivered — is the completion consumed.
			retired := retireConsumedSupervisors(ctx, deps, base, poll)
			result := map[string]any{
				"terminalIds": base.terminalIDs, "format": "json", "attempts": poll.attempts,
				"elapsedMs": elapsedMs, "matched": poll.matched, "finished": poll.finished,
				"truncated": extracted.truncated, "result": extracted.json,
			}
			if retired > 0 {
				result["watchersRetired"] = retired
			}
			return tools.Ok("Extracted JSON result.", result)
		},
	}
}

/* --------------------------- terminal.extract.async ----------------------- */

type extractAsyncArgs struct {
	Instruction string `json:"instruction"`
	baseArgs
	Title              string `json:"title,omitempty"`
	VerdictInstruction string `json:"verdictInstruction,omitempty"`
	DedupeKey          string `json:"dedupeKey,omitempty"`
	TTLMs              *int64 `json:"ttlMs,omitempty"`
}

// Validate enforces the async-only required instruction (Zod: string.min(1)) on
// top of the shared base bounds. terminal.extract.async always runs the model
// (no gate-only mode), so an empty instruction is meaningless. This shadows the
// promoted baseArgs.Validate, so it must delegate to it explicitly.
func (a *extractAsyncArgs) Validate() error {
	if err := a.baseArgs.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Instruction) == "" {
		return fmt.Errorf("instruction is required")
	}
	// Match the schema's minimum:1 — storage treats ttlMs <= 0 as "no expiry",
	// which is the OMITTED semantics; an explicit non-positive value is a mistake.
	if a.TTLMs != nil && *a.TTLMs < 1 {
		return fmt.Errorf("ttlMs must be a positive integer (omit it for no expiry)")
	}
	return nil
}

var extractAsyncSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "instruction": { "type": "string", "description": "What to extract from the output, as plain text." },` + sharedBaseProps + `,
    "title": { "type": "string", "description": "Short label for the queue event the result is published under." },
    "verdictInstruction": { "type": "string", "description": "A pass/fail question evaluated against the extracted result; drives event severity." },
    "dedupeKey": { "type": "string", "description": "Events sharing this key collapse into one in the queue." },
    "ttlMs": { "type": "integer", "minimum": 1, "description": "Time-to-live for the published event, in ms (omit for no expiry)." }
  },
  "required": ["terminalIds", "instruction"]
}`)

func newExtractAsyncTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract.async",
		Description: "Fire-and-forget terminal extraction. Polls the terminal(s) until the wait condition is met, extracts text with " +
			"the small model, optionally judges the result against a pass/fail condition, and publishes the outcome to the attention " +
			"queue (instead of blocking the turn). The main thread drains the verdict when next idle. Read-only; requires Daintree MCP.",
		Risk:   domain.RiskLocal,
		Schema: extractAsyncSchema,
		Decode: tools.StrictDecoder(func() any { return &extractAsyncArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a extractAsyncArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
			// Canonicalize ids synchronously, before detaching the background extraction, so
			// the goroutine works with full ids (a truncated/prefix id resolves; an unknown
			// id fails fast in-turn instead of silently producing a bad async result).
			resolvedIDs, idFail := resolveTerminalIDs(ctx, deps, a.TerminalIDs)
			if idFail != nil {
				return *idFail
			}
			a.TerminalIDs = resolvedIDs
			base, errMsg := resolveBase(a.baseArgs)
			if errMsg != "" {
				return tools.Fail(domain.CodeValidation, "Invalid arguments for terminal.extract.async: "+errMsg)
			}
			if rejected, ok := rejectModelJudge(base.wait); ok {
				return rejected
			}

			// requestId is a bare UUID, reused as the
			// queue dedupeKey default. Not a prefixed domain ID.
			requestID := uuid.NewString()
			// Fire in the background; the result lands in the attention queue. This
			// work OUTLIVES the turn, so it must NOT carry the turn's cancellation
			// — but it MUST be cancellable when the App
			// shuts down, so detach to the APP-SCOPED context, not a fresh
			// context.Background() (which would leak into closed deps after Shutdown).
			// Bound it with a deadline derived from the poll knobs (a generous 2×
			// envelope over the worst-case poll wall-clock, plus a fixed slack for the
			// extract+verdict model calls) so a never-settling wait can't pin the
			// goroutine forever.
			bg, cancel := context.WithTimeout(deps.baseContext(), asyncDeadline(base))
			go func() {
				defer cancel()
				runAsyncExtraction(bg, deps, base, a, requestID)
			}()
			return tools.Ok(
				fmt.Sprintf("Started background extraction %s; the result will land in the attention queue.", requestID),
				map[string]any{"requestId": requestID, "terminalIds": base.terminalIDs})
		},
	}
}

// runAsyncExtraction runs the poll+extract, then optionally a pass/fail verdict,
// and publishes the outcome to the attention queue. Never throws: any failure
// becomes an `error` event so the result always lands in the inbox.
func runAsyncExtraction(ctx context.Context, deps Deps, base resolvedBase, a extractAsyncArgs, requestID string) {
	label := a.Title
	if label == "" {
		label = fmt.Sprintf("Extraction (%s)", strings.Join(base.terminalIDs, ", "))
	}
	var target *domain.EventTarget
	if len(base.terminalIDs) == 1 {
		target = &domain.EventTarget{TerminalID: base.terminalIDs[0]}
	}
	dedupeKey := a.DedupeKey
	if dedupeKey == "" {
		dedupeKey = "extract:" + requestID
	}
	// publish is nil-safe AND panic-safe: a nil queue (mis-wiring) or a panicking
	// Publish must NOT crash the background goroutine — a panic here would be caught
	// by the recovery defer below, which itself calls publish, re-panicking and
	// taking down the process. A failed publish means the async RESULT is lost, so
	// surface it via the debug log instead of swallowing it silently.
	publish := func(args domain.QueuePublishArgs) {
		defer func() {
			if r := recover(); r != nil {
				logAsyncDebug(deps, "extract.async.publish_panic", map[string]any{
					"requestId": requestID, "title": args.Title, "panic": fmt.Sprintf("%v", r),
				})
			}
		}()
		if deps.Queue == nil {
			logAsyncDebug(deps, "extract.async.publish_dropped", map[string]any{
				"requestId": requestID, "title": args.Title, "reason": "no queue wired",
			})
			return
		}
		args.Target = target
		args.DedupeKey = dedupeKey
		args.TTLMs = a.TTLMs
		if _, err := deps.Queue.Publish(ctx, args); err != nil {
			logAsyncDebug(deps, "extract.async.publish_failed", map[string]any{
				"requestId": requestID, "title": args.Title, "error": err.Error(),
			})
		}
	}

	defer func() {
		if r := recover(); r != nil {
			publish(domain.QueuePublishArgs{
				Source: domain.SourceModelWorker, Severity: domain.SeverityError,
				Title: label + ": error", Summary: fmt.Sprintf("Extraction failed: %v", r),
			})
		}
	}()

	poll := pollUntil(ctx, deps, base.poll())
	if base.wait != nil && !poll.matched {
		publish(domain.QueuePublishArgs{
			Source: domain.SourceModelWorker, Severity: domain.SeverityAttention,
			Title:   label + ": wait timed out",
			Summary: fmt.Sprintf("Wait condition not met after %d attempt(s); nothing extracted.", poll.attempts),
		})
		return
	}

	extracted, err := runExtract(ctx, deps, base.core(a.Instruction, "text", ""), poll.combinedTail)
	if err != nil {
		publish(domain.QueuePublishArgs{
			Source: domain.SourceModelWorker, Severity: domain.SeverityError,
			Title: label + ": error", Summary: "Extraction failed: " + err.Error(),
		})
		return
	}
	resultText := extracted.text
	truncatedSuffix := ""
	if extracted.truncated {
		truncatedSuffix = " ⚠ extractor hit its maxTokens cap — result is cut off; raise maxTokens or read raw via terminal.read."
	}

	var pass bool
	var reason string
	hasVerdict := a.VerdictInstruction != ""
	if hasVerdict {
		p, r, verr := runVerdict(ctx, deps, a.VerdictInstruction, resultText)
		if verr != nil {
			publish(domain.QueuePublishArgs{
				Source: domain.SourceModelWorker, Severity: domain.SeverityError,
				Title: label + ": error", Summary: "Extraction failed: " + verr.Error(),
			})
			return
		}
		pass, reason = p, r
	}

	severity := domain.SeverityDone
	title := label + ": done"
	summary := truncateRunes(resultText, 280)
	if summary == "" {
		summary = "(empty result)"
	}
	if hasVerdict {
		if pass {
			title = label + ": pass"
		} else {
			severity = domain.SeverityAttention
			title = label + ": fail"
		}
		summary = reason
	}
	publish(domain.QueuePublishArgs{
		Source: domain.SourceModelWorker, Severity: severity,
		Title:    title,
		Summary:  summary + truncatedSuffix,
		Evidence: []string{truncateRunes(resultText, 2000)},
	})
}

/* --------------------------------- helpers -------------------------------- */

// asyncDeadline bounds a detached extraction so a never-settling wait can't pin
// the goroutine (or its app-scoped ctx) indefinitely. The worst-case poll
// wall-clock is maxAttempts × pollIntervalMs; we allow 2× that envelope plus a
// fixed 60s slack for the extract (+ optional verdict) model calls, with a sane
// floor so a tiny poll budget still leaves room for the model round-trips.
func asyncDeadline(base resolvedBase) time.Duration {
	pollMs := int64(base.maxAttempts) * int64(base.pollIntervalMs)
	const modelSlackMs = 60_000
	total := pollMs*2 + modelSlackMs
	if total < 120_000 {
		total = 120_000
	}
	return time.Duration(total) * time.Millisecond
}

// logAsyncDebug forwards a background-extraction trace, wrapped so it can never
// break the goroutine (debug logging is a side-channel).
func logAsyncDebug(deps Deps, event string, fields map[string]any) {
	defer func() { _ = recover() }()
	debuglog.LogDebug(deps.DebugLog, event, fields)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func intOr(p *int, d int) int {
	if p != nil {
		return *p
	}
	return d
}

// isEmptyObject reports whether raw is a JSON object with no keys ({}).
func isEmptyObject(raw json.RawMessage) bool {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return false
	}
	return len(m) == 0
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
