package asyncx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
	"github.com/daintreehq/daintree-assistant/internal/tools/terminalid"
)

const (
	codeAsyncUnavailable = "ASYNC_UNAVAILABLE"
	codeAsyncLimit       = "ASYNC_LIMIT"
	codeMCPUnavailable   = "MCP_UNAVAILABLE"
	codeSendFailed       = "SEND_FAILED"
	codeUnknownTerminals = "UNKNOWN_TERMINALS"
)

// Timeout bounds. The default is generous (most async work is a long test run
// or an agent task); the ceiling stops a forgotten future from being polled all
// day — past it the invocation expires and the wake reports what settled.
const (
	defaultTimeoutMs int64 = 30 * 60 * 1000     // 30 minutes
	minTimeoutMs     int64 = 10_000             // 10 seconds
	maxTimeoutMs     int64 = 2 * 60 * 60 * 1000 // 2 hours

	// maxLiveInvocations bounds concurrently-tracked futures (backpressure): a
	// model fanning out async work past this gets a clear failure instead of an
	// unbounded poll set. Terminals per invocation share awaitAll's 16 cap.
	maxLiveInvocations = 16
	maxTerminals       = 16

	// titleMaxRunes bounds a caller title / derived command label so a verbose
	// command can't bloat every queue event and footer row it appears in.
	titleMaxRunes = 64

	// listHistoryCap bounds async.list's includeFinished output to the newest
	// rows — the retention sweep keeps a week of history, which must not all
	// serialize into one tool result.
	listHistoryCap = 50
)

// validateTimeout applies the shared bounds; 0 means "use the default".
func validateTimeout(p *int64) (int64, error) {
	if p == nil {
		return defaultTimeoutMs, nil
	}
	if *p < minTimeoutMs || *p > maxTimeoutMs {
		return 0, fmt.Errorf("timeoutMs must be between %d (10s) and %d (2h)", minTimeoutMs, maxTimeoutMs)
	}
	return *p, nil
}

// asyncPreflight runs the shared gates: a running coordinator (async work
// needs a live poll loop to register with), a connected MCP, and the
// live-invocation cap.
func asyncPreflight(deps Deps, toolName string) *tools.ToolResult {
	if deps.Coordinator == nil || !deps.Coordinator.Started() {
		f := tools.Fail(codeAsyncUnavailable,
			toolName+" needs a running async supervisor, which this one-shot invocation does not have. Use the blocking terminal.awaitAll instead, or run interactively.",
			tools.Unrecoverable())
		return &f
	}
	if !deps.Reader.Connected() {
		f := tools.Fail(codeMCPUnavailable, "Daintree MCP is not connected, so terminals cannot be watched. Use /reconnect to retry once Daintree is available.")
		return &f
	}
	if n, err := deps.Store.CountLiveAsyncInvocations(); err == nil && n >= maxLiveInvocations {
		f := tools.Fail(codeAsyncLimit, fmt.Sprintf(
			"Too many async operations are already running (%d of %d). Wait for completions (they arrive through the attention queue), or cancel ones you no longer need with async.cancel.",
			n, maxLiveInvocations))
		return &f
	}
	return nil
}

// resolveIDs canonicalizes caller-supplied terminal ids against the live roster
// (the model routinely truncates Daintree's full terminal-<uuid> ids). Fails
// OPEN on an unreadable/empty roster — mirrors extractionx.resolveTerminalIDs.
func resolveIDs(ctx context.Context, deps Deps, ids []string) ([]string, *tools.ToolResult) {
	live, ok := deps.Reader.ListTerminals(ctx)
	if !ok || len(live) == 0 {
		return ids, nil
	}
	r := terminalid.Resolve(ids, live)
	if r.OK() {
		return r.Resolved, nil
	}
	var b strings.Builder
	if len(r.Unknown) > 0 {
		fmt.Fprintf(&b, "No live terminal matches %s. ", quoteList(r.Unknown))
	}
	if len(r.Ambiguous) > 0 {
		fmt.Fprintf(&b, "%s is an ambiguous prefix matching several terminals. ", quoteList(r.Ambiguous))
	}
	b.WriteString("Use the EXACT, FULL terminal id returned by the spawn — never an abbreviated prefix. Live terminals: " + strings.Join(live, ", ") + ".")
	f := tools.Fail(codeUnknownTerminals, b.String(), tools.WithDetails(map[string]any{
		"unknown": r.Unknown, "ambiguous": r.Ambiguous, "liveTerminals": live,
	}))
	return nil, &f
}

