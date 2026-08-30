// Package queue holds the attention-queue tools: queue.publish, queue.digest,
// queue.resolve. All sub-threads publish here instead of interrupting the main
// thread; the human reads the digest.
package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

const (
	codeInvalidArgs   = "INVALID_ARGS"
	codeQueueNotFound = "QUEUE_NOT_FOUND"

	// queue.digest is bounded so an inbox that has accumulated many open events can never
	// return an unbounded tool result that busts the model's context budget. The caller may
	// ask for fewer; a larger ask is clamped, and no ask defaults to defaultDigestItems.
	defaultDigestItems = 50
	maxDigestItems     = 200
)

// Queue is the consumer-defined slice of the attention queue these tools drive.
// Format renders a human-readable digest; the others mutate/read the queue.
type Queue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
	Digest(ctx context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error)
	Resolve(ctx context.Context, id string) (bool, error)
	Format(events []domain.QueueEvent) string
}

// Deps is the dependency set for the queue family.
type Deps struct {
	Queue Queue
}

// Tools returns the queue tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newPublishTool(deps),
		newDigestTool(deps),
		newResolveTool(deps),
	}
}

// queueFrom prefers the explicit Deps.Queue; otherwise falls back to the
// ToolContext.Queue (which only exposes Publish). A nil result means unavailable.
func (d Deps) queue() Queue { return d.Queue }

// --- queue.publish ---
//
// NB: the model-facing JSON-schema intentionally OMITS epistemicKind from
// properties even though the decoder accepts it (the model never sets it;
// internal callers do).

var publishSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["source", "severity", "title", "summary"],
  "properties": {
    "source": { "type": "string", "enum": ["worktree_watcher","pr_watcher","workflow","model_worker","system","user"], "description": "Who is reporting. A note to the human — it never starts a turn. \"timer\" and \"terminal_watcher\" are reserved for the engine's own fires and checks." },
    "severity": { "type": "string", "enum": ["debug","info","attention","urgent","blocked","done","error"] },
    "title": { "type": "string" },
    "summary": { "type": "string" },
    "target": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "projectId": { "type": "string" },
        "worktreeId": { "type": "string" },
        "terminalId": { "type": "string" },
        "workflowRunId": { "type": "string" }
      }
    },
    "evidence": { "type": "array", "items": { "type": "string" } },
    "recommendedActions": {
      "type": "array",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "required": ["label","toolName"],
        "properties": {
          "label": { "type": "string" },
          "toolName": { "type": "string" },
          "args": { "type": "object", "additionalProperties": true },
          "risk": { "type": "string" },
          "requiresConfirmation": { "type": "boolean" }
        }
      }
    },
    "dedupeKey": { "type": "string" },
    "ttlMs": { "type": "number" }
  }
}`)

func newPublishTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "queue.publish",
		Description: "Publish ONE event to the human's attention inbox — how background work reports without interrupting the conversation. Use it when you must STOP leaving something the user needs to see later (a blocked step, a finished long run, an escalation from a wait you gave up on); in a live turn, just say it in your reply instead. Pass a stable dedupeKey so repeats collapse into one counted item.",
		Risk:        domain.RiskLocal,
		Schema:      publishSchema,
		Decode:      tools.StrictDecoder(func() any { return &domain.QueuePublishArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a domain.QueuePublishArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for queue.publish: "+err.Error())
			}
			if !a.Source.IsValid() || !a.Severity.IsValid() {
				return tools.Fail(codeInvalidArgs, "queue.publish: invalid source or severity")
			}
			if a.Title == "" || a.Summary == "" {
				return tools.Fail(codeInvalidArgs, "queue.publish: title and summary are required")
			}
			// REFUSE a source that would make this event start a turn.
			//
			// Two sources are actionable wakes — a terminal_watcher event carrying a
			// terminalId, and async_tool — and this tool decodes the same struct the
			// engine publishes with, so the model could name one and have its own text
			// delivered as a machine observation that costs a paid turn. Worse, that
			// turn is not a timer-message turn, so it carries none of the recursion
			// lineage: a timed message could publish itself a watcher wake and start the
			// cycle again through the side door. Reporting is what this tool is for;
			// manufacturing the engine's own signals is not.
			if isWakeActionableSource(a.Source) {
				return tools.Fail(codeInvalidArgs, fmt.Sprintf(
					"queue.publish: source %q is reserved for the engine's own fires and checks, because events from it can "+
						"start an autonomous turn. Publish as \"system\" (or \"user\") to leave a note, or do the work now.", a.Source),
					tools.Unrecoverable())
			}
			// STRIP the scheduled-message provenance. It is not part of this tool's
			// advertised schema, but strict decoding rejects fields unknown to the Go
			// STRUCT, not fields hidden from the model — and the struct is the same one
			// the scheduler publishes with. Left readable, any local tool call could
			// stamp timerMessage on an event it wrote itself and have the wake reactor
			// deliver its text as the user's own instruction, in a paid turn, with no
			// timer behind it. Only fireTimer may confer that meaning, so it is cleared
			// here rather than validated: there is no legitimate caller to accommodate.
			if a.Target != nil && (a.Target.TimerMessage || a.Target.TimerOccurrence != 0) {
				stripped := *a.Target
				stripped.TimerMessage = false
				stripped.TimerOccurrence = 0
				a.Target = &stripped
			}
			q := deps.queue()
			if q == nil {
				return tools.Fail(domain.CodeInternal, "queue.publish: queue unavailable")
			}
			ev, err := q.Publish(ctx, a)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "queue.publish: "+err.Error())
			}
			summary := fmt.Sprintf("Published %q to the inbox.", ev.Title)
			if ev.Count > 1 {
				// On dedupe the count bumps; surface (×N) so the model knows it collapsed.
				summary = fmt.Sprintf("Published %q to the inbox (×%d).", ev.Title, ev.Count)
			}
			return tools.Ok(summary, ev)
		},
	}
}

// --- queue.digest ---

type digestArgs struct {
	SeverityAtLeast string `json:"severityAtLeast,omitempty"`
	MaxItems        *int   `json:"maxItems,omitempty"`
	IncludeResolved bool   `json:"includeResolved,omitempty"`
}

var digestSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "severityAtLeast": { "type": "string", "enum": ["debug","info","attention","urgent","blocked","done","error"] },
    "maxItems": { "type": "integer", "minimum": 1 },
    "includeResolved": { "type": "boolean" }
  }
}`)

func newDigestTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "queue.digest",
		Description: "Read the human's attention inbox: open events newest-first (default 50, max 200), filter with severityAtLeast, add includeResolved for history. Returns the structured events plus a pre-rendered `text` digest you can relay. The inbox does NOT ride the turn context, so call this when the user asks what needs attention, or on a wake turn to see what was published while you were away.",
		Risk:        domain.RiskRead,
		Schema:      digestSchema,
		Decode:      tools.StrictDecoder(func() any { return &digestArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a digestArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for queue.digest: "+err.Error())
			}
			q := deps.queue()
			if q == nil {
				return tools.Fail(domain.CodeInternal, "queue.digest: queue unavailable")
			}
			opts := domain.QueueDigestOptions{IncludeResolved: a.IncludeResolved}
			if a.SeverityAtLeast != "" {
				sev := domain.Severity(a.SeverityAtLeast)
				if !sev.IsValid() {
					return tools.Fail(codeInvalidArgs, "queue.digest: invalid severityAtLeast")
				}
				opts.SeverityAtLeast = &sev
			}
			limit := defaultDigestItems
			if a.MaxItems != nil {
				if *a.MaxItems <= 0 {
					return tools.Fail(codeInvalidArgs, "queue.digest: maxItems must be a positive integer")
				}
				limit = *a.MaxItems
				if limit > maxDigestItems {
					limit = maxDigestItems // clamp, never an unbounded dump
				}
			}
			opts.MaxItems = &limit
			events, err := q.Digest(ctx, opts)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "queue.digest: "+err.Error())
			}
			text := q.Format(events)
			return tools.Ok(fmt.Sprintf("%d inbox item(s).", len(events)),
				map[string]any{"events": events, "text": text})
		},
	}
}

// --- queue.resolve ---

type resolveArgs struct {
	ID string `json:"id"`
}

var resolveSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newResolveTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "queue.resolve",
		Description: "Mark ONE attention-queue (inbox) event resolved by its id so it stops appearing in queue.digest and stops re-surfacing as open work. Call it AFTER you have actually acted on the item — resolving is the acknowledgement, not the fix. An unknown or already-resolved id fails QUEUE_NOT_FOUND (unrecoverable). Local bookkeeping only: it never touches terminals, watchers or async work.",
		Risk:        domain.RiskLocal,
		Schema:      resolveSchema,
		Decode:      tools.StrictDecoder(func() any { return &resolveArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a resolveArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for queue.resolve: "+err.Error())
			}
			if a.ID == "" {
				return tools.Fail(codeInvalidArgs, "queue.resolve: id is required")
			}
			q := deps.queue()
			if q == nil {
				return tools.Fail(domain.CodeInternal, "queue.resolve: queue unavailable")
			}
			resolved, err := q.Resolve(ctx, a.ID)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "queue.resolve: "+err.Error())
			}
			if !resolved {
				return tools.Fail(codeQueueNotFound, "queue.resolve: no such event: "+a.ID, tools.Unrecoverable())
			}
			return tools.Ok("Resolved "+a.ID+".", map[string]any{"id": a.ID, "resolved": resolved})
		},
	}
}

// isWakeActionableSource reports whether a source can produce an event the wake
// reactors will act on (see agent.IsActionableWake). Kept as a small named list rather
// than importing the agent package, which would be a dependency cycle — the pairing is
// asserted by a test in internal/agent that fails if the two ever disagree.
func isWakeActionableSource(s domain.EventSource) bool {
	return s == domain.SourceTerminalWatcher || s == domain.SourceAsyncTool || s == domain.SourceTimer
}
