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
)

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

// Deps is the dependency set for the timer family.
type Deps struct {
	Store Store
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
          "enum": ["enqueue", "call_safe_tool"],
          "description": "\"enqueue\" posts an inbox item and runs nothing; \"call_safe_tool\" dispatches toolCall.toolName. A timer's own event never wakes you. Success files info (below the deck's attention filter), an ordinary failure error, a confirmation-required denial blocked."
        },
        "message": { "type": "string", "description": "Reminder text for \"enqueue\" (defaults to the timer title). Ignored by \"call_safe_tool\"." },
        "toolCall": {
          "type": "object",
          "additionalProperties": false,
          "required": ["toolName"],
          "description": "Required by \"call_safe_tool\", ignored by \"enqueue\".",
          "properties": {
            "toolName": { "type": "string", "minLength": 1, "description": "Exact registered tool name — any tool in your inventory, not a restricted subset. Use \"agentTask.spawnForEdits\" to spawn a terminal at fire time." },
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
		Description: "Schedule a durable timer that fires once (fireAt ISO-8601 or delayMs) or repeats (repeat.everyMs plus maxRuns/until). payload.type \"call_safe_tool\" runs toolCall.toolName — ANY registered tool, not a safe subset (tier/confirm gates still apply): use agentTask.spawnForEdits to spawn AT fire time, then grant.create (actorType \"timer\", actorId = the returned timerId) for a grantable confirm-required target. \"enqueue\" only posts message and runs nothing. A timer's own event never wakes you. Timers persist after the assistant closes; missed occurrences catch up. Returns timerId.",
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
			if a.Payload.Type != "enqueue" && a.Payload.Type != "call_safe_tool" {
				return tools.Fail(codeInvalidArgs, "timer.schedule: payload.type must be enqueue|call_safe_tool")
			}
			if a.Payload.Type == "call_safe_tool" {
				if a.Payload.ToolCall == nil || strings.TrimSpace(a.Payload.ToolCall.ToolName) == "" {
					return tools.Fail(codeInvalidArgs, "timer.schedule: call_safe_tool payload requires toolCall.toolName")
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
				fmt.Sprintf("Scheduled timer %q for %s.%s", a.Title, fireISO, lifecycleNote(active)),
				map[string]any{"timerId": id, "fireAt": fireISO, "daemonActive": active},
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
