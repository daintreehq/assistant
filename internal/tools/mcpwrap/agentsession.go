package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ------------------------ agentSessionHistory.list ------------------------ */

// Daintree's own paging bounds for the session journal (agentActions.ts).
const (
	sessionListDefaultLimit = 20
	sessionListMaxLimit     = 100
)

// agentSessionHistoryListArgs mirrors the host's SessionHistoryListArgsSchema.
//
// Both ids are `.min(1)` on the host: an EMPTY string is a validation error there, not a
// silent "unscoped", because a blank id would fall through the bridge's `if (!worktreeId)`
// guard and quietly widen the listing. Rejecting it here keeps that promise at the first
// boundary the model touches, with a message that says what to do instead.
type agentSessionHistoryListArgs struct {
	// Pointers, not string+omitempty: StrictDecoder re-marshals, so a plain string could
	// not tell an explicit "" from an absent key, and dropping an explicit "" is exactly
	// the silent widening the host's `.min(1)` exists to prevent.
	WorktreeID *string `json:"worktreeId,omitempty"`
	ProjectID  *string `json:"projectId,omitempty"`
	Limit      *int    `json:"limit,omitempty"`
	Offset     *int    `json:"offset,omitempty"`
}

func (a agentSessionHistoryListArgs) Validate() error {
	// A supplied-but-blank id must be rejected, never forwarded and never dropped: the
	// host declares both `.min(1)` because a blank one falls through its
	// `if (!worktreeId)` guard and quietly widens the listing to every project.
	if a.WorktreeID != nil && strings.TrimSpace(*a.WorktreeID) == "" {
		return fmt.Errorf("worktreeId is blank; omit it to fall back to this session's scope rather than passing an empty id")
	}
	if a.ProjectID != nil && strings.TrimSpace(*a.ProjectID) == "" {
		return fmt.Errorf("projectId is blank; omit it to fall back to this session's scope rather than passing an empty id")
	}
	if a.Limit != nil && (*a.Limit < 1 || *a.Limit > sessionListMaxLimit) {
		return fmt.Errorf("limit is %d; Daintree accepts 1–%d (omit it for the default of %d)", *a.Limit, sessionListMaxLimit, sessionListDefaultLimit)
	}
	if a.Offset != nil && *a.Offset < 0 {
		return fmt.Errorf("offset is %d; it must be zero or greater", *a.Offset)
	}
	return nil
}

var agentSessionHistoryListSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "minLength": 1, "description": "Scope to one worktree id. Omit it; never pass \"\"." },
    "projectId": { "type": "string", "minLength": 1, "description": "Scope to one project id; combines with worktreeId." },
    "limit": { "type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Max records, newest first." },
    "offset": { "type": "integer", "minimum": 0, "default": 0, "description": "Records to skip, for paging past the limit." }
  }
}`)

// newAgentSessionHistoryListTool lists closed agent sessions that can be relaunched.
//
// Two host behaviours are surfaced in the description because neither is visible in the
// payload. (1) It FAILS rather than returning an empty list when no scope resolves —
// an empty list and an unscoped call are different answers, and the host refuses to blur
// them. (2) Old records are pruned by retention, so an absent session is not proof one
// never existed; a model that reads absence as history is wrong in the one direction
// that matters when deciding whether work was already done.
func newAgentSessionHistoryListTool() *tools.Tool {
	return &tools.Tool{
		Name: "agentSessionHistory.list",
		Description: "List CLOSED agent sessions that can be relaunched, from the on-disk journal — which sessions exist, " +
			"carrying no transcript text. Newest first; read `total`/`hasMore` before concluding you have seen them all. " +
			"It must resolve a worktree or project scope and FAILS rather than listing everything, so pass one when this " +
			"session has none. Retention prunes old records, so absence does not prove a session never existed.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         agentSessionHistoryListSchema,
		Decode:         tools.StrictDecoder(func() any { return &agentSessionHistoryListArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a agentSessionHistoryListArgs
			if res, ok := strictDecode(args, "agentSessionHistory.list", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.WorktreeID != nil {
				fwd["worktreeId"] = *a.WorktreeID
			}
			if a.ProjectID != nil {
				fwd["projectId"] = *a.ProjectID
			}
			if a.Limit != nil {
				fwd["limit"] = *a.Limit
			}
			if a.Offset != nil {
				fwd["offset"] = *a.Offset
			}
			res := passthrough(ctx, tctx, "agentSessionHistory.list", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("agentSessionHistory.list")
			}
			// Presence-checked: a missing `sessions` would be reported as "Found 0
			// resumable session(s)", which the description explicitly warns must not be
			// read as "none exist". A malformed payload must not be the thing that says it.
			sessions, ok2 := obj["sessions"].([]any)
			if !ok2 {
				return failMalformed("agentSessionHistory.list")
			}
			out := map[string]any{"sessions": sessions}
			for _, k := range []string{"total", "hasMore"} {
				if v, present := obj[k]; present {
					out[k] = v
				}
			}
			summary := fmt.Sprintf("Found %d resumable session(s)", len(sessions))
			if total, ok := numberFrom(obj["total"]); ok && total > len(sessions) {
				summary += fmt.Sprintf(" of %d in scope", total)
			}
			if more, ok := obj["hasMore"].(bool); ok && more {
				summary += " — MORE remain; raise offset to page further"
			}
			return tools.Ok(summary+".", out)
		},
	}
}
