package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// supervisorDefaultCadenceMs is the cadence of a CLI-spawned supervisor watcher.
// Defined locally so this package needs no shared-cadence import.
const supervisorDefaultCadenceMs = 3000

// foregroundOnlyNote is appended to a summary whenever a session-scoped watcher
// is attached: watchers tick only while the assistant is open and never resume.
const foregroundOnlyNote = " NOTE: supervision is foreground-only — the watcher ticks only while the assistant is open and does not resume on the next launch."

// workflowMcpArgs is the shared shape for the workflow MCP passthroughs: an
// opaque arguments record (workflow setup fields are Daintree-defined), an
// optional requestKey, and an assistant-side attachWatcher flag.
type workflowMcpArgs struct {
	Arguments map[string]any `json:"arguments"`
	// RequestKey is forwarded to Daintree for idempotency.
	RequestKey string `json:"requestKey,omitempty"`
	// AttachWatcher is ASSISTANT-SIDE ONLY (never forwarded). Pointer so we can
	// distinguish "omitted" (default true) from an explicit false.
	AttachWatcher *bool `json:"attachWatcher,omitempty"`
}

var startWorkOnIssueSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["arguments"],
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true },
    "requestKey": { "type": "string" },
    "attachWatcher": { "type": "boolean", "description": "Attach a supervisor watcher to the spawned terminal (default true). Assistant-side only." }
  }
}`)

var prepBranchForReviewSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["arguments"],
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true },
    "requestKey": { "type": "string" }
  }
}`)

// newWorkflowStartWorkOnIssueTool builds workflow.startWorkOnIssue (external):
// passthrough to Daintree, then attach a supervisor watcher to the spawned
// terminal UNLESS attachWatcher===false or the passthrough failed. attachWatcher
// is NEVER forwarded to Daintree.
func newWorkflowStartWorkOnIssueTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.startWorkOnIssue",
		Description: "Start work on an issue in Daintree (spawns a worktree + agent terminal), then attach a supervisor watcher.",
		Risk:        domain.RiskExternal,
		Consequence: "Spawns a worktree and a visible agent terminal to start work on the issue.",
		Schema:      startWorkOnIssueSchema,
		Decode:      tools.StrictDecoder(func() any { return &workflowMcpArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a workflowMcpArgs
			if res, ok := strictDecode(args, "workflow.startWorkOnIssue", &a); !ok {
				return res
			}
			// Forward ONLY arguments (attachWatcher is assistant-side, never sent).
			res := passthrough(ctx, tctx, "workflow.startWorkOnIssue", a.Arguments, a.RequestKey)
			attach := a.AttachWatcher == nil || *a.AttachWatcher
			if !attach || !res.Ok {
				return res
			}
			return attachSupervisorWatcher(ctx, deps, tctx, res)
		},
	}
}

