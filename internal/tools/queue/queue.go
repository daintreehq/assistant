// Package queue holds the attention-queue tools: queue.publish, queue.digest,
// queue.resolve. All sub-threads publish here instead of interrupting the main
// thread; the human reads the digest.
package queue

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
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
    "source": { "type": "string", "enum": ["timer","terminal_watcher","worktree_watcher","pr_watcher","workflow","model_worker","system","user"] },
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
		Description: "Publish an event to the attention queue (the human's inbox). Use dedupeKey to collapse repeats.",
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
		Description: "Read a digest of the attention queue, optionally filtered by minimum severity.",
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
		Description: "Mark an attention-queue event resolved by id.",
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