func quoteList(ids []string) string {
	q := make([]string, len(ids))
	for i, id := range ids {
		q[i] = fmt.Sprintf("%q", id)
	}
	return strings.Join(q, ", ")
}

// registerAndAccept hands a RUNNING invocation to the coordinator and builds
// the immediate "accepted" result with its typed async handle. Shared by both
// async starters — the row is already status=running when this runs (the
// starters insert it that way, or claim it there after their side effect).
// sideEffectNote is appended to any failure so a starter whose mutating side
// effect ALREADY EXECUTED (terminal.run.async's send) can forbid a re-send —
// a generic failure here must never read as "safe to retry".
func registerAndAccept(deps Deps, rec domain.AsyncInvocationRecord, terminalIDs []string, summary, sideEffectNote string) tools.ToolResult {
	if err := deps.Coordinator.Register(rec, terminalIDs); err != nil {
		// The row exists but nothing will poll it — finalize it as abandoned so
		// the ledger never shows a live invocation no coordinator owns.
		_, _ = deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
			"status": string(domain.AsyncAbandoned), "finishedAt": deps.now(),
			"lastError": err.Error(),
		})
		return tools.Fail(codeAsyncUnavailable,
			"Could not start async supervision: "+err.Error()+sideEffectNote)
	}

	res := tools.Ok(summary, map[string]any{
		"asyncId":     rec.ID,
		"state":       "running",
		"terminalIds": terminalIDs,
		"title":       rec.Title,
		"expiresAt":   rec.ExpiresAt,
		"note":        "Asynchronous: the runtime is watching this and KEEPS watching after the assistant closes (async work is project-scoped; the background supervisor adopts it and integrates the completion). Do NOT poll, await, or re-run it — the completion arrives through the attention queue and will wake you, or greet the user on their next attach.",
	})
	res.Async = &domain.AsyncHandle{
		ID:          rec.ID,
		ToolName:    rec.ToolName,
		Title:       rec.Title,
		GroupID:     rec.GroupID,
		TerminalIDs: terminalIDs,
	}
	return res
}

// deriveTitle picks the invocation's short human label.
func deriveTitle(explicit, fallback string) string {
	t := strings.TrimSpace(explicit)
	if t == "" {
		t = strings.TrimSpace(fallback)
	}
	t = strings.Join(strings.Fields(t), " ")
	r := []rune(t)
	if len(r) > titleMaxRunes {
		t = string(r[:titleMaxRunes-1]) + "…"
	}
	if t == "" {
		t = "async operation"
	}
	return t
}

// strPtr returns a pointer for a non-empty string, nil otherwise.
func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

/* ----------------------------- terminal.run.async ------------------------- */

type runAsyncArgs struct {
	TerminalID string `json:"terminalId"`
	Command    string `json:"command"`
	Title      string `json:"title,omitempty"`
	TimeoutMs  *int64 `json:"timeoutMs,omitempty"`
}

func (a *runAsyncArgs) Validate() error {
	if strings.TrimSpace(a.TerminalID) == "" {
		return fmt.Errorf("terminalId is required")
	}
	if strings.TrimSpace(a.Command) == "" {
		return fmt.Errorf("command is required")
	}
	if _, err := validateTimeout(a.TimeoutMs); err != nil {
		return err
	}
	return nil
}

var runAsyncSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "minLength": 1, "description": "The ONE terminal to send the command to (full terminal-<uuid> id)." },
    "command": { "type": "string", "description": "Command line to type into the terminal and run (for an agent terminal, this is the prompt/reply you are sending it)." },
    "title": { "type": "string", "description": "Short human label for this operation (shown in the pending state, the completion event, and async.list). Defaults to the command text." },
    "timeoutMs": { "type": "integer", "minimum": 10000, "maximum": 7200000, "default": 1800000, "description": "Deadline in ms (default 30m, max 2h). Past it you are woken with whatever settled." }
  },
  "required": ["terminalId", "command"]
}`)

func newRunAsyncTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.run.async",
		Description: "Send a command (or a prompt to an agent) to ONE Daintree terminal and watch it to completion ASYNCHRONOUSLY: the " +
			"command is typed and run exactly like terminal.sendCommand, then this returns IMMEDIATELY with an async handle (asy_…) and the " +
			"runtime takes over — cheap 1-second agent-state polls (no model cost, no output reads) until the terminal settles, then an " +
			"attention-queue event that WAKES you with the outcome. Use it instead of terminal.sendCommand + terminal.awaitAll whenever the " +
			"work will take more than a minute or two, or the user should get your reply now while the work continues. AFTER calling it: " +
			"tell the user what is running and END the turn (or move on to other work) — do NOT awaitAll/extract-wait the same terminal, do " +
			"NOT poll async.list for it, and do NOT re-send the command. When the completion wake arrives, read the output then (terminal.summarize " +
			"or terminal.extract) and continue the task. Finish detection tracks agent state, so it is built for agent terminals. " +
			"PROJECT-SCOPED AND DURABLE: the watch keeps running after the assistant closes — the background supervisor adopts it and " +
			"integrates the completion, so you MAY promise the user an after-close or overnight result (it pauses only if Daintree itself " +
			"closes, and resumes on the next launch). Mutating (it runs a command), so it confirms like terminal.sendCommand.",
		Consequence: "Runs a command in the named terminal (as if typed), then watches it — including after the assistant closes — and notifies when it finishes. Effects depend on the command and may not be reversible.",
		Risk:        domain.RiskTerminal,
		Schema:      runAsyncSchema,
		Decode:      tools.StrictDecoder(func() any { return &runAsyncArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a runAsyncArgs
			_ = json.Unmarshal(raw, &a)
			if fail := asyncPreflight(deps, "terminal.run.async"); fail != nil {
				return *fail
			}
			ids, fail := resolveIDs(ctx, deps, []string{strings.TrimSpace(a.TerminalID)})
			if fail != nil {
				return *fail
			}
			terminalID := ids[0]
			timeout, _ := validateTimeout(a.TimeoutMs)
			title := deriveTitle(a.Title, a.Command)
			now := deps.now()

			rec := domain.AsyncInvocationRecord{
				ToolName:        "terminal.run.async",
				Title:           title,
				GroupID:         groupIDFor(tctx),
				SessionID:       deps.SessionID,
				TerminalIdsJson: mustJSONIDs([]string{terminalID}),
				Command:         strPtr(a.Command),
				Status:          domain.AsyncStarting,
				CreatedAt:       now,
				ExpiresAt:       now + timeout,
			}
			rec, err := deps.Store.InsertAsyncInvocation(rec)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "Failed to register the async operation: "+err.Error())
			}

			// Invalidate the terminal's cross-call "seen working" settle evidence
			// BEFORE the send: a transport failure is AMBIGUOUS (the command may
			// already be running), so the mark cannot wait for a confirmed success —
			// stale evidence would let an in-turn wait settle "finished" on the
			// pre-send prompt. A definitively rejected send injects nothing; the
			// spurious invalidation only routes the next wait to the safe slow path.
			if deps.Observer != nil {
				deps.Observer.MarkCommandSent(terminalID, deps.now())
			}
			// The ONE mutating side effect — persisted-intent-first (the row above),
			// performed exactly once, never retried. A failure finalizes the row so
			// the ledger never shows a live future whose command never ran. The
			// failure TEXT depends on what is actually known: a tool-level rejection
			// means the command did NOT run (safe to fix and retry), while a
			// transport/timeout error is AMBIGUOUS — Daintree may have accepted the
			// command before the connection dropped — so a blind re-send could
			// execute it twice.
			if err := deps.Sender.SendCommand(ctx, terminalID, a.Command); err != nil {
				_, _ = deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
					"status": string(domain.AsyncFailed), "finishedAt": deps.now(),
					"lastError": err.Error(),
				})
				var rejected SendRejectedError
				if errors.As(err, &rejected) {
					return tools.Fail(codeSendFailed,
						"Daintree rejected the command for "+terminalID+" (it did NOT run): "+rejected.Msg+
							" Fix the arguments and retry if still wanted.")
				}
				return tools.Fail(codeSendFailed,
					"Sending the command to "+terminalID+" failed with a transport error, so its outcome is UNKNOWN — the command MAY already be running in the terminal. Do NOT blindly re-send it; read the terminal first (terminal.read/terminal.summarize) to see whether it started. Underlying error: "+err.Error())
			}

			// The command IS running now. Activate the row; from here every failure
			// message must carry the do-NOT-re-send warning — a generic failure would
			// invite the model to run the (already executing) command twice.
			const alreadySentNote = " IMPORTANT: the command WAS already sent and is running in the terminal — do NOT re-send it; read the terminal later (terminal.summarize/read) to see its result."
			if ok, aerr := deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
				"status": string(domain.AsyncRunning), "startedAt": deps.now(),
			}); aerr != nil || !ok {
				_, _ = deps.Store.ClaimLiveAsyncInvocation(rec.ID, map[string]any{
					"status": string(domain.AsyncAbandoned), "finishedAt": deps.now(),
					"lastError": "failed to activate the ledger row after the send",
				})
				return tools.Fail(domain.CodeInternal,
					"The async ledger could not be activated."+alreadySentNote)
			}
			rec.Status = domain.AsyncRunning

			return registerAndAccept(deps, rec, []string{terminalID}, fmt.Sprintf(
				"Started asynchronously: %q is running in %s (async id %s). The completion arrives through the attention queue — even after the assistant closes, the background supervisor keeps watching. Do not wait for it in this turn.",
				title, terminalID, rec.ID), alreadySentNote)
		},
	}
}

/* ---------------------------- terminal.await.async ------------------------ */

type awaitAsyncArgs struct {
	TerminalIDs []string `json:"terminalIds"`
	Title       string   `json:"title,omitempty"`
	TimeoutMs   *int64   `json:"timeoutMs,omitempty"`
}

func (a *awaitAsyncArgs) Validate() error {
	if len(a.TerminalIDs) == 0 {
		return fmt.Errorf("terminalIds must have at least 1 entry")
	}
	if len(a.TerminalIDs) > maxTerminals {
		return fmt.Errorf("terminalIds must have at most %d entries", maxTerminals)
	}
	seen := make(map[string]struct{}, len(a.TerminalIDs))
	for _, id := range a.TerminalIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("terminalIds entries must be non-empty")
		}
		if _, dup := seen[id]; dup {
			return fmt.Errorf("terminalIds must not contain duplicates (%q appears more than once)", id)
		}
		seen[id] = struct{}{}
	}
	if _, err := validateTimeout(a.TimeoutMs); err != nil {
		return err
	}
	return nil
}

var awaitAsyncSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalIds": { "type": "array", "items": { "type": "string", "minLength": 1 }, "minItems": 1, "maxItems": 16, "uniqueItems": true, "description": "The agent terminals to watch — full terminal-<uuid> ids exactly as listed. ONE call covers the whole cohort — never one call per agent." },
    "title": { "type": "string", "description": "Short human label for this wait (shown in the completion event and async.list)." },
    "timeoutMs": { "type": "integer", "minimum": 10000, "maximum": 7200000, "default": 1800000, "description": "Deadline in ms (default 30m, max 2h). Past it you are woken with whatever settled." }
  },
  "required": ["terminalIds"]
}`)

func newAwaitAsyncTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.await.async",
		Description: "Watch agent terminal(s) to completion ASYNCHRONOUSLY — the out-of-turn twin of terminal.awaitAll. Returns IMMEDIATELY " +
			"with an async handle (asy_…); the runtime polls the cohort's agent state every second (no model cost, no output reads) and WAKES " +
			"you through the attention queue once EVERY terminal has finished, failed, or asked a question (or the deadline passes — you are " +
			"woken with whatever settled). Choose by wait length and user experience: terminal.awaitAll BLOCKS the turn and is right for short " +
			"waits whose results you need immediately to continue; terminal.await.async lets you reply to the user NOW and end the turn while " +
			"agents keep working — right after spawning a cohort with agentTask.spawnForEdits, or after sending long work with " +
			"terminal.sendCommand. AFTER calling it: report what is running and end the turn — do NOT also awaitAll/extract-wait the same " +
			"terminals, and do NOT attach a watcher just for finish detection (watchers are for goal-based observation with classification). " +
			"When the wake arrives, read outputs with terminal.summarize/terminal.extract and continue. PROJECT-SCOPED AND DURABLE: the " +
			"watch keeps running after the assistant closes — the background supervisor adopts it and integrates the completion, so you MAY " +
			"promise the user an after-close or overnight result (it pauses only if Daintree itself closes, and resumes on the next launch). " +
			"Read-only; requires Daintree MCP.",
		Risk:   domain.RiskLocal,
		Schema: awaitAsyncSchema,
		Decode: tools.StrictDecoder(func() any { return &awaitAsyncArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a awaitAsyncArgs
			_ = json.Unmarshal(raw, &a)
			if fail := asyncPreflight(deps, "terminal.await.async"); fail != nil {
				return *fail
			}
			ids, fail := resolveIDs(ctx, deps, a.TerminalIDs)
			if fail != nil {
				return *fail
			}
			timeout, _ := validateTimeout(a.TimeoutMs)
			title := deriveTitle(a.Title, fmt.Sprintf("wait for %d terminal(s)", len(ids)))
			now := deps.now()

			// Watch-only: no side effect sits between insert and activation, so the
			// row is born running in ONE write (the two-phase 'starting' shape is
			// reserved for run.async, where the send happens in between).
			started := now
			rec := domain.AsyncInvocationRecord{
				ToolName:        "terminal.await.async",
				Title:           title,
				GroupID:         groupIDFor(tctx),
				SessionID:       deps.SessionID,
				TerminalIdsJson: mustJSONIDs(ids),
				Status:          domain.AsyncRunning,
				CreatedAt:       now,
				StartedAt:       &started,
				ExpiresAt:       now + timeout,
			}
			rec, err := deps.Store.InsertAsyncInvocation(rec)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "Failed to register the async operation: "+err.Error())
			}

			return registerAndAccept(deps, rec, ids, fmt.Sprintf(
				"Watching %d terminal(s) asynchronously as %q (async id %s). You will be woken through the attention queue when they settle — even after the assistant closes, the background supervisor keeps watching. Do not wait for them in this turn.",
				len(ids), title, rec.ID), "")
		},
	}
}

