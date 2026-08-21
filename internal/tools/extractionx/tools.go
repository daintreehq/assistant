package extractionx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
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
	seen := make(map[string]struct{}, len(b.TerminalIDs))
	for _, id := range b.TerminalIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("terminalIds entries must be non-empty")
		}
		// Reject duplicates, same as awaitAll. A repeated id feeds the SAME tail into the
		// extraction twice and inflates the merged-tail count the result reports — and it
		// survives here, because id resolution (which does dedupe) fails open on an
		// unreadable roster and hands the ids straight back.
		if _, dup := seen[id]; dup {
			return fmt.Errorf("terminalIds must not contain duplicates (%q appears more than once)", id)
		}
		seen[id] = struct{}{}
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

// sharedBaseProps renders the JSON-schema properties common to every extract tool. The
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
//
// leafDocs controls only the per-LEAF description prose of `wait`, never structure —
// the same lever internal/tools/watcher uses across stopWhen/alertWhen, and for the
// same reason. This block is rendered TWICE (terminal.extract + terminal.extract.json)
// and the inventory ships on every model round, so documenting each leaf twice was
// ~1.2 KB of pure duplication per turn. terminal.extract projects FIRST (deps.go
// registration order, which is the projection order) and carries the full prose; the
// .json variant points at it.
//
// What stays in BOTH copies, terse or not:
//   - every structural keyword (type/enum/minLength/minimum/minItems/minProperties/
//     maxProperties/additionalProperties/items) — pinned leaf by leaf by
//     TestExtractWaitCarriesTheFullUnion;
//   - the `wait` CONTAINER description, which carries the stateIs:'waiting' trap;
//   - the COMBINED-tail behaviour on contains/regex, because .json is the variant
//     reached FOR multi-terminal work and is where straddling a boundary bites;
//   - the no-modelJudge constraint on the combinators, and the "not is a property,
//     not the JSON-Schema keyword" trap.
//
// Those are the hard-won parts. Terseness may only take restatement.
func sharedBaseProps(leafDocs bool) string {
	doc := func(verbose, terse string) string {
		if leafDocs {
			return verbose
		}
		return terse
	}
	return `
    "terminalIds": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "maxItems": 16, "uniqueItems": true, "description": "Daintree terminal id(s) to read and extract from — full terminal-<uuid> ids exactly as listed (a unique prefix resolves as a fallback, but never invent or abbreviate ids)." },
    "wait": {
      "type": "object",
      "maxProperties": 1,
      "additionalProperties": false,
      "description": "Poll until this holds before extracting; omit to read once. EXACTLY ONE key below, or {} for a genuine FINISH — prefer {} over stateIs:'waiting', which a pre-start prompt also matches. {} is single-terminal; for a cohort use terminal.awaitAll. No modelJudge here.",
      "properties": {
        "stateIs": { "type": "string", "enum": ["idle", "working", "waiting", "directing", "completed", "exited"], "description": "` + doc("Fires when the agent state equals this value exactly. Do NOT use stateIs:'waiting' to mean finished — pass {} instead (it confirms a real finish).", "Agent state equals this value exactly.") + `" },
        "runtimeStatusIs": { "type": "string", "enum": ["running", "exited"], "description": "` + doc("Fires on the coarse terminal runtime status.", "Coarse terminal runtime status.") + `" },
        "contains": { "type": "string", "minLength": 1, "description": "` + doc("Fires when the terminal tail contains this literal substring (non-empty). Matches the COMBINED tail across multiple terminalIds.", "Tail contains this literal substring. Matches the COMBINED tail across terminalIds.") + `" },
        "regex": { "type": "string", "minLength": 1, "description": "` + doc("Fires when the tail matches this Go/RE2 regular expression (must compile). Matches the COMBINED tail across multiple terminalIds, so it can straddle a terminal boundary.", "Tail matches this Go/RE2 regex (must compile). Matches the COMBINED tail, so it can straddle a terminal boundary.") + `" },
        "noOutputForMs": { "type": "integer", "minimum": 1, "description": "` + doc("Fires once no NEW output has appeared for this many ms.", "No new output for this many ms.") + `" },
        "all": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "` + doc("AND — every nested condition (each the same one-key WatchCondition shape; modelJudge is not supported anywhere in the tree) must hold.", "AND over nested one-key conditions; no modelJudge anywhere.") + `" },
        "any": { "type": "array", "minItems": 1, "items": { "type": "object", "minProperties": 1, "maxProperties": 1 }, "description": "` + doc("OR — at least one nested condition (same one-key shape; no modelJudge anywhere) holds.", "OR over nested one-key conditions; no modelJudge anywhere.") + `" },
        "not": { "type": "object", "minProperties": 1, "maxProperties": 1, "description": "` + doc("Negates ONE nested condition (same one-key shape; no modelJudge anywhere). This is a property named not, NOT the JSON-Schema keyword.", "Negates ONE nested one-key condition; no modelJudge anywhere. A property named not, NOT the JSON-Schema keyword.") + `" }
      }
    },
    "pollIntervalMs": { "type": "integer", "minimum": 0, "maximum": 60000, "default": 2000, "description": "Delay between polls in wait mode, in ms." },
    "maxAttempts": { "type": "integer", "minimum": 1, "maximum": 120, "default": 30, "description": "Hard cap on poll attempts in wait mode." },
    "tailBytes": { "type": "integer", "minimum": 1, "maximum": 100000, "default": 12000, "description": "Max characters of each terminal's tail fed to the model." },
    "maxTokens": { "type": "integer", "minimum": 1, "maximum": 2000, "default": 1024, "description": "Max tokens the extraction model may produce. For a verbatim/full-reproduction instruction prefer terminal.read (raw scrollback, no model, no cap)." }`
}

var extractSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "instruction": { "type": "string", "description": "What to extract, as plain TEXT (a number, a name, a yes/no, an agent's answer). Omit to run a wait gate only. Over several terminalIds this MERGES them into ONE answer — for named fields, or one answer per terminal, use terminal.extract.json." },` + sharedBaseProps(true) + `
  },
  "required": ["terminalIds"]
}`)

func newExtractTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract",
		Description: "Over MULTIPLE terminalIds, MERGES bounded tails via the small model into ONE plain-TEXT answer — never one per terminal. " +
			"For a distinct answer per agent, or several named fields, use terminal.extract.json with an array schema keyed by terminalId. " +
			"On a SINGLE terminalId it is the default way to read what an agent said. " +
			"PARALLEL: no-wait extract/.json calls batched in ONE reply run CONCURRENTLY — emit several independent extractions as one batch, not one per turn; the wait is roughly the slowest single call. A wait-bearing call is a barrier and runs serially. " +
			"Omit `instruction` to use it as a finished/condition gate (booleans only, no extraction model call). " +
			"A wait that observes the agent FINISH auto-retires that terminal's spawn-attached watcher (watchersRetired) — the completion is in your hands, so no notification follows. " +
			"Read-only; needs Daintree MCP.",
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
				note = textTruncationNote(base.maxTokens)
			}
			result := map[string]any{
				"terminalIds": base.terminalIDs, "format": "text", "attempts": poll.attempts,
				"elapsedMs": elapsedMs, "matched": poll.matched, "finished": poll.finished,
				"truncated": extracted.truncated, "result": extracted.text,
			}
			if retired > 0 {
				result["watchersRetired"] = retired
			}
			// Merge scope leads: it says the answer may be about the WRONG thing, which
			// outranks the truncation note's "there is more of the right thing".
			return tools.Ok(noteMergedExtraction(result, base.terminalIDs, mergeRemedyText)+note+base0, result)
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
    "jsonSchema": { "type": "string", "description": "Required. A REAL JSON Schema (the kind you put under a tool's \"parameters\"), NOT an example value. Accepted keywords ONLY: type, properties, required, items, enum, const, additionalProperties, anyOf, oneOf, allOf, minimum, maximum, exclusiveMinimum, exclusiveMaximum, minLength, maxLength, minItems, maxItems, uniqueItems. Any other key is REJECTED — no description, title, default, format, examples, $ref or $schema anywhere. Put explanation in instruction. For one entry per terminal, make the value an array of objects each carrying its own terminalId." },` + sharedBaseProps(false) + `
  },
  "required": ["terminalIds", "instruction", "jsonSchema"]
}`)

func newExtractJSONTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract.json",
		Description: "Extract STRUCTURED JSON from one or more Daintree terminal tails with the small model. Use it for several NAMED fields at once, or one entry PER terminal across a cohort: the multi-terminal tail is labelled by terminalId, so an array schema keyed by terminalId attributes each agent's answer in ONE call. " +
			"Both `instruction` and `jsonSchema` are required. " +
			"Same wait, watcher-retirement and PARALLEL batching rules as terminal.extract — use that one for a single value or plain text to relay. " +
			"Read-only; needs Daintree MCP.",
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
			// Flagged even though a terminalId-keyed schema CAN attribute per agent: the
			// merge is a fact about the input pass, and whether the schema actually asked
			// for an entry per id is not something this handler can tell from an arbitrary
			// jsonSchema. Reporting the topology and letting the remedy name the check
			// beats a classifier that would quietly overclaim coverage.
			return tools.Ok(noteMergedExtraction(result, base.terminalIDs, mergeRemedyJSON)+"Extracted JSON result.", result)
		},
	}
}

/* --------------------------------- helpers -------------------------------- */

// textTruncationNote is the summary prefix for an extraction the MODEL cut short at its
// maxTokens cap. A function rather than an inline literal so the merge note's length
// budget (below) can measure the real string instead of a copy that silently goes stale.
func textTruncationNote(maxTokens int) string {
	return fmt.Sprintf("⚠ This result is cut off: the extraction model hit its maxTokens cap (currently %d) — the SOURCE agent's output is not necessarily incomplete. Do NOT re-extract with the same arguments; either raise maxTokens, or to relay text verbatim use terminal.read (raw scrollback, no model, no token cap).\n\n", maxTokens)
}

// The remedy clause each extraction tool appends to the shared merge warning. They
// differ because the way OUT of the ambiguity differs: text has no way to attribute an
// answer, so the fix is one call per id; the json tool can attribute, so the fix is to
// check the schema actually produced an entry per id.
const (
	mergeRemedyText = "For an answer each, call terminal.extract once per id, or terminal.extract.json keyed by terminalId."
	mergeRemedyJSON = "Confirm there is an entry for every terminalId before treating this as full-cohort coverage."
)

// noteMergedExtraction flags a successful extraction whose INPUT spanned several
// terminals, and returns the same warning as a summary prefix ("" for a single id).
//
// The gap it closes: over several ids the tails are concatenated into ONE extraction
// pass, so the model may answer about a single terminal while the result echoes all N
// ids back. None of the existing fields catch that — terminalIds is input provenance,
// matched is the WAIT verdict, and truncated is the extraction model's token cap; all
// three are honestly true of a one-terminal answer. Only the merge itself is missing,
// so that is what gets reported, and only when it can actually mislead (len > 1).
//
// It lands in BOTH channels deliberately, from ONE string so they cannot drift. The map
// is the structured signal, but it is not guaranteed to reach the model: once the
// SERIALIZED result outgrows MaxToolResultChars, SerializeToolResult swaps the whole map
// for a stub whose only view of it is a positional Preview slice — a flag near the end of
// the map can fall outside that window, while the Summary is kept (truncated to
// TruncationSummaryChars) unconditionally. The summary copy is the guaranteed channel,
// which is why it LEADS.
//
// Kept short for that same reason: at the schema's maxItems (16) the text note plus the
// truncation note beneath it total 493 runes, so both survive whole inside the 500-rune
// summary cap. Lengthening either silently amputates the other's remedy — pinned by
// TestMergeNote_LeavesRoomForTheTruncationNote.
func noteMergedExtraction(result map[string]any, terminalIDs []string, remedy string) string {
	if len(terminalIDs) <= 1 {
		return ""
	}
	note := fmt.Sprintf("⚠ MERGED: %d tails went into ONE extraction pass — this answer may cover only one terminal. %s",
		len(terminalIDs), remedy)
	result["merged"] = true
	result["note"] = note
	return note + "\n\n"
}

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
