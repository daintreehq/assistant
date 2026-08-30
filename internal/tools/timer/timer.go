// Package timer holds the durable-timer tools: timer.schedule, timer.list,
// timer.cancel. Timers persist in SQLite and fire whenever a supervision
// engine is running — the open assistant OR the persistent supervisor daemon
// after it closes (missed occurrences catch up on the next tick). Every
// creator appends a lifecycle NOTE.
package timer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/timers"
	"github.com/daintreehq/assistant/internal/tools"
)

const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeTimerFireAt    = "TIMER_FIRE_AT"
	codeTimerRepeatEnd = "TIMER_REPEAT_UNTIL"
	codeTimerNotFound  = "TIMER_NOT_FOUND"
	// The payload is well-formed but could never run at fire time.
	codeTimerUnrunnable = "TIMER_PAYLOAD_UNRUNNABLE"
	// A scheduled message tried to schedule another one.
	codeTimerMessageRecursion = "TIMER_MESSAGE_RECURSION"
)

// minMessageRepeatMs is the floor between two firings of a repeating message. One
// minute, because the scheduler ticks every three seconds and each fire costs a model
// turn — anything faster is a spend loop wearing a schedule's clothing.
const minMessageRepeatMs = 60_000

// maxRepeatEveryMs caps a repeat interval at roughly a year. The point is not the
// calendar, it is that `now + everyMs` must not overflow int64: a wrapped next-fire is
// stored as a negative timestamp, which every due check reads as "overdue for ever".
const maxRepeatEveryMs int64 = 366 * 24 * 60 * 60 * 1000

// maxMessageRuns caps how many times a repeating message may run. Each run is a full
// paid turn, so this is a spending limit, not a scheduling one — a nightly check for a
// year is well inside it, and anything past it is a number nobody chose deliberately.
const maxMessageRuns = 500

// isTimerToolName reports whether a name refers to the timer family's own scheduler,
// in either spelling the model might write.
func isTimerToolName(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	n = strings.ReplaceAll(n, "__", ".")
	return n == "timer.schedule"
}

const ()

// Store is the slice of storage the timer tools touch.
//
// It embeds timers.Store so this tool family and every host surface retire a
// timer through the SAME operation (internal/timers.Cancel). Cancelling is two
// writes — retire the schedule row, revoke the grants scoped to it — and a
// second implementation of that pair is how a live grant ends up outliving the
// timer it was minted for.
type Store interface {
	timers.Store
	InsertTimer(rec domain.TimerRecord) (string, error)
}

// ScheduledCall is what a scheduling surface needs in order to store a tool call
// that will run with nobody present: the name dispatch will resolve, the arguments
// to persist in place of the ones written, a one-line note naming anything that was
// filled in on the caller's behalf (empty when nothing was), and a refusal that is
// non-empty only when the call could not be made runnable at all.
type ScheduledCall struct {
	ToolName string
	Args     json.RawMessage
	Note     string
	Refusal  string
}

// Deps is the dependency set for the timer family.
type Deps struct {
	Store Store
	// PrepareScheduledCall makes a scheduled tool call storable: it resolves the name
	// DISPATCH will actually look up, fills in what the call needs and only this turn
	// can supply, and refuses what it could not repair. Backed by the registry; nil ⇒
	// every step is skipped and the call is stored exactly as written, so a stripped
	// test context schedules as it always did.
	//
	// Canonicalizing is a third of the job and not an afterthought. Fire-time dispatch
	// looks a tool up by its exact internal name and resolves nothing, so a payload
	// written in the wire spelling — or with stray whitespace — is stored happily and
	// dies with UNKNOWN_TOOL hours later. A check that could FIND the tool while
	// dispatch could not would be the worst of both: it would pass judgement on a call
	// that was never going to run under that name anyway.
	//
	// REPAIRING is the other two thirds, and it is why this returns args at all. A
	// scheduled call is written inside a turn and runs without one, so the fields a
	// tool would have inferred from "here" have to be captured while "here" still
	// exists. Storing the repaired args — not the ones the model typed — is what makes
	// the row runnable; see tools.Tool.PrepareUnattended for why the alternative,
	// bouncing the call back for the model to complete, freezes the same values one
	// round trip later and adds a guess.
	//
	// A seam rather than a *tools.Registry because the registry does not exist yet when
	// this family is constructed — the tools are what is being built. The closure
	// resolves at SCHEDULE time, by which point it does.
	PrepareScheduledCall func(toolName string, args json.RawMessage) ScheduledCall
}

