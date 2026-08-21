package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ------------------------ worktree.resource.status ------------------------ */

// WorktreeID is a pointer with NO minLength, mirroring the host's plain optional string.
//
// This is the sharpest case for presence-awareness in the whole family. The host resolves
// `args?.worktreeId ?? ctx.focusedWorktreeId ?? ctx.activeWorktreeId`, and `??` keeps an
// explicit "" — so on the host an empty id fails with "Worktree not found". If this
// wrapper dropped an empty id instead, the call would fall through to the FOCUSED
// worktree and EXECUTE ITS CONFIGURED STATUS COMMAND: a shell command run against a
// resource the caller never named. Daintree's own locationArgs.ts warns about exactly
// this ("an empty selector ... would retarget a destructive call at whatever happens to
// be active"). So an explicit "" is forwarded verbatim and fails honestly on the host.
type worktreeResourceStatusArgs struct {
	WorktreeID *string `json:"worktreeId,omitempty"`
}

var worktreeResourceStatusSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "description": "The worktree whose remote resource to check. Omit for the focused or active worktree; the call fails when neither resolves." }
  }
}`)

// newWorktreeResourceStatusTool reports a worktree's remote-resource status.
//
// RISK: project, NOT read — and this is a deliberate departure from the read-shaped name.
// Daintree implements it as `worktreeClient.resourceAction(id, "status")`, which EXECUTES
// THE WORKTREE'S CONFIGURED STATUS COMMAND: arbitrary user-supplied shell, against a
// remote resource, whose cost is whatever that command costs (the host's own description
// says exactly that). It is declared `kind: "command"` there, not `kind: "query"`.
// docs/TOOLS.md sets the rule this follows: Risk names the riskiest thing the handler
// does, and running an arbitrary configured command is not a read no matter how
// read-shaped the answer is. project.runCheck is classified the same way for the same
// reason.
//
// Issue #367 grouped this with the read-only wrappers; the host source says otherwise,
// and the issue also asks for accurate policy over blanket relabelling, so accuracy wins
// and the evidence is recorded here.
//
// Consequently NOT parallelizable: the double-gate on RiskRead would refuse the flag,
// and fanning out shell executions across worktrees is not something a batch should do
// implicitly.
//
// It also keeps the transport's ordinary 120s deadline. Unlike project.runCheck this
// action exposes NO timeout argument, so there is no server-side budget to size a longer
// wire deadline against — inventing one would be a guess at someone's status command.
func newWorktreeResourceStatusTool() *tools.Tool {
	return &tools.Tool{
		Name: "worktree.resource.status",
		Description: "Run a worktree's configured remote-resource status command (e.g. a cloud devbox) and report what it said. " +
			"It EXECUTES a real command and waits, so it costs whatever that command costs. `configured:false` means no " +
			"status command is set up — not a failure, and not evidence the resource is down. A failing status command fails " +
			"this call rather than returning a stale cached answer, so a result is always fresh.",
		Consequence: "Runs the worktree's configured remote-resource status command and waits for it to finish.",
		Risk:        domain.RiskProject,
		Schema:      worktreeResourceStatusSchema,
		Decode:      tools.StrictDecoder(func() any { return &worktreeResourceStatusArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a worktreeResourceStatusArgs
			if res, ok := strictDecode(args, "worktree.resource.status", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.WorktreeID != nil {
				fwd["worktreeId"] = *a.WorktreeID
			}
			res := passthrough(ctx, tctx, "worktree.resource.status", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("worktree.resource.status")
			}
			// `configured` is the field that decides how to read everything else, so an
			// absent one leaves no answer to report rather than a defaulted false — which
			// would claim "no status command is set up" on no evidence at all.
			configured, hasConfigured := obj["configured"].(bool)
			if !hasConfigured {
				return failMalformed("worktree.resource.status")
			}
			out := map[string]any{"configured": configured, "status": obj["status"]}
			if !configured {
				return tools.Ok("No resource status command is configured for this worktree, so there is nothing to report — this is not evidence the resource is down.", out)
			}
			if obj["status"] == nil {
				return tools.Ok("The resource status command ran but reported no status.", out)
			}
			return tools.Ok(fmt.Sprintf("Resource status: %v.", obj["status"]), out)
		},
	}
}
