package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Forge READS (risk read). All four are TYPED: the host's forge list/get actions
// validate their arguments with strict (closed) schemas, so an invented key is a
// hard refusal rather than a silently-dropped filter. Mirroring the real field
// set locally means the model sees the exact contract in its tool schema and the
// strict decoder catches a bad shape here — before a round-trip. Nothing forge
// is forwarded under an opaque `arguments` record any more; the flat fields ARE
// the contract. Result projection is the HOST's job (view: summary|full), so
// passthrough keeps handing back text/structuredContent untouched.

// forgeArgumentsArgs forwards an opaque arguments record (strict at the top).
type forgeArgumentsArgs struct {
	Arguments map[string]any `json:"arguments,omitempty"`
}

var forgeArgumentsSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "arguments": { "type": "object", "additionalProperties": true }
  }
}`)

// forgeRead builds a read-only opaque-args wrapper that forwards args.arguments ?? {}.
// Despite the name it is NOT forge-specific and no longer builds any forge tool:
// its remaining callers are worktree.list / worktree.getCurrent (worktree.go) and
// git.getProjectPulse (git.go), whose Daintree-side schemas are open and take no
// required argument, so there is nothing to type.
func forgeRead(name, desc string) *tools.Tool {
	return &tools.Tool{
		Name:        name,
		Description: desc,
		Risk:        domain.RiskRead,
		// Every forgeRead tool is an independent, bounded MCP snapshot read (list/get over
		// the forge / worktree / git surface) with no ordering dependency on its batch
		// siblings, so a batch of them overlaps its round-trips up to the MCP governor's
		// in-flight cap instead of serializing. Safe like terminal.extract; double-gated on
		// RiskRead by the runner.
		Parallelizable: true,
		Schema:         forgeArgumentsSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeArgumentsArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeArgumentsArgs
			if res, ok := strictDecode(args, name, &a); !ok {
				return res
			}
			fwd := a.Arguments
			if fwd == nil {
				fwd = map[string]any{}
			}
			return passthrough(ctx, tctx, name, fwd, "")
		},
	}
}

// forgeWorktreeLocationArgs is the host's worktree-location shape, shared by every
// forge read. All three are optional and omitting them targets the ACTIVE worktree,
// which is the overwhelmingly common case. cwd is the host's legacy alias for a
// worktree path; it is still accepted (internal/daemon/prwatcher.go calls
// forge.getPR with it directly), so it stays exposed rather than being dropped.
type forgeWorktreeLocationArgs struct {
	WorktreeID   *string `json:"worktreeId,omitempty"`
	WorktreePath *string `json:"worktreePath,omitempty"`
	CWD          *string `json:"cwd,omitempty"`
}

// forgeListPagingArgs is the paging/projection block both list tools share.
// Every field is a POINTER so "unset" is distinguishable from a zero value: the
// host's list schemas are strict AND bounded, so forwarding perPage:0 or state:""
// for an omitted field would be refused outright. nil simply omits the key and
// lets the host apply its own documented default.
type forgeListPagingArgs struct {
	forgeWorktreeLocationArgs
	Cursor      *string `json:"cursor,omitempty"`
	PerPage     *int    `json:"perPage,omitempty"`
	Sort        *string `json:"sort,omitempty"`
	Direction   *string `json:"direction,omitempty"`
	BypassCache *bool   `json:"bypassCache,omitempty"`
	View        *string `json:"view,omitempty"`
}

type forgeListIssuesArgs struct {
	forgeListPagingArgs
	State  *string `json:"state,omitempty"`
	Search *string `json:"search,omitempty"`
}

// forgeListPRsArgs deliberately has NO Search field and a WIDER state enum
// (it adds "merged"): the host's PR list schema differs from the issue one on
// exactly those two points, and both schemas are strict, so copying either into
// the other would produce a refusal the model can't diagnose.
type forgeListPRsArgs struct {
	forgeListPagingArgs
	State *string `json:"state,omitempty"`
}

// forgeGetIssueArgs / forgeGetPRArgs take a required positive number (never a
// string) plus an optional worktree location.
type forgeGetIssueArgs struct {
	forgeWorktreeLocationArgs
	IssueNumber int `json:"issueNumber"`
}

type forgeGetPRArgs struct {
	forgeWorktreeLocationArgs
	PRNumber int `json:"prNumber"`
}

// Validate mirrors the host's Zod refinements (enums + numeric bounds) that
// strict JSON decoding alone can't express. StrictDecoder runs it at the registry
// Decode gate; every handler ALSO calls it after its own strictDecode, because
// that helper is structural only — so a direct Handle call is guarded too.
func (a *forgeListPagingArgs) Validate() error {
	if a.Cursor != nil && *a.Cursor == "" {
		return fmt.Errorf("cursor must be a non-empty opaque cursor from a previous response's nextCursor")
	}
	if a.PerPage != nil && (*a.PerPage < 1 || *a.PerPage > 100) {
		return fmt.Errorf("perPage must be between 1 and 100")
	}
	if err := oneOf("sort", a.Sort, "created", "updated"); err != nil {
		return err
	}
	if err := oneOf("direction", a.Direction, "asc", "desc"); err != nil {
		return err
	}
	return oneOf("view", a.View, "summary", "full")
}

func (a *forgeListIssuesArgs) Validate() error {
	if err := a.forgeListPagingArgs.Validate(); err != nil {
		return err
	}
	return oneOf("state", a.State, "open", "closed", "all")
}

func (a *forgeListPRsArgs) Validate() error {
	if err := a.forgeListPagingArgs.Validate(); err != nil {
		return err
	}
	return oneOf("state", a.State, "open", "closed", "merged", "all")
}

func (a *forgeGetIssueArgs) Validate() error {
	if a.IssueNumber <= 0 {
		return fmt.Errorf("issueNumber must be a positive integer")
	}
	return nil
}

func (a *forgeGetPRArgs) Validate() error {
	if a.PRNumber <= 0 {
		return fmt.Errorf("prNumber must be a positive integer")
	}
	return nil
}

// oneOf enforces a string enum on an optional field, listing the legal values in
// the error so the model can correct itself in one retry instead of guessing.
func oneOf(field string, got *string, allowed ...string) error {
	if got == nil {
		return nil
	}
	for _, want := range allowed {
		if *got == want {
			return nil
		}
	}
	return fmt.Errorf("%s must be one of %v, got %q", field, allowed, *got)
}

// addTo copies only the SET location fields into the outgoing call.
func (a forgeWorktreeLocationArgs) addTo(fwd map[string]any) {
	if a.WorktreeID != nil {
		fwd["worktreeId"] = *a.WorktreeID
	}
	if a.WorktreePath != nil {
		fwd["worktreePath"] = *a.WorktreePath
	}
	if a.CWD != nil {
		fwd["cwd"] = *a.CWD
	}
}

// forwardArgs flattens the typed args into the MCP call payload, omitting every
// unset optional so the host's strict schema only ever sees keys the model
// actually chose.
func (a forgeListPagingArgs) forwardArgs() map[string]any {
	fwd := map[string]any{}
	a.forgeWorktreeLocationArgs.addTo(fwd)
	if a.Cursor != nil {
		fwd["cursor"] = *a.Cursor
	}
	if a.PerPage != nil {
		fwd["perPage"] = *a.PerPage
	}
	if a.Sort != nil {
		fwd["sort"] = *a.Sort
	}
	if a.Direction != nil {
		fwd["direction"] = *a.Direction
	}
	if a.BypassCache != nil {
		fwd["bypassCache"] = *a.BypassCache
	}
	if a.View != nil {
		fwd["view"] = *a.View
	}
	return fwd
}

func (a forgeListIssuesArgs) forwardArgs() map[string]any {
	fwd := a.forgeListPagingArgs.forwardArgs()
	if a.State != nil {
		fwd["state"] = *a.State
	}
	if a.Search != nil {
		fwd["search"] = *a.Search
	}
	return fwd
}

func (a forgeListPRsArgs) forwardArgs() map[string]any {
	fwd := a.forgeListPagingArgs.forwardArgs()
	if a.State != nil {
		fwd["state"] = *a.State
	}
	return fwd
}

func (a forgeGetIssueArgs) forwardArgs() map[string]any {
	fwd := map[string]any{"issueNumber": a.IssueNumber}
	a.forgeWorktreeLocationArgs.addTo(fwd)
	return fwd
}

func (a forgeGetPRArgs) forwardArgs() map[string]any {
	fwd := map[string]any{"prNumber": a.PRNumber}
	a.forgeWorktreeLocationArgs.addTo(fwd)
	return fwd
}

// forgeLocationSchemaProps is the shared JSON-Schema fragment for the three
// worktree-location fields. worktreeId's description repeats the PATH-like-id
// warning from worktree.list on purpose: a model that passes a BRANCH name here
// gets a silent null target rather than an error.
const forgeLocationSchemaProps = `
    "worktreeId": { "type": "string", "description": "Target a specific Daintree worktree by its id — a PATH-like id from worktree.list, NEVER a branch name. Omit all three location fields to use the active worktree." },
    "worktreePath": { "type": "string", "description": "Target a worktree by its filesystem path. Omit all three location fields to use the active worktree." },
    "cwd": { "type": "string", "description": "Legacy alias for worktreePath. Prefer worktreeId or worktreePath." }`

// forgeListSchemaProps is the shared paging/projection fragment. Every bound is a
// real JSON-Schema keyword (enum / minimum / maximum / default) because the tool
// schema is forwarded VERBATIM to the model — a bound described only in prose is
// a bound the model will violate.
const forgeListSchemaProps = `
    "cursor": { "type": "string", "minLength": 1, "description": "Opaque cursor from a previous response's nextCursor. Fetch the next page instead of raising perPage." },
    "perPage": { "type": "integer", "minimum": 1, "maximum": 100, "default": 20, "description": "Items per page." },
    "sort": { "type": "string", "enum": ["created", "updated"], "default": "created", "description": "Which timestamp orders the page." },
    "direction": { "type": "string", "enum": ["asc", "desc"], "default": "desc", "description": "Sort direction." },
    "bypassCache": { "type": "boolean", "default": false, "description": "Skip the forge's list cache. Use ONLY to observe a write you just made; otherwise omit — the cached listing is fresh enough and bypassing it costs a provider round-trip." },
    "view": { "type": "string", "enum": ["summary", "full"], "default": "summary", "description": "summary (default) returns compact items — enough to triage, list, or pick one. Ask for full ONLY when you need every field of every row; prefer summary plus a getIssue/getPR on the one row you care about." }`

var forgeListIssuesSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "state": { "type": "string", "enum": ["open", "closed", "all"], "default": "open", "description": "Issue state to include." },
    "search": { "type": "string", "description": "Provider-native (GitHub) search query, forwarded verbatim — this is where label/assignee/author/text filters go, e.g. \"no:assignee -label:human-review\" or \"label:bug in:title parser\". There are no separate label/assignee/limit arguments." },` +
	forgeListSchemaProps + `,` + forgeLocationSchemaProps + `
  }
}`)

var forgeListPRsSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "state": { "type": "string", "enum": ["open", "closed", "merged", "all"], "default": "open", "description": "PR state to include. Unlike issues, \"merged\" is a distinct state here." },` +
	forgeListSchemaProps + `,` + forgeLocationSchemaProps + `
  }
}`)

var forgeGetIssueSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["issueNumber"],
  "properties": {
    "issueNumber": { "type": "integer", "minimum": 1, "description": "The issue number (positive integer), e.g. 299." },` +
	forgeLocationSchemaProps + `
  }
}`)

var forgeGetPRSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["prNumber"],
  "properties": {
    "prNumber": { "type": "integer", "minimum": 1, "description": "The PR number (positive integer)." },` +
	forgeLocationSchemaProps + `
  }
}`)

const forgeParallelNote = "PARALLEL: forge.* reads batched in ONE reply run concurrently. " +
	"MCP_UNAVAILABLE when Daintree is disconnected; a forge refusal returns MCP_TOOL_ERROR."

func newForgeListIssuesTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.listIssues",
		Description: "List the project's GitHub issues through Daintree's forge, with server-side filtering, paging and a compact default projection. " +
			"Filter HERE rather than listing everything and sifting: state narrows open/closed, and search takes a provider-native GitHub query (\"no:assignee -label:human-review\") — label, assignee and text filters all live in search, not in separate arguments. " +
			"Rows come back as compact summaries; pass view:\"full\" only when you genuinely need every field. Page with cursor rather than a large perPage. Read-only. " +
			forgeParallelNote,
		Risk: domain.RiskRead,
		// Independent, bounded MCP snapshot read with no ordering dependency on its
		// batch siblings — see forgeRead's note.
		Parallelizable: true,
		Schema:         forgeListIssuesSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeListIssuesArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeListIssuesArgs
			if res, ok := decodeForge(args, "forge.listIssues", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "forge.listIssues", a.forwardArgs(), "")
		},
	}
}

func newForgeListPRsTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.listPRs",
		Description: "List the project's GitHub pull requests through Daintree's forge, with server-side filtering, paging and a compact default projection. " +
			"state narrows the listing and accepts \"merged\" as a distinct value. This tool has NO search argument — filter by state/sort/direction only. " +
			"Rows come back as compact summaries; pass view:\"full\" only when you genuinely need every field, and use forge.getPR for one PR's detail. Page with cursor rather than a large perPage. Read-only. " +
			forgeParallelNote,
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         forgeListPRsSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeListPRsArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeListPRsArgs
			if res, ok := decodeForge(args, "forge.listPRs", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "forge.listPRs", a.forwardArgs(), "")
		},
	}
}

func newForgeGetIssueTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getIssue",
		Description: "Fetch ONE GitHub issue's full detail through Daintree's forge. Pass issueNumber as a top-level positive integer (e.g. {\"issueNumber\": 299}) — never a string and never nested under an arguments object. " +
			"Use it after forge.listIssues to expand a row you picked, or when the user names an issue number. Read-only. " +
			"PARALLEL: several forge.* reads in ONE batch run concurrently, so fetch multiple issues in a single reply rather than one per turn.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         forgeGetIssueSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetIssueArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetIssueArgs
			if res, ok := decodeForge(args, "forge.getIssue", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "forge.getIssue", a.forwardArgs(), "")
		},
	}
}

func newForgeGetPRTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getPR",
		Description: "Fetch ONE GitHub pull request's full detail through Daintree's forge. Pass prNumber as a top-level positive integer (e.g. {\"prNumber\": 42}) — never a string and never nested under an arguments object. " +
			"PARALLEL: forge.getPR calls batched in ONE reply run concurrently — to check several PRs, emit one call each in one batch.",
		Risk: domain.RiskRead,
		// Independent per-PR read over the forge MCP, no ordering dependency on siblings:
		// checking several PRs at once overlaps their round-trips. See terminal.extract.
		Parallelizable: true,
		Schema:         forgeGetPRSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetPRArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetPRArgs
			if res, ok := decodeForge(args, "forge.getPR", &a); !ok {
				return res
			}
			return passthrough(ctx, tctx, "forge.getPR", a.forwardArgs(), "")
		},
	}
}

// decodeForge is strictDecode plus the args' own Validate(). Handlers can be
// reached directly (not only through the registry's Decode gate, which already
// runs Validate via StrictDecoder), and strictDecode is structural only — so the
// enum/bound refinements have to be re-asserted here or a direct call could
// forward perPage:0 to a host that will refuse it.
func decodeForge(raw json.RawMessage, name string, out tools.Validator) (tools.ToolResult, bool) {
	if res, ok := strictDecode(raw, name, out); !ok {
		return res, false
	}
	if err := out.Validate(); err != nil {
		return tools.Fail(codeInvalidArgs, fmt.Sprintf("Invalid arguments for %s: %s", name, err.Error())), false
	}
	return tools.ToolResult{}, true
}