/* --------------------------------- async.list ----------------------------- */

type listArgs struct {
	IncludeFinished bool `json:"includeFinished,omitempty"`
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "includeFinished": { "type": "boolean", "description": "Also include finished/cancelled/expired operations (default: live only)." }
  },
  "required": []
}`)

func newListTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "async.list",
		Description: "List asynchronous operations (terminal.run.async / terminal.await.async): the live ones by default, everything recent " +
			"with includeFinished. Use it to answer 'what is still running?' — NOT to poll for a completion (completions wake you through the " +
			"attention queue on their own). Read-only.",
		Risk:   domain.RiskRead,
		Schema: listSchema,
		Decode: tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a listArgs
			_ = json.Unmarshal(raw, &a)
			var (
				rows []domain.AsyncInvocationRecord
				err  error
			)
			if a.IncludeFinished {
				rows, err = deps.Store.ListAsyncInvocations("")
			} else {
				rows, err = deps.Store.ListLiveAsyncInvocations()
			}
			if err != nil {
				return tools.Fail(domain.CodeInternal, "Failed to list async operations: "+err.Error())
			}
			// Bound the includeFinished history: rows are createdAt-ascending, so
			// keep the NEWEST listHistoryCap (the live set is already ≤16). A busy
			// week of finished rows must not balloon one tool result.
			truncatedRows := 0
			if len(rows) > listHistoryCap {
				truncatedRows = len(rows) - listHistoryCap
				rows = rows[len(rows)-listHistoryCap:]
			}
			now := deps.now()
			out := make([]map[string]any, 0, len(rows))
			live := 0
			for _, r := range rows {
				if !r.Status.IsTerminal() {
					live++
				}
				entry := map[string]any{
					"asyncId":     r.ID,
					"tool":        r.ToolName,
					"title":       r.Title,
					"status":      string(r.Status),
					"terminalIds": parseIDs(r.TerminalIdsJson),
					"ageMs":       now - r.CreatedAt,
					"expiresAt":   r.ExpiresAt,
				}
				if r.Command != nil {
					entry["command"] = *r.Command
				}
				if r.OutcomesJson != nil {
					entry["outcomes"] = json.RawMessage(*r.OutcomesJson)
				}
				if r.LastError != nil {
					entry["lastError"] = *r.LastError
				}
				out = append(out, entry)
			}
			result := map[string]any{
				"invocations": out,
				"count":       len(out),
				"live":        live,
			}
			summary := fmt.Sprintf("%d async operation(s) (%d live).", len(out), live)
			if truncatedRows > 0 {
				result["truncated"] = truncatedRows
				summary = fmt.Sprintf("%d async operation(s) (%d live; %d older finished rows omitted).", len(out), live, truncatedRows)
			}
			return tools.Ok(summary, result)
		},
	}
}

/* -------------------------------- async.cancel ---------------------------- */

type cancelArgs struct {
	AsyncID string `json:"asyncId"`
}

func (a *cancelArgs) Validate() error {
	if strings.TrimSpace(a.AsyncID) == "" {
		return fmt.Errorf("asyncId is required")
	}
	return nil
}

var cancelSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "asyncId": { "type": "string", "description": "The asy_… id returned when the operation started (see async.list)." }
  },
  "required": ["asyncId"]
}`)

func newCancelTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "async.cancel",
		Description: "Stop tracking an asynchronous operation: the runtime stops polling it and will NOT wake you for it. The terminal and " +
			"whatever runs inside it are NOT touched — nothing is killed or closed. Use it when the user no longer cares about the result. " +
			"(Actually stopping the process is a separate, explicit user request — e.g. closing the terminal.)",
		Consequence: "Stops the background watch for this operation only; the terminal process keeps running untouched.",
		Risk:        domain.RiskLocal,
		Schema:      cancelSchema,
		Decode:      tools.StrictDecoder(func() any { return &cancelArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a cancelArgs
			_ = json.Unmarshal(raw, &a)
			id := strings.TrimSpace(a.AsyncID)
			rec, err := deps.Store.GetAsyncInvocation(id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "Failed to look up "+id+": "+err.Error())
			}
			if rec == nil {
				return tools.Fail(domain.CodeNotFound, "No async operation with id "+id+". See async.list for the ledger.")
			}
			if rec.Status.IsTerminal() {
				return tools.Ok(fmt.Sprintf("%s is already %s — nothing to cancel.", id, rec.Status), map[string]any{
					"asyncId": id, "status": string(rec.Status),
				})
			}
			// Claim first; deregister ONLY on a won claim. The claim guard is what
			// makes the race with an in-flight coordinator finalize benign (whoever
			// claims first wins) — but a LOST claim means the coordinator owns the
			// row now, possibly mid-publish-retry, so deregistering it here would
			// kill the retry and silently lose the completion event.
			ok, err := deps.Store.ClaimLiveAsyncInvocation(id, map[string]any{
				"status": string(domain.AsyncCancelled), "endedReason": "user_cancelled",
				"finishedAt": deps.now(),
			})
			if err != nil {
				return tools.Fail(domain.CodeInternal, "Failed to cancel "+id+": "+err.Error())
			}
			if !ok {
				// The coordinator finalized it between the read and the claim; its
				// completion is being (or has been) delivered through the queue.
				fresh, _ := deps.Store.GetAsyncInvocation(id)
				status := "finished"
				if fresh != nil {
					status = string(fresh.Status)
				}
				return tools.Ok(fmt.Sprintf("%s completed just before the cancel (now %s) — its completion is being delivered through the attention queue.", id, status), map[string]any{
					"asyncId": id, "status": status,
				})
			}
			if deps.Coordinator != nil {
				deps.Coordinator.Deregister(id)
			}
			return tools.Ok(fmt.Sprintf("Stopped monitoring async operation %s. The terminal process was NOT killed and keeps running.", id), map[string]any{
				"asyncId": id, "status": string(domain.AsyncCancelled),
			})
		},
	}
}

/* --------------------------------- helpers -------------------------------- */

// groupIDFor derives the sibling-coalescing group from the turn's run id
// (verbatim — the run_… id doubles as provenance), so async operations started
// in the same turn wake together. Empty when there is no run id (the storage
// layer then self-groups on the invocation id).
func groupIDFor(tctx *tools.ToolContext) string {
	if tctx == nil || tctx.RunID == "" {
		return ""
	}
	return tctx.RunID
}

// mustJSONIDs serializes a terminal-id list (always small, plain strings — a
// marshal error is impossible in practice; degrade to "[]").
func mustJSONIDs(ids []string) string {
	b, err := json.Marshal(ids)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// parseIDs decodes a stored terminal-id list leniently (a corrupt blob yields
// an empty list, never an error — list rendering must not fail on bad rows).
func parseIDs(idsJSON string) []string {
	var ids []string
	if json.Unmarshal([]byte(idsJSON), &ids) != nil {
		return []string{}
	}
	if ids == nil {
		return []string{}
	}
	return ids
}