// newWorkflowPrepBranchForReviewTool builds workflow.prepBranchForReview
// (external): passthrough only.
func newWorkflowPrepBranchForReviewTool() *tools.Tool {
	return &tools.Tool{
		Name:        "workflow.prepBranchForReview",
		Description: "Prepare a branch for review in Daintree (push, open PR, etc.).",
		Risk:        domain.RiskExternal,
		Consequence: "Pushes the branch and prepares it for review (may open a PR).",
		Schema:      prepBranchForReviewSchema,
		Decode:      tools.StrictDecoder(func() any { return &workflowMcpArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a workflowMcpArgs
			if res, ok := strictDecode(args, "workflow.prepBranchForReview", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "workflow.prepBranchForReview", a.Arguments, a.RequestKey)
		},
	}
}

// supervisorContext is the slice of the passthrough's structuredContent we read
// to attach a watcher and seed the workflow ledger row. issueUrl/branch are
// best-effort enrichments (absent on older Daintree builds → left empty).
type supervisorContext struct {
	TerminalID  string `json:"terminalId"`
	WorktreeID  string `json:"worktreeId"`
	IssueTitle  string `json:"issueTitle"`
	IssueNumber *int   `json:"issueNumber"`
	IssueURL    string `json:"issueUrl"`
	Branch      string `json:"branch"`
}

// attachSupervisorWatcher reads the spawned terminal from the passthrough result,
// records the work in the durable workflow ledger, and (unless one already
// supervises it) inserts a supervisor watcher back-linked to that ledger row. A
// missing terminalId, a ledger failure, or a watcher-insert failure NEVER fails the
// call — setup still succeeded; we only annotate the summary.
func attachSupervisorWatcher(ctx context.Context, deps Deps, tctx *tools.ToolContext, res tools.ToolResult) tools.ToolResult {
	sc := extractSupervisorContext(res.Result)
	if sc == nil || sc.TerminalID == "" {
		// No terminal to supervise — return the setup result untouched.
		return res
	}
	if deps.Store == nil {
		return res
	}

	// Dedup: an active supervisor already targeting the terminal means we must not
	// attach a second one. If it already carries a workflow link, the work is already
	// recorded — nothing to do. If it does NOT (e.g. an adopted superviseTerminal or a
	// manually-created watcher), fall through to record the work in the ledger but
	// skip the duplicate watcher.
	skipWatcher := false
	if existing, err := deps.Store.ListWatchers(ctx, "active"); err == nil {
		for i := range existing {
			w := &existing[i]
			if w.IsSupervisor != nil && *w.IsSupervisor && watcherTargets(w, sc.TerminalID) {
				if w.WorkflowRunID != nil && *w.WorkflowRunID != "" {
					return res // already supervised AND recorded
				}
				skipWatcher = true
				break
			}
		}
	}

	// Record the spawned work in the durable ledger BEFORE the watcher, so the
	// watcher can back-link to it (the daemon then advances the row's status as the
	// watcher progresses). Best-effort: a ledger failure never fails setup.
	workflowRunID := ""
	if deps.WorkflowStore != nil {
		if runID, err := deps.WorkflowStore.InsertWorkflowRun(ctx, domain.WorkflowRunRecord{
			Status:          domain.WorkflowActive,
			IssueNumber:     sc.IssueNumber,
			IssueURL:        strOrNil(sc.IssueURL),
			IssueTitle:      strOrNil(sc.IssueTitle),
			Branch:          strOrNil(sc.Branch),
			WorktreeID:      strOrNil(sc.WorktreeID),
			TerminalIdsJson: strOrNil(jsonIDArray(sc.TerminalID)),
		}); err == nil {
			workflowRunID = runID
		}
	}

	// A link-less supervisor already watches the terminal: the work is now recorded,
	// but attaching a second watcher would violate the one-supervisor-per-terminal
	// invariant, so we stop here (the row stays active for a human/model to advance).
	if skipWatcher {
		return res
	}

	title := "watch issue"
	if sc.IssueTitle != "" {
		title = "watch " + sc.IssueTitle
	} else if sc.IssueNumber != nil {
		title = fmt.Sprintf("watch issue #%d", *sc.IssueNumber)
	}

	// Assemble the supervisor record via the shared builder so this attach can never
	// drift from the spawn-for-edits / superviseTerminal attach paths.
	rec := domain.BuildSupervisorWatcherRecord(domain.SupervisorWatcherSpec{
		TerminalID: sc.TerminalID,
		WorktreeID: sc.WorktreeID,
		Title:      title,
		Goal:       "Supervise the spawned agent until the work is complete and verified.",
		CadenceMs:  supervisorDefaultCadenceMs,
		SpawnMode:  "edit",
	})
	if workflowRunID != "" {
		rec.WorkflowRunID = &workflowRunID
	}
	watcher, err := deps.Store.InsertWatcher(ctx, rec)
	if err != nil {
		// The agent IS running, but supervision could not be wired. Path A has no
		// idempotency anchor for the ledger row, so an active row left here would let
		// a retry insert a duplicate active row. Close it (best-effort) so a retry
		// records a fresh one instead.
		if workflowRunID != "" && deps.WorkflowStore != nil {
			_ = deps.WorkflowStore.UpdateWorkflowRun(ctx, workflowRunID, map[string]any{
				"status":      string(domain.WorkflowFailed),
				"completedAt": domain.NowMS(),
			})
		}
		return res
	}
	// Record the supervising watcher on the ledger row (best-effort).
	if workflowRunID != "" && deps.WorkflowStore != nil {
		_ = deps.WorkflowStore.UpdateWorkflowRun(ctx, workflowRunID, map[string]any{"watcherIdsJson": jsonIDArray(watcher.ID)})
	}
	res.Summary += foregroundOnlyNote
	return res
}

// strOrNil maps an empty string to a nil *string (so the ledger column stays NULL)
// and a non-empty one to its address.
func strOrNil(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// jsonIDArray marshals a single id into a JSON string-array ("[\"x\"]") for the
// ledger's *IdsJson columns, or "" when the id is empty (strOrNil maps "" → NULL).
func jsonIDArray(id string) string {
	if id == "" {
		return ""
	}
	b, err := json.Marshal([]string{id})
	if err != nil {
		return ""
	}
	return string(b)
}

// extractSupervisorContext pulls the supervisor fields from the passthrough
// result's structuredContent (the ok() payload is {text, structuredContent}).
func extractSupervisorContext(result any) *supervisorContext {
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	sc, ok := m["structuredContent"]
	if !ok || sc == nil {
		return nil
	}
	// Round-trip through JSON to decode whatever generic shape the MCP returned.
	data, err := json.Marshal(sc)
	if err != nil {
		return nil
	}
	var out supervisorContext
	if err := json.Unmarshal(data, &out); err != nil {
		return nil
	}
	return &out
}

// watcherTargets reports whether a watcher's targetsJson includes terminalID.
func watcherTargets(w *domain.WatcherRecord, terminalID string) bool {
	var targets []string
	if err := json.Unmarshal([]byte(w.TargetsJson), &targets); err != nil {
		return false
	}
	for _, t := range targets {
		if t == terminalID {
			return true
		}
	}
	return false
}
