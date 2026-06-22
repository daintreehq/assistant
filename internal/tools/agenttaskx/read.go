package agenttaskx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// listDefaultLimit caps agentTask.list to the newest N sagas. A session rarely
// spawns more than a handful of agents, so 20 gives plenty of headroom without
// flooding the model's context.
const listDefaultLimit = 20

// launchView is the model-facing projection of an AgentLaunchRecord. It drops
// the internal saga plumbing (idempotencyKey, the launch name) and surfaces the
// fields useful for a status check: which agent/worktree/terminal the launch
// landed on, the saga stage, and any failure detail.
type launchView struct {
	ID           string                  `json:"id"`
	Stage        domain.AgentLaunchStage `json:"stage"`
	Mode         string                  `json:"mode"`
	Title        string                  `json:"title"`
	AgentID      string                  `json:"agentId"`
	WorktreeID   string                  `json:"worktreeId,omitempty"`
	TerminalID   string                  `json:"terminalId,omitempty"`
	WatcherID    string                  `json:"watcherId,omitempty"`
	ErrorCode    string                  `json:"errorCode,omitempty"`
	ErrorMessage string                  `json:"errorMessage,omitempty"`
	CreatedAt    int64                   `json:"createdAt"`
	UpdatedAt    int64                   `json:"updatedAt"`
}

func toLaunchView(r domain.AgentLaunchRecord) launchView {
	return launchView{
		ID:           r.ID,
		Stage:        r.Stage,
		Mode:         r.Mode,
		Title:        r.Title,
		AgentID:      r.AgentID,
		WorktreeID:   derefStr(r.WorktreeID),
		TerminalID:   derefStr(r.TerminalID),
		WatcherID:    derefStr(r.WatcherID),
		ErrorCode:    derefStr(r.ErrorCode),
		ErrorMessage: derefStr(r.ErrorMessage),
		CreatedAt:    r.CreatedAt,
		UpdatedAt:    r.UpdatedAt,
	}
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- agentTask.status ---

// codeInvalidArgs is the family-local arg-rejection code (package tools keeps its
// own unexported copy, so each family redeclares the shared "INVALID_ARGS" string).
const codeInvalidArgs = "INVALID_ARGS"

type statusArgs struct {
	LaunchID string `json:"launchId"`
}

// Validate rejects a blank launchId at the Decode gate (strict decoding alone
// would accept "" / whitespace), so the dispatch path returns INVALID_ARGS before
// the handler runs.
func (a *statusArgs) Validate() error {
	if strings.TrimSpace(a.LaunchID) == "" {
		return fmt.Errorf("launchId is required")
	}
	return nil
}

var statusSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["launchId"],
  "properties": {
    "launchId": { "type": "string", "description": "The launchId (agt_…) returned by agentTask.spawnForEdits." }
  }
}`)

func newStatusTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "agentTask.status",
		Description: "Read back the durable spawn saga for one launch by its launchId (the agt_… id returned by " +
			"agentTask.spawnForEdits). Returns the current stage (launch_requested → agent_started → terminal_bound → " +
			"watcher_attached → confirmed, or failed/ambiguous), the bound terminalId/watcherId, and any error detail — " +
			"so you can confirm a spawn actually landed instead of trusting the launch call's return alone. Read-only.",
		Risk:   domain.RiskRead,
		Schema: statusSchema,
		Decode: tools.StrictDecoder(func() any { return &statusArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a statusArgs
			_ = json.Unmarshal(raw, &a)
			id := strings.TrimSpace(a.LaunchID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "agentTask.status: launchId is required")
			}
			rec, err := deps.DB.GetAgentLaunch(id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "agentTask.status: "+err.Error())
			}
			if rec == nil {
				return tools.Fail(codeLaunchNotFound, "no spawn launch found for id "+id, tools.Unrecoverable())
			}
			view := toLaunchView(*rec)
			return tools.Ok(
				fmt.Sprintf("Launch %s for %q is at stage %s.", view.ID, view.Title, view.Stage),
				view)
		},
	}
}

// --- agentTask.list ---

func newListTool(deps Deps) tools.Tool {
	schema, _ := json.Marshal(tools.NoArgs)
	return tools.Tool{
		Name: "agentTask.list",
		Description: "List the most recent spawn sagas (newest first, up to 20) with their stage, bound terminal/watcher, " +
			"and any error. Use it to see what agentTask.spawnForEdits launches happened and where they stand. Read-only. " +
			"Rows can span sessions, but any launch still in a non-terminal stage belongs to THIS session — a prior session's " +
			"in-flight sagas are swept to 'failed' when the DB opens, so a live-looking stage is always current.",
		Risk:   domain.RiskRead,
		Schema: schema,
		Handle: func(_ context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			rows, err := deps.DB.ListAgentLaunches(listDefaultLimit)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "agentTask.list: "+err.Error())
			}
			// Normalize nil → [] so the result is always an array, never null.
			views := make([]launchView, 0, len(rows))
			for _, r := range rows {
				views = append(views, toLaunchView(r))
			}
			plural := "es"
			if len(views) == 1 {
				plural = ""
			}
			return tools.Ok(
				fmt.Sprintf("%d spawn launch%s.", len(views), plural),
				map[string]any{"launches": views})
		},
	}
}
