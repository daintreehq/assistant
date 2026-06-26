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
		return resolvedBase{}, "wait: {} waits for ONE agent to finish and cannot span terminals. To wait for a whole cohort, call terminal.awaitAll with all the terminalIds (it polls them concurrently and confirms each); then read their output with one terminal.extract over the same ids (no wait)."
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

/* ------------------------------ terminal.extract -------------------------- */

type extractArgs struct {
	Instruction string `json:"instruction,omitempty"`
	baseArgs
}

// sharedBaseProps are the JSON-schema properties common to every extract tool. The
// output-shape fields (format/jsonSchema) are NOT here — output shape is fixed per
// tool (terminal.extract = text, terminal.extract.json = structured).
var sharedBaseProps = `
    "terminalIds": { "type": "array", "items": { "type": "string" }, "description": "Daintree terminal id(s) to read and extract from." },
    "wait": { "type": "object", "description": "Poll until this WatchCondition is met before extracting. Exactly ONE key: stateIs, runtimeStatusIs, contains, regex, noOutputForMs, or all/any/not. modelJudge unsupported. Pass {} to wait until the agent has genuinely FINISHED its turn: a bare 'waiting' is an unreliable proxy (a pre-start prompt or a backgrounded window also reads as 'waiting'), so {} prefers a real working->waiting transition (or a stable idle past a short grace if one was never seen) and ALWAYS confirms with a small-model check on the tail before it resolves — it will not return on a momentary idle. completed/exited resolve immediately. {} is single-terminal (one agent's state); for a COHORT use terminal.awaitAll, then read with a no-wait extract over the same ids. Omit to read once." },
    "pollIntervalMs": { "type": "number", "description": "Delay between polls in wait mode, in ms (default 2000)." },
    "maxAttempts": { "type": "number", "description": "Hard cap on poll attempts (default 30, max 120)." },
    "tailBytes": { "type": "number", "description": "Max characters of each terminal's tail fed to the model." },
    "maxTokens": { "type": "number", "description": "Max tokens the extraction model may produce (default 1024, max 2000). For a verbatim/full-reproduction instruction prefer terminal.read." }`

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
			"— use terminal.extract.json with an array schema keyed by terminalId, or extract a single id at a time. Optionally wait (poll) " +
			"until a condition is met before extracting. Omit `instruction` to use it as a finished/condition gate (returns booleans, no " +
			"extraction model call). For STRUCTURED output (several named fields, or one entry per terminal) use terminal.extract.json instead. " +
			"Read-only; requires Daintree MCP.",
		Risk:   domain.RiskRead,
		Schema: extractSchema,
		Decode: tools.StrictDecoder(func() any { return &extractArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a extractArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
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
			if a.Instruction == "" {
				met := "not met"
				if poll.matched {
					met = "met"
				}
				return tools.Ok(
					fmt.Sprintf("finished=%v, condition %s (%d attempt(s)).", poll.finished, met, poll.attempts),
					map[string]any{
						"finished": poll.finished, "matched": poll.matched, "attempts": poll.attempts,
						"elapsedMs": elapsedMs, "terminalIds": base.terminalIDs,
					})
			}
			if base.wait != nil && !poll.matched {
				return tools.Fail(codeWaitTimeout,
					fmt.Sprintf("Wait condition not met after %d attempt(s) (%dms).", poll.attempts, elapsedMs),
					tools.WithDetails(map[string]any{"attempts": poll.attempts, "finished": poll.finished}))
			}

			extracted, err := runExtract(ctx, deps, base.core(a.Instruction, "text", ""), poll.combinedTail)
			if err != nil {
				return tools.Fail(codeExtract, "Extraction failed: "+err.Error())
			}
			base0 := extracted.text
			if base0 == "" {
				base0 = "(empty result)"
			}
			note := ""
			if extracted.truncated {
				note = fmt.Sprintf("⚠ This result is cut off: the extraction model hit its maxTokens cap (currently %d) — the SOURCE agent's output is not necessarily incomplete. Do NOT re-extract with the same arguments; either raise maxTokens, or to relay text verbatim use terminal.read (raw scrollback, no model, no token cap).\n\n", base.maxTokens)
			}
			return tools.Ok(note+base0, map[string]any{
				"terminalIds": base.terminalIDs, "format": "text", "attempts": poll.attempts,
				"elapsedMs": elapsedMs, "matched": poll.matched, "finished": poll.finished,
				"truncated": extracted.truncated, "result": extracted.text,
			})
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
    "jsonSchema": { "type": "string", "description": "A JSON-Schema/description of the value to extract; the small model returns it under 'result'. Required — this is the whole point of the json tool. Example: \"{ \\\"votes\\\": [ { \\\"player\\\": \\\"string\\\", \\\"vote\\\": \\\"yes|no\\\" } ] }\"." },` + sharedBaseProps + `
  },
  "required": ["terminalIds", "instruction", "jsonSchema"]
}`)

func newExtractJSONTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.extract.json",
		Description: "Read a bounded tail of one or more Daintree terminals and extract STRUCTURED JSON with the small model — use " +
			"this when you need several NAMED fields at once, OR one entry PER terminal when reading a COHORT: the multi-terminal tail is " +
			"labelled by terminalId, so an array schema keyed by terminalId attributes each agent's answer in ONE call (e.g. collecting " +
			"every agent's fact, or tallying a cohort's votes into one object). Both `instruction` and `jsonSchema` are required. Optionally " +
			"wait (poll) until a condition is met before extracting. For a single value or plain text to relay from ONE terminal, use " +
			"terminal.extract instead. Read-only; requires Daintree MCP.",
		Risk:   domain.RiskRead,
		Schema: extractJSONSchema,
		Decode: tools.StrictDecoder(func() any { return &extractJSONArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a extractJSONArgs
			_ = json.Unmarshal(raw, &a)
			if !deps.Reader.Connected() {
				return tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminal output cannot be read. Use /reconnect to retry once Daintree is available.")
			}
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
				return tools.Fail(codeExtract, "Extraction failed: "+err.Error())
			}
			return tools.Ok("Extracted JSON result.", map[string]any{
				"terminalIds": base.terminalIDs, "format": "json", "attempts": poll.attempts,
				"elapsedMs": elapsedMs, "matched": poll.matched, "finished": poll.finished,
				"truncated": extracted.truncated, "result": extracted.json,
			})
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
    "ttlMs": { "type": "number", "description": "Time-to-live for the published event, in ms." }
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