// Tools returns the timer tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newScheduleTool(deps),
		newListTool(deps),
		newCancelTool(deps),
	}
}

// daemonActive reports whether the scheduler is running (nil ⇒ assume active).
func daemonActive(tctx *tools.ToolContext) bool {
	if tctx.DaemonActive == nil {
		return true
	}
	return tctx.DaemonActive()
}

// lifecycleNote is the durability NOTE appended to every schedule summary.
// The text differs when no scheduler is running right now (one-shot mode).
func lifecycleNote(active bool) string {
	if active {
		return " NOTE: this timer persists and keeps firing after the assistant closes — the background supervisor owns the schedule; missed occurrences catch up on the next tick."
	}
	return " NOTE: no scheduler is running in this one-shot invocation; the timer persists and fires once the assistant (or its background supervisor) next runs."
}

// resolvedFor renders what the schedule path filled in for the model, or "" when it
// filled in nothing. It reads as a clause of the summary sentence rather than a
// separate line, because a repair is part of what was scheduled and not an aside.
func resolvedFor(note string) string {
	if strings.TrimSpace(note) == "" {
		return ""
	}
	return " Resolved for you: " + note + "."
}

// --- timer.schedule ---

// timerRepeat is the optional repeat block.
type timerRepeat struct {
	EveryMs int64  `json:"everyMs"`
	MaxRuns *int   `json:"maxRuns,omitempty"`
	Until   string `json:"until,omitempty"` // ISO-8601
}

// timerTarget scopes a timer to a Daintree object (strict).
type timerTarget struct {
	ProjectID     string `json:"projectId,omitempty"`
	WorktreeID    string `json:"worktreeId,omitempty"`
	TerminalID    string `json:"terminalId,omitempty"`
	WorkflowRunID string `json:"workflowRunId,omitempty"`
}

// timerPayload is the discriminated payload union (on "type"). run_check is no
// longer creatable (legacy rows still fire as a plain reminder).
type timerPayload struct {
	Type     string         `json:"type"` // enqueue | call_safe_tool
	Message  string         `json:"message,omitempty"`
	ToolCall *timerToolCall `json:"toolCall,omitempty"`
}

type timerToolCall struct {
	ToolName string         `json:"toolName"`
	Args     map[string]any `json:"args,omitempty"`
}

type scheduleArgs struct {
	Title   string       `json:"title"`
	FireAt  string       `json:"fireAt,omitempty"`  // ISO-8601
	DelayMs *int64       `json:"delayMs,omitempty"` // >0
	Repeat  *timerRepeat `json:"repeat,omitempty"`
	Payload timerPayload `json:"payload"`
	Target  *timerTarget `json:"target,omitempty"`
}

var scheduleSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["title", "payload"],
  "properties": {
    "title": { "type": "string" },
    "fireAt": { "type": "string", "format": "date-time", "description": "ISO-8601 absolute fire time." },
    "delayMs": { "type": "integer", "minimum": 1, "description": "Relative delay from now in ms (alternative to fireAt)." },
    "repeat": {
      "type": "object",
      "additionalProperties": false,
      "required": ["everyMs"],
      "properties": {
        "everyMs": { "type": "integer", "minimum": 1 },
        "maxRuns": { "type": "integer", "minimum": 1 },
        "until": { "type": "string", "format": "date-time" }
      }
    },
    "payload": {
      "type": "object",
      "additionalProperties": false,
      "required": ["type"],
      "properties": {
        "type": {
          "type": "string",
          "enum": ["enqueue", "message", "call_safe_tool"],
          "description": "\"message\" delivers payload.message to YOU at fire time and you act on it then — the choice for any deferred instruction. \"enqueue\" posts an inbox note for the human and runs nothing, waking nobody. \"call_safe_tool\" dispatches one fixed toolCall.toolName."
        },
        "message": { "type": "string", "description": "For \"message\", the instruction delivered to you at fire time — write it as the user would, e.g. \"Send npm test to the build terminal and report the result\"; REQUIRED. For \"enqueue\", the human-facing reminder text (defaults to the title). Ignored by \"call_safe_tool\"." },
        "toolCall": {
          "type": "object",
          "additionalProperties": false,
          "required": ["toolName"],
          "description": "Required by \"call_safe_tool\", ignored by \"enqueue\".",
          "properties": {
            "toolName": { "type": "string", "minLength": 1, "description": "Exact registered tool name — almost any tool in your inventory, not a restricted subset, but NOT \"timer.schedule\" (a timer cannot schedule a timer). Use \"agentTask.spawnForEdits\" to spawn a terminal at fire time." },
            "args": { "type": "object", "additionalProperties": true, "description": "Arguments passed to toolName; omitted becomes {}." }
          }
        }
      }
    },
    "target": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "projectId": { "type": "string" },
        "worktreeId": { "type": "string" },
        "terminalId": { "type": "string" },
        "workflowRunId": { "type": "string" }
      }
    }
  }
}`)

func newScheduleTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name: "timer.schedule",
		// "call_safe_tool" is named first and the "not a safe subset" correction is kept
		// inside the first ~256 bytes on purpose: that prefix is what the model sees while
		// scanning its tool index, and believing the name was a curated allowlist is the
		// exact misreading this description exists to kill (issue #333). Sentence 1 is
		// byte-identical to the previous text because capabilityref's firstSentence() feeds
		// docs/generated/TOOLS.md from it — changing it would strand the generated doc.
		//
		// Two deliberate hedges, both load-bearing: the gates parenthetical, because
		// "ANY registered tool" describes what DISPATCHES, not what is permitted to run;
		// and "grantable", because a confirm-required tool can also be ungrantable
		// (daintree.call), where grant.create cannot unblock it. "A timer's OWN event"
		// is scoped on purpose too — a dispatched tool may create a watcher or async
		// operation whose later event does wake, so the flat "timers never wake you"
		// would be false. 580 runes, inside toolbudget_test's 600 ordinary budget.
		Description: "Schedule a durable timer that fires once (delayMs) or repeats (repeat.everyMs plus maxRuns/until). To do something LATER — \"in 25 minutes send npm test to the build terminal\", \"start a timer then spawn an agent\" — use payload.type \"message\" with the instruction in payload.message: it reaches you at fire time and you act on it THEN, so do not also do it now. \"enqueue\" posts a note for the human, runs nothing, and catches up if missed. \"call_safe_tool\" runs one named tool with fixed args. A \"message\" over an hour overdue is reported missed, not run late. Prefer delayMs. Returns timerId.",
		Risk:        domain.RiskLocal,
		Schema:      scheduleSchema,
		Decode:      tools.StrictDecoder(func() any { return &scheduleArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a scheduleArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for timer.schedule: "+err.Error())
			}
			if strings.TrimSpace(a.Title) == "" {
				return tools.Fail(codeInvalidArgs, "timer.schedule: title is required")
			}
			if a.Payload.Type != "enqueue" && a.Payload.Type != "call_safe_tool" && a.Payload.Type != "message" {
				return tools.Fail(codeInvalidArgs, "timer.schedule: payload.type must be enqueue|message|call_safe_tool")
			}
			// THE LOOP CUT, and it covers every payload type rather than just "message".
			//
			// Narrowing it to messages left the cycle open through a longer route: a
			// message turn could schedule a repeating call_safe_tool whose target is
			// timer.schedule itself, and each firing would mint the next message. The
			// outer call was not a message, so it passed; the inner one runs under the
			// daemon's own context, which carries no turn flag at all. A timed message
			// therefore schedules NOTHING — do the work now, or say what is blocked.
			if tctx != nil && (tctx.FromWake || tctx.FromTimerMessage) {
				// Gated on ANY autonomous turn, not just a scheduled message.
				//
				// Lineage does not survive a hop: a timed message that starts an async
				// wait sheds its own marker at the completion wake, and that turn was
				// then free to schedule again — a cycle with one extra step in it. Every
				// descendant of an autonomous turn is itself autonomous, so gating on
				// that closes the whole class instead of chasing a tag through async
				// completions, watcher digests, and whatever is added next.
				//
				// The user is never affected: they are interactive by definition, and
				// they are the only one who ever asks for a timer.
				return tools.Fail(codeTimerMessageRecursion,
					"timer.schedule: this turn is running autonomously (a scheduled message, a watcher, or an async "+
						"completion), and an autonomous turn cannot schedule a timer. Do the work now, or say what is "+
						"blocked and leave it for the user.",
					tools.Unrecoverable())
			}
			if a.Payload.Type == "message" && strings.TrimSpace(a.Payload.Message) == "" {
				return tools.Fail(codeInvalidArgs,
					"timer.schedule: a \"message\" payload requires payload.message — the instruction to carry out when it fires")
			}
			// Anything PrepareScheduledCall filled in on the model's behalf, phrased for
			// the summary. Disclosure is the other half of repairing silently: a value
			// resolved from the turn is a value the user can see is wrong NOW, while the
			// timer is still trivially cancellable, rather than at fire time.
			resolvedNote := ""
			if a.Payload.Type == "call_safe_tool" {
				if a.Payload.ToolCall == nil || strings.TrimSpace(a.Payload.ToolCall.ToolName) == "" {
					return tools.Fail(codeInvalidArgs, "timer.schedule: call_safe_tool payload requires toolCall.toolName")
				}
				// Refuse a payload that is already known to be unrunnable, HERE, where
				// the model can still fix it and a human is still watching. Scheduling
				// it instead buys a confident "Scheduled." now and a failure hours later
				// in a queue row nobody has open — the exact shape of the bug this
				// guards: a timer-dispatched spawn that named no worktree reported
				// success at schedule time and died on its only firing.
				name := strings.TrimSpace(a.Payload.ToolCall.ToolName)
				// A timer that schedules a timer is a machine for spending money, and no
				// real request needs it: "do X later" is one timer, not a chain. Blocked
				// by NAME here as well as by the turn flag above, because the two guards
				// fail in different places — the flag cannot reach a payload dispatched
				// later by the daemon, and the name cannot catch a turn that calls
				// timer.schedule directly.
				if isTimerToolName(name) {
					return tools.Fail(codeTimerMessageRecursion,
						"timer.schedule: a timer cannot schedule another timer. Put the work itself in the payload, "+
							"or use payload.type \"message\" and decide what to do when it fires.",
						tools.Unrecoverable())
				}
				if deps.PrepareScheduledCall != nil {
					// nil/absent args are `{}` here for the same reason dispatch treats
					// them as `{}`: the check has to see what the handler will see, and
					// a nil map marshals to `null`, which no decoder accepts.
					argsJSON := []byte("{}")
					if a.Payload.ToolCall.Args != nil {
						if raw, err := json.Marshal(a.Payload.ToolCall.Args); err == nil && len(raw) > 0 {
							argsJSON = raw
						}
					}
					prepared := deps.PrepareScheduledCall(name, argsJSON)
					if prepared.Refusal != "" {
						return tools.Fail(codeTimerUnrunnable,
							fmt.Sprintf("timer.schedule: this %s call cannot be scheduled because %s.", name, prepared.Refusal),
							tools.Unrecoverable())
					}
					// Persist the name dispatch will resolve, not the one that was
					// typed. Storing the wire spelling is how a payload that passed
					// every check still fails at fire time with UNKNOWN_TOOL.
					if prepared.ToolName != "" {
						a.Payload.ToolCall.ToolName = prepared.ToolName
					}
					// Persist the REPAIRED args for the same reason: what runs at fire
					// time is this row, so a value the turn supplied has to be in it.
					// A repair that cannot be read back is dropped rather than stored
					// half-applied — the unrepaired call is the one that was graded.
					if len(prepared.Args) > 0 {
						var merged map[string]any
						if err := json.Unmarshal(prepared.Args, &merged); err == nil {
							a.Payload.ToolCall.Args = merged
							resolvedNote = prepared.Note
						}
					}
				}
			}

			// fireAt = parse(fireAt) ?? now+delayMs ?? NaN → TIMER_FIRE_AT.
			now := domain.NowMS()
			var fireAt int64
			switch {
			case a.FireAt != "":
				t, err := parseISO(a.FireAt)
				if err != nil {
					return tools.Fail(codeTimerFireAt, "timer.schedule: fireAt is not a valid ISO-8601 datetime", tools.Unrecoverable())
				}
				fireAt = t.UnixMilli()
			case a.DelayMs != nil:
				if *a.DelayMs <= 0 {
					return tools.Fail(codeInvalidArgs, "timer.schedule: delayMs must be a positive integer")
				}
				// Same wrap hazard as a repeat interval: a delay near MaxInt64 makes
				// now+delayMs negative, and a negative fireAt reads as permanently
				// overdue — the timer fires immediately and on every tick after.
				if *a.DelayMs > maxRepeatEveryMs {
					return tools.Fail(codeInvalidArgs, fmt.Sprintf(
						"timer.schedule: delayMs must be at most %d ms (about a year); got %d",
						maxRepeatEveryMs, *a.DelayMs), tools.Unrecoverable())
				}
				fireAt = now + *a.DelayMs
			default:
				return tools.Fail(codeTimerFireAt, "timer.schedule: provide fireAt or delayMs", tools.Unrecoverable())
			}

			rec := domain.TimerRecord{
				ID:          domain.NewID(domain.PrefixTimer),
				Title:       a.Title,
				FireAt:      fireAt,
				PayloadType: a.Payload.Type,
				Status:      "scheduled",
				CreatedAt:   now,
			}
			payloadJSON, _ := json.Marshal(a.Payload)
			rec.PayloadJson = string(payloadJSON)

			if a.Repeat != nil {
				if a.Repeat.EveryMs <= 0 {
					return tools.Fail(codeInvalidArgs, "timer.schedule: repeat.everyMs must be a positive integer")
				}
				// A repeating MESSAGE starts a paid model turn on every fire, which the
				// other two payloads do not: an enqueue posts a row and a call_safe_tool
				// runs one local dispatch. Unbounded, `everyMs:1` would wake the model on
				// every three-second scheduler tick, for as long as the project exists,
				// with nobody watching. Both bounds are required because either alone
				// still permits that: a fast repeat with a cap burns the cap in seconds,
				// and a slow repeat without one never stops.
				// A repeat far enough in the future is indistinguishable from no repeat,
				// and one large enough to overflow is WORSE than no repeat: now+everyMs
				// wraps negative, the row reads as permanently overdue, and it fires on
				// every three-second tick for ever — the exact opposite of what the
				// number asked for.
				if a.Repeat.EveryMs > maxRepeatEveryMs {
					return tools.Fail(codeInvalidArgs, fmt.Sprintf(
						"timer.schedule: repeat.everyMs must be at most %d ms (about a year); got %d",
						maxRepeatEveryMs, a.Repeat.EveryMs), tools.Unrecoverable())
				}
				// The limits below apply to every payload that COSTS something per fire,
				// which is both "message" and "call_safe_tool" — not messages alone.
				//
				// Scoping them to messages left the same spend loop one step away: a
				// call_safe_tool repeating every millisecond can target a tool that calls
				// the model itself (an instructed terminal.extract), or register an async
				// wait whose completion is a full paid wake. "enqueue" is deliberately
				// exempt: it writes one inbox row and runs nothing, so a fast unbounded
				// reminder costs nothing but the row.
				if a.Payload.Type == "message" || a.Payload.Type == "call_safe_tool" {
					if a.Repeat.EveryMs < minMessageRepeatMs {
						return tools.Fail(codeInvalidArgs, fmt.Sprintf(
							"timer.schedule: a repeating %q must be at least %ds apart (each fire can cost a model call); got %dms",
							a.Payload.Type, minMessageRepeatMs/1000, a.Repeat.EveryMs), tools.Unrecoverable())
					}
					if a.Repeat.MaxRuns == nil && a.Repeat.Until == "" {
						return tools.Fail(codeInvalidArgs,
							"timer.schedule: a repeating "+a.Payload.Type+" must be bounded — set repeat.maxRuns or repeat.until, "+
								"or it keeps running forever.", tools.Unrecoverable())
					}
					// A bound has to BIND. "maxRuns: 4000000000" and
					// "until: 9999-12-31" both satisfy the rule above while describing
					// billions of paid turns, which is the thing the rule exists to
					// prevent — a limit nobody would ever reach is not a limit.
					if a.Repeat.MaxRuns != nil && *a.Repeat.MaxRuns > maxMessageRuns {
						return tools.Fail(codeInvalidArgs, fmt.Sprintf(
							"timer.schedule: a repeating %q may run at most %d times (each run can cost a model call); got %d",
							a.Payload.Type, maxMessageRuns, *a.Repeat.MaxRuns), tools.Unrecoverable())
					}
					if a.Repeat.MaxRuns == nil && a.Repeat.Until != "" {
						until, err := parseISO(a.Repeat.Until)
						if err == nil {
							// Ceiling, not floor: a fire is allowed to land exactly ON
							// `until`, so integer division admitted one more run than it
							// counted at the boundary.
							span := until.UnixMilli() - now
							if runs := (span + a.Repeat.EveryMs) / a.Repeat.EveryMs; runs > int64(maxMessageRuns) {
								return tools.Fail(codeInvalidArgs, fmt.Sprintf(
									"timer.schedule: that repeat.until spans more than %d firings at %dms apart. "+
										"Shorten the window, slow the repeat, or set repeat.maxRuns.",
									maxMessageRuns, a.Repeat.EveryMs), tools.Unrecoverable())
							}
						}
					}
				}
				every := a.Repeat.EveryMs
				rec.RepeatEveryMs = &every
				rec.MaxRuns = a.Repeat.MaxRuns
				if a.Repeat.Until != "" {
					t, err := parseISO(a.Repeat.Until)
					if err != nil {
						return tools.Fail(codeTimerRepeatEnd, "timer.schedule: repeat.until is not a valid ISO-8601 datetime", tools.Unrecoverable())
					}
					ms := t.UnixMilli()
					rec.RepeatUntil = &ms
				}
			}
			if a.Target != nil {
				tj, _ := json.Marshal(a.Target)
				ts := string(tj)
				rec.TargetJson = &ts
			}

			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertTimer(rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "timer.schedule: "+err.Error())
				}
				if newID != "" {
					id = newID
				}
			}

			active := daemonActive(tctx)
			fireISO := time.UnixMilli(fireAt).UTC().Format(time.RFC3339)
			return tools.Ok(
				fmt.Sprintf("Scheduled timer %q for %s.%s%s", a.Title, fireISO, resolvedFor(resolvedNote), lifecycleNote(active)),
				map[string]any{"timerId": id, "fireAt": fireISO, "daemonActive": active, "resolved": resolvedNote},
			)
		},
	}
}

// --- timer.list ---

func newListTool(deps Deps) *tools.Tool {
	schema, _ := json.Marshal(tools.NoArgs)
	return &tools.Tool{
		Name:        "timer.list",
		Description: "List the timers still SCHEDULED (not yet fired, not cancelled): id, title, fireAt (RFC3339 UTC), payloadType, runCount and any repeat settings. Timers do NOT ride the turn context, so this is the only way to see what is pending — call it before scheduling a near-duplicate reminder, when the user asks what is scheduled, or to get a tmr_… id for timer.cancel.",
		Risk:        domain.RiskRead,
		Schema:      schema,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			if deps.Store == nil {
				return tools.Ok("No timers (storage unavailable).", map[string]any{"timers": []any{}})
			}
			rows, err := deps.Store.ListTimers(timers.StatusScheduled)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "timer.list: "+err.Error())
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				r := &rows[i]
				view := map[string]any{
					"id":          r.ID,
					"title":       r.Title,
					"fireAt":      time.UnixMilli(r.FireAt).UTC().Format(time.RFC3339),
					"runCount":    r.RunCount,
					"payloadType": r.PayloadType,
				}
				if r.RepeatEveryMs != nil {
					view["repeatEveryMs"] = *r.RepeatEveryMs
				}
				if r.MaxRuns != nil {
					view["maxRuns"] = *r.MaxRuns
				}
				out = append(out, view)
			}
			return tools.Ok(fmt.Sprintf("%d scheduled timer(s).", len(out)), map[string]any{"timers": out})
		},
	}
}

// --- timer.cancel ---

type cancelArgs struct {
	ID string `json:"id"`
}

var cancelSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newCancelTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "timer.cancel",
		Description: "Cancel a scheduled timer by its tmr_… id (from timer.list): it never fires again and any automation grant held by that timer actor is revoked. Use it when the reminder or scheduled tool call is no longer wanted. An unknown id fails TIMER_NOT_FOUND (unrecoverable). Local bookkeeping only — it never touches terminals or project state. The result reports revokedGrants (live grants that cascade withdrew, 0 if none) — they need no follow-up grant.revoke unless grantRevokeFailed is true.",
		Risk:        domain.RiskLocal,
		Schema:      cancelSchema,
		Decode:      tools.StrictDecoder(func() any { return &cancelArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a cancelArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for timer.cancel: "+err.Error())
			}
			if a.ID == "" {
				return tools.Fail(codeInvalidArgs, "timer.cancel: id is required")
			}
			if deps.Store == nil {
				return tools.Fail(codeTimerNotFound, "timer.cancel: no such timer: "+a.ID, tools.Unrecoverable())
			}
			// The retire-and-revoke pair lives in internal/timers so this tool and the
			// host's own cancel surface cannot drift on it. A cascade that only one of
			// them performed would leave standing unattended authority behind whichever
			// route the user happened to take.
			out, err := timers.Cancel(deps.Store, a.ID, domain.NowMS())
			if errors.Is(err, timers.ErrNotFound) {
				return tools.Fail(codeTimerNotFound, "timer.cancel: no such timer: "+a.ID, tools.Unrecoverable())
			}
			if err != nil {
				return tools.Fail(domain.CodeInternal, "timer.cancel: "+err.Error())
			}
			revokedGrants, revokeErr := out.RevokedGrants, out.GrantRevokeErr
			result := map[string]any{
				"timerId": a.ID, "status": "cancelled",
				"revokedGrants": revokedGrants, "grantRevokeFailed": revokeErr != nil,
			}
			summary := "Cancelled timer " + a.ID + "."
			if out.AlreadyInactive {
				// Saying "cancelled" here would be a lie the model then repeats to the
				// user: the timer had already run or been retired, so this call stopped
				// nothing. The grant sweep above still ran, which is the part that can
				// genuinely have work left to do.
				result["status"] = out.PriorStatus
				result["alreadyInactive"] = true
				summary = "Timer " + a.ID + " was already " + out.PriorStatus + " — nothing to cancel."
			}
			if revokeErr != nil {
				// The timer row is already cancelled, so failing the whole call would be the
				// bigger lie. But a bare revokedGrants:0 reads as "nothing to clean up" when a
				// grant may still be LIVE, so the failure has to be said out loud — otherwise
				// this fix would just trade one misleading signal for another.
				summary += " Its automation grants could NOT be revoked (" + revokeErr.Error() + ") — revoke them with grant.revoke."
			}
			return tools.Ok(summary, result)
		},
	}
}

// parseISO parses an ISO-8601 datetime, accepting RFC3339 with/without fractional
// seconds (the common JS Date.parse inputs).
func parseISO(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}
