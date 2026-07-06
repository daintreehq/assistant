// Package timer holds the durable-timer tools: timer.schedule, timer.list,
// timer.cancel. Timers persist in SQLite and fire whenever a supervision
// engine is running — the open assistant OR the persistent supervisor daemon
// after it closes (missed occurrences catch up on the next tick). Every
// creator appends a lifecycle NOTE.
package timer

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

const (
	codeInvalidArgs    = "INVALID_ARGS"
	codeTimerFireAt    = "TIMER_FIRE_AT"
	codeTimerRepeatEnd = "TIMER_REPEAT_UNTIL"
	codeTimerNotFound  = "TIMER_NOT_FOUND"
)

// Store is the slice of storage the timer tools touch.
type Store interface {
	InsertTimer(ctx context.Context, rec domain.TimerRecord) (string, error)
	ListTimers(ctx context.Context, status string) ([]domain.TimerRecord, error)
	GetTimer(ctx context.Context, id string) (*domain.TimerRecord, error)
	UpdateTimerStatus(ctx context.Context, id, status string) error
	RevokeGrantsByActor(ctx context.Context, actorID string) (int, error)
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
        "type": { "type": "string", "enum": ["enqueue", "call_safe_tool"] },
        "message": { "type": "string" },
        "toolCall": {
          "type": "object",
          "additionalProperties": false,
          "required": ["toolName"],
          "properties": {
            "toolName": { "type": "string", "minLength": 1 },
            "args": { "type": "object", "additionalProperties": true }
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
		Name:        "timer.schedule",
		Description: "Schedule a durable timer that enqueues a reminder or runs a safe tool at a future time (optionally repeating).",
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
				newID, err := deps.Store.InsertTimer(context.Background(), rec)
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
		Description: "List scheduled (not-yet-fired) timers.",
		Risk:        domain.RiskRead,
		Schema:      schema,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			if deps.Store == nil {
				return tools.Ok("No timers (storage unavailable).", map[string]any{"timers": []any{}})
			}
			rows, err := deps.Store.ListTimers(context.Background(), "scheduled")
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
		Description: "Cancel a scheduled timer by id.",
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
			existing, err := deps.Store.GetTimer(context.Background(), a.ID)
			if err != nil || existing == nil {
				return tools.Fail(codeTimerNotFound, "timer.cancel: no such timer: "+a.ID, tools.Unrecoverable())
			}
			if err := deps.Store.UpdateTimerStatus(context.Background(), a.ID, "cancelled"); err != nil {
				return tools.Fail(domain.CodeInternal, "timer.cancel: "+err.Error())
			}
			// A cancelled timer keeps no grant — revoke any it held.
			_, _ = deps.Store.RevokeGrantsByActor(context.Background(), a.ID)
			return tools.Ok("Cancelled timer "+a.ID+".", map[string]any{"timerId": a.ID, "status": "cancelled"})
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
