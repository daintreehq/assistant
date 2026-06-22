// Package workflow holds the workflow-ledger tools: workflow.create,
// workflow.get, workflow.list, workflow.update. The ledger tracks an end-to-end
// run (issue → branch → worktree → PR) so the assistant can resume and report on
// long-running work. Array fields serialize to JSON columns; an update REPLACES
// arrays wholesale. Reaching a terminal status stamps completedAt exactly once.
//
// Spec: docs/port/tools-families.md §4.12 (workflowTools.ts).
package workflow

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

const (
	codeInvalidArgs      = "INVALID_ARGS"
	codeWorkflowNotFound = "WORKFLOW_NOT_FOUND"

	// maxWorkflowList bounds workflow.list so a long-lived workspace's run history can't
	// return an unbounded tool result. The store orders by updatedAt DESC, so the cap keeps
	// the most-recently-active runs and truncates older ones (surfaced in the summary).
	maxWorkflowList = 100
)

// terminalStatuses stamp completedAt the first time they are reached.
var terminalStatuses = map[domain.WorkflowRunStatus]bool{
	domain.WorkflowDone:      true,
	domain.WorkflowCancelled: true,
	domain.WorkflowFailed:    true,
}

var validStatus = map[domain.WorkflowRunStatus]bool{
	domain.WorkflowPending:   true,
	domain.WorkflowActive:    true,
	domain.WorkflowBlocked:   true,
	domain.WorkflowDone:      true,
	domain.WorkflowCancelled: true,
	domain.WorkflowFailed:    true,
}

// Store is the slice of storage the workflow tools touch.
type Store interface {
	InsertWorkflowRun(ctx context.Context, rec domain.WorkflowRunRecord) (string, error)
	GetWorkflowRun(ctx context.Context, id string) (*domain.WorkflowRunRecord, error)
	ListWorkflowRuns(ctx context.Context, status string) ([]domain.WorkflowRunRecord, error)
	UpdateWorkflowRun(ctx context.Context, rec domain.WorkflowRunRecord) error
}

// Deps is the dependency set for the workflow family.
type Deps struct {
	Store Store
}

// Tools returns the workflow ledger tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newCreateTool(deps),
		newGetTool(deps),
		newListTool(deps),
		newUpdateTool(deps),
	}
}

// mutableFields are the create/update fields (everything but the id). Pointers
// distinguish "omitted" (leave alone on update) from "set to value".
type mutableFields struct {
	IssueNumber   *int                      `json:"issueNumber,omitempty"`
	IssueURL      *string                   `json:"issueUrl,omitempty"`
	IssueTitle    *string                   `json:"issueTitle,omitempty"`
	Branch        *string                   `json:"branch,omitempty"`
	WorktreeID    *string                   `json:"worktreeId,omitempty"`
	PRNumber      *int                      `json:"prNumber,omitempty"`
	PRURL         *string                   `json:"prUrl,omitempty"`
	TerminalIDs   []string                  `json:"terminalIds,omitempty"`
	WatcherIDs    []string                  `json:"watcherIds,omitempty"`
	QueueEventIDs []string                  `json:"queueEventIds,omitempty"`
	Status        *string                   `json:"status,omitempty"`
	NextAction    *domain.RecommendedAction `json:"nextAction,omitempty"`
	Notes         []string                  `json:"notes,omitempty"`
}

const fieldsSchemaProps = `
    "issueNumber": { "type": "integer" },
    "issueUrl": { "type": "string" },
    "issueTitle": { "type": "string" },
    "branch": { "type": "string" },
    "worktreeId": { "type": "string" },
    "prNumber": { "type": "integer" },
    "prUrl": { "type": "string" },
    "terminalIds": { "type": "array", "items": { "type": "string" } },
    "watcherIds": { "type": "array", "items": { "type": "string" } },
    "queueEventIds": { "type": "array", "items": { "type": "string" } },
    "status": { "type": "string", "enum": ["pending","active","blocked","done","cancelled","failed"] },
    "nextAction": {
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
    },
    "notes": { "type": "array", "items": { "type": "string" } }`

// --- workflow.create ---

var createSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {` + fieldsSchemaProps + `
  }
}`)

func newCreateTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.create",
		Description: "Create a workflow ledger entry tracking an end-to-end run (issue/branch/worktree/PR).",
		Risk:        domain.RiskLocal,
		Schema:      createSchema,
		Decode:      tools.StrictDecoder(func() any { return &mutableFields{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var f mutableFields
			if err := tools.DecodeStrict(args, &f); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.create: "+err.Error())
			}
			status := domain.WorkflowPending
			if f.Status != nil {
				status = domain.WorkflowRunStatus(*f.Status)
				if !validStatus[status] {
					return tools.Fail(codeInvalidArgs, "workflow.create: invalid status")
				}
			}
			now := domain.NowMS()
			rec := domain.WorkflowRunRecord{
				ID:        domain.NewID(domain.PrefixWorkflow),
				Status:    status,
				CreatedAt: now,
				UpdatedAt: now,
			}
			applyFields(&rec, &f)
			// A status BORN terminal stamps completedAt now.
			if terminalStatuses[status] {
				rec.CompletedAt = &now
			}
			id := rec.ID
			if deps.Store != nil {
				newID, err := deps.Store.InsertWorkflowRun(ctx, rec)
				if err != nil {
					return tools.Fail(domain.CodeInternal, "workflow.create: "+err.Error())
				}
				if newID != "" {
					id = newID
					rec.ID = newID
				}
			}
			return tools.Ok("Created workflow "+id+".", map[string]any{"id": id, "workflow": workflowView(&rec)})
		},
	}
}

// --- workflow.get ---

type idArgs struct {
	ID string `json:"id"`
}

var getSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": { "id": { "type": "string" } }
}`)

func newGetTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.get",
		Description: "Get a workflow ledger entry by id.",
		Risk:        domain.RiskRead,
		Schema:      getSchema,
		Decode:      tools.StrictDecoder(func() any { return &idArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a idArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.get: "+err.Error())
			}
			if a.ID == "" || deps.Store == nil {
				return tools.Fail(codeWorkflowNotFound, "workflow.get: no such workflow: "+a.ID, tools.Unrecoverable())
			}
			rec, err := deps.Store.GetWorkflowRun(ctx, a.ID)
			if err != nil || rec == nil {
				return tools.Fail(codeWorkflowNotFound, "workflow.get: no such workflow: "+a.ID, tools.Unrecoverable())
			}
			return tools.Ok("Workflow "+a.ID+".", map[string]any{"workflow": workflowView(rec)})
		},
	}
}

// --- workflow.list ---

type listArgs struct {
	Status string `json:"status,omitempty"`
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": { "status": { "type": "string", "enum": ["pending","active","blocked","done","cancelled","failed"] } }
}`)

func newListTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.list",
		Description: "List workflow ledger entries, optionally filtered by status.",
		Risk:        domain.RiskRead,
		Schema:      listSchema,
		Decode:      tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a listArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.list: "+err.Error())
			}
			if deps.Store == nil {
				return tools.Ok("No workflows (storage unavailable).", map[string]any{"workflows": []any{}})
			}
			rows, err := deps.Store.ListWorkflowRuns(ctx, a.Status)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.list: "+err.Error())
			}
			total := len(rows)
			if len(rows) > maxWorkflowList {
				rows = rows[:maxWorkflowList] // ORDER BY updatedAt DESC → keep the most recent
			}
			out := make([]map[string]any, 0, len(rows))
			for i := range rows {
				out = append(out, workflowView(&rows[i]))
			}
			summary := fmt.Sprintf("%d workflow(s).", len(out))
			if total > len(out) {
				summary = fmt.Sprintf("%d of %d workflow(s) (most recent shown; older truncated).", len(out), total)
			}
			return tools.Ok(summary, map[string]any{"workflows": out})
		},
	}
}

// --- workflow.update ---

type updateArgs struct {
	ID string `json:"id"`
	mutableFields
}

var updateSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string" },` + fieldsSchemaProps + `
  }
}`)

func newUpdateTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.update",
		Description: "Update a workflow ledger entry. Provided array fields REPLACE the stored arrays.",
		Risk:        domain.RiskLocal,
		Schema:      updateSchema,
		Decode:      tools.StrictDecoder(func() any { return &updateArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a updateArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for workflow.update: "+err.Error())
			}
			if a.ID == "" || deps.Store == nil {
				return tools.Fail(codeWorkflowNotFound, "workflow.update: no such workflow: "+a.ID, tools.Unrecoverable())
			}
			rec, err := deps.Store.GetWorkflowRun(ctx, a.ID)
			if err != nil || rec == nil {
				return tools.Fail(codeWorkflowNotFound, "workflow.update: no such workflow: "+a.ID, tools.Unrecoverable())
			}
			if a.Status != nil {
				status := domain.WorkflowRunStatus(*a.Status)
				if !validStatus[status] {
					return tools.Fail(codeInvalidArgs, "workflow.update: invalid status")
				}
			}
			now := domain.NowMS()
			applyFields(rec, &a.mutableFields)
			rec.UpdatedAt = now
			// Stamp completedAt on the FIRST transition to a terminal status.
			if a.Status != nil && terminalStatuses[rec.Status] && rec.CompletedAt == nil {
				rec.CompletedAt = &now
			}
			if err := deps.Store.UpdateWorkflowRun(ctx, *rec); err != nil {
				return tools.Fail(domain.CodeInternal, "workflow.update: "+err.Error())
			}
			return tools.Ok("Updated workflow "+a.ID+".", map[string]any{"workflow": workflowView(rec)})
		},
	}
}

// applyFields patches a record from the provided (pointer-gated) fields. Array
// fields REPLACE; nil pointers leave the field alone.
func applyFields(rec *domain.WorkflowRunRecord, f *mutableFields) {
	if f.IssueNumber != nil {
		rec.IssueNumber = f.IssueNumber
	}
	if f.IssueURL != nil {
		rec.IssueURL = f.IssueURL
	}
	if f.IssueTitle != nil {
		rec.IssueTitle = f.IssueTitle
	}
	if f.Branch != nil {
		rec.Branch = f.Branch
	}
	if f.WorktreeID != nil {
		rec.WorktreeID = f.WorktreeID
	}
	if f.PRNumber != nil {
		rec.PRNumber = f.PRNumber
	}
	if f.PRURL != nil {
		rec.PRURL = f.PRURL
	}
	if f.Status != nil {
		rec.Status = domain.WorkflowRunStatus(*f.Status)
	}
	if f.TerminalIDs != nil {
		rec.TerminalIdsJson = marshalStrings(f.TerminalIDs)
	}
	if f.WatcherIDs != nil {
		rec.WatcherIdsJson = marshalStrings(f.WatcherIDs)
	}
	if f.QueueEventIDs != nil {
		rec.QueueEventIdsJson = marshalStrings(f.QueueEventIDs)
	}
	if f.Notes != nil {
		rec.NotesJson = marshalStrings(f.Notes)
	}
	if f.NextAction != nil {
		nj, _ := json.Marshal(f.NextAction)
		s := string(nj)
		rec.NextActionJson = &s
	}
}

func marshalStrings(v []string) *string {
	b, _ := json.Marshal(v)
	s := string(b)
	return &s
}

// workflowView deserializes the JSON columns + parses nextAction (garbage → nil).
func workflowView(rec *domain.WorkflowRunRecord) map[string]any {
	view := map[string]any{
		"id":            rec.ID,
		"status":        rec.Status,
		"createdAt":     rec.CreatedAt,
		"updatedAt":     rec.UpdatedAt,
		"terminalIds":   unmarshalStrings(rec.TerminalIdsJson),
		"watcherIds":    unmarshalStrings(rec.WatcherIdsJson),
		"queueEventIds": unmarshalStrings(rec.QueueEventIdsJson),
		"notes":         unmarshalStrings(rec.NotesJson),
	}
	addPtr := func(k string, p any) {
		switch v := p.(type) {
		case *int:
			if v != nil {
				view[k] = *v
			}
		case *string:
			if v != nil {
				view[k] = *v
			}
		case *int64:
			if v != nil {
				view[k] = *v
			}
		}
	}
	addPtr("issueNumber", rec.IssueNumber)
	addPtr("issueUrl", rec.IssueURL)
	addPtr("issueTitle", rec.IssueTitle)
	addPtr("branch", rec.Branch)
	addPtr("worktreeId", rec.WorktreeID)
	addPtr("prNumber", rec.PRNumber)
	addPtr("prUrl", rec.PRURL)
	addPtr("completedAt", rec.CompletedAt)
	if rec.NextActionJson != nil {
		var act domain.RecommendedAction
		if err := json.Unmarshal([]byte(*rec.NextActionJson), &act); err == nil && act.ToolName != "" {
			view["nextAction"] = act
		}
	}
	return view
}

func unmarshalStrings(p *string) []string {
	out := []string{}
	if p != nil {
		_ = json.Unmarshal([]byte(*p), &out)
	}
	return out
}
