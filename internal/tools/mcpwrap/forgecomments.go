package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ------------------------ forge.listIssueComments ------------------------- */

// forgeListIssueCommentsArgs mirrors the host's argsSchema exactly: the shared
// worktreeLocationShape (with its legacy `cwd` alias), a required positive issueNumber,
// and the two paging knobs.
//
// Cursor and PerPage are pointers so "omitted" survives the round trip. An empty cursor
// is a value the host accepts and it is NOT the same as no cursor, and a zero PerPage
// must fail the declared minimum rather than quietly become the default.
type forgeListIssueCommentsArgs struct {
	CWD          string  `json:"cwd,omitempty"`
	WorktreeID   string  `json:"worktreeId,omitempty"`
	WorktreePath string  `json:"worktreePath,omitempty"`
	IssueNumber  int     `json:"issueNumber"`
	Cursor       *string `json:"cursor,omitempty"`
	PerPage      *int    `json:"perPage,omitempty"`
}

func (a forgeListIssueCommentsArgs) Validate() error {
	if a.IssueNumber <= 0 {
		return fmt.Errorf("issueNumber must be a positive integer")
	}
	if a.PerPage != nil && (*a.PerPage < 1 || *a.PerPage > 100) {
		return fmt.Errorf("perPage is %d; the forge accepts 1–100 (omit it for the default of 20)", *a.PerPage)
	}
	return nil
}

var forgeListIssueCommentsSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["issueNumber"],
  "properties": {
    "cwd": { "type": "string", "description": "Legacy alias for worktreePath." },
    "worktreeId": { "type": "string", "description": "Worktree id. Takes precedence over a path." },
    "worktreePath": { "type": "string", "description": "Absolute worktree path." },
    "issueNumber": { "type": "integer", "minimum": 1, "description": "The issue whose comments to read (positive integer)." },
    "cursor": { "type": "string", "description": "Opaque cursor — pass the previous response's nextCursor for the next page." },
    "perPage": { "type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Comments per page." }
  }
}`)

// forwardLocation copies whichever worktree locator was supplied into an outgoing call.
// All three spellings are accepted because the host accepts all three, and this action
// is on the daintree.call denylist — a spelling the wrapper cannot express is a spelling
// with no path at all. Precedence (id beats path) is the forge action's rule and stays
// there; re-implementing it here would be a second authority on one question.
func (a forgeListIssueCommentsArgs) forwardLocation(fwd map[string]any) {
	if a.CWD != "" {
		fwd["cwd"] = a.CWD
	}
	if a.WorktreeID != "" {
		fwd["worktreeId"] = a.WorktreeID
	}
	if a.WorktreePath != "" {
		fwd["worktreePath"] = a.WorktreePath
	}
}

// newForgeListIssueCommentsTool reads one page of an issue's comment thread.
//
// The paging direction is the trap worth naming in the description: comments arrive
// OLDEST FIRST, so the newest reply — usually the one that matters — is at the END of
// the thread, behind however many pages precede it. A model that reads page one and
// stops has read the least recent discussion, and nothing in the payload says so.
func newForgeListIssueCommentsTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.listIssueComments",
		Description: "Read ONE page of a GitHub issue's comment thread. Comments come back OLDEST FIRST, so the newest reply is " +
			"at the END: page with nextCursor until hasMore is false before concluding what the thread says. An empty page " +
			"genuinely means nobody commented — a missing issue fails instead, so silence is never ambiguous. " +
			"PARALLEL: forge.* reads batched in ONE reply run concurrently.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         forgeListIssueCommentsSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeListIssueCommentsArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeListIssueCommentsArgs
			if res, ok := strictDecode(args, "forge.listIssueComments", &a); !ok {
				return res
			}
			fwd := map[string]any{"issueNumber": a.IssueNumber}
			a.forwardLocation(fwd)
			if a.Cursor != nil {
				fwd["cursor"] = *a.Cursor
			}
			if a.PerPage != nil {
				fwd["perPage"] = *a.PerPage
			}
			res := passthrough(ctx, tctx, "forge.listIssueComments", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("forge.listIssueComments")
			}
			items, _ := obj["items"].([]any)
			out := map[string]any{"issueNumber": a.IssueNumber, "items": items}
			// hasMore/nextCursor/totalCount are copied only when the forge actually sent
			// them. A defaulted hasMore:false would read as "you have the whole thread",
			// which is the one wrong answer this tool must never give.
			for _, k := range []string{"nextCursor", "hasMore", "totalCount"} {
				if v, present := obj[k]; present {
					out[k] = v
				}
			}
			summary := fmt.Sprintf("Read %d comment(s) on issue #%d", len(items), a.IssueNumber)
			if more, ok := obj["hasMore"].(bool); ok && more {
				summary += " — MORE remain; page with nextCursor to reach the newest"
			}
			return tools.Ok(summary+".", out)
		},
	}
}
