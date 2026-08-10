package agenttaskx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
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

type listArgs struct {
	Scope string `json:"scope,omitempty"`
}

// Validate pins scope to the two supported values ("" defaults to "session") so a
// typo'd scope fails as INVALID_ARGS instead of silently listing the wrong window.
func (a *listArgs) Validate() error {
	switch a.Scope {
	case "", "session", "all":
		return nil
	}
	return fmt.Errorf(`scope must be "session" or "all"`)
}

var listSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "scope": { "type": "string", "enum": ["session", "all"], "default": "session", "description": "\"session\" (default) lists only launches made by THIS session — the right window for checking whether a batch of spawns landed. \"all\" includes earlier sessions' launches (newest first, up to 20) for history questions." }
  },
  "required": []
}`)

func newListTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "agentTask.list",
		Description: "List recent spawn sagas (newest first) with their stage, bound terminal/watcher, and any error. Use it to see " +
			"what agentTask.spawnForEdits launches happened and where they stand. By default it lists ONLY this session's launches — " +
			"the usual 'did my spawns land?' check — so stale terminal ids from earlier sessions (whose titles can collide with " +
			"today's) never mix into the answer; pass scope:\"all\" when the user asks about older history. Read-only. Any launch " +
			"still in a non-terminal stage belongs to THIS session — a prior session's in-flight sagas are swept to 'failed' when " +
			"the DB opens, so a live-looking stage is always current.",
		Risk:   domain.RiskRead,
		Schema: listSchema,
		Decode: tools.StrictDecoder(func() any { return &listArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a listArgs
			_ = json.Unmarshal(raw, &a)
			scope := a.Scope
			if scope == "" {
				scope = "session"
			}
			rows, err := deps.DB.ListAgentLaunches(listDefaultLimit)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "agentTask.list: "+err.Error())
			}
			// The launch record carries no session id (deliberately — no schema churn),
			// but only the process holding the DB lease can insert launches, so
			// "created at-or-after this process booted" IS "this session's launches".
			// SessionStartedAt==0 (unwired) disables the filter rather than hiding rows.
			older := 0
			if scope == "session" && deps.SessionStartedAt > 0 {
				kept := rows[:0]
				for _, r := range rows {
					if r.CreatedAt >= deps.SessionStartedAt {
						kept = append(kept, r)
					} else {
						older++
					}
				}
				rows = kept
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
			summary := fmt.Sprintf("%d spawn launch%s.", len(views), plural)
			if scope == "session" && deps.SessionStartedAt > 0 {
				summary = fmt.Sprintf("%d spawn launch%s this session.", len(views), plural)
				if older > 0 {
					// Name the hidden remainder explicitly (a model told "0 launches" with
					// older rows silently dropped would honestly answer a history question
					// with "none") — but no count: the filter only saw the newest-20
					// window, so an exact number would understate.
					summary += ` Older launches exist — pass scope:"all" to include them.`
				}
			}
			return tools.Ok(summary, map[string]any{"launches": views, "scope": scope})
		},
	}
}
