package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// Forge READS (risk read). listIssues/getIssue/listPRs forward an opaque
// arguments record verbatim (forge query options are forge-defined). getPR takes
// a typed {cwd?, prNumber} so the model can't drop the required positive integer.
// Forge wrappers (reads).

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

// forgeRead builds a read-only forge wrapper that forwards args.arguments ?? {}.
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

func newForgeListIssuesTool() *tools.Tool {
	return forgeRead("forge.listIssues", "List the project's GitHub issues through Daintree's forge integration. `arguments` is a forge-defined options record forwarded verbatim (state, labels, limit, …) — omit it for the default listing rather than inventing keys. Read-only. PARALLEL: forge.* reads batched in ONE reply run concurrently. MCP_UNAVAILABLE when Daintree is disconnected; a forge refusal returns MCP_TOOL_ERROR.")
}

func newForgeListPRsTool() *tools.Tool {
	return forgeRead("forge.listPRs", "List the project's GitHub pull requests through Daintree's forge integration. `arguments` is a forge-defined options record forwarded verbatim (state, limit, …) — omit it for the default listing rather than inventing keys. Read-only. PARALLEL: forge.* reads batched in ONE reply run concurrently. For one PR's full detail use forge.getPR with its prNumber.")
}

func newForgeGetIssueTool() *tools.Tool {
	return forgeRead("forge.getIssue", "Fetch ONE GitHub issue's detail through Daintree's forge integration. `arguments` is forwarded verbatim — pass the issue number under the key the forge expects (mirror what forge.listIssues returned); do not invent keys. Read-only. PARALLEL: several forge.* reads in ONE batch run concurrently, so fetch multiple issues in a single reply rather than one per turn.")
}

// forgeGetPRArgs is the typed shape for the by-number forge reads: a required positive
// prNumber (never a string) plus an optional worktree locator.
//
// All THREE locator spellings are accepted because the underlying forge actions accept
// all three (`worktreeLocationShape` with a legacy `cwd` alias), and these tools are on
// the daintree.call denylist — so a spelling the wrapper cannot express is a spelling
// with no path at all. A wrapper that narrows its action's arguments while blocking the
// escape hatch does not simplify the surface, it removes capability.
type forgeGetPRArgs struct {
	CWD          string `json:"cwd,omitempty"`
	WorktreeID   string `json:"worktreeId,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	PRNumber     int    `json:"prNumber"`
}

// forwardLocation copies whichever worktree locator was supplied into an outgoing call.
// Passing them through rather than resolving locally: the forge action owns the
// id-beats-path precedence, and duplicating that here would be a second implementation
// of a rule with one authority.
func (a forgeGetPRArgs) forwardLocation(fwd map[string]any) {
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

var forgeGetPRSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["prNumber"],
  "properties": {
    "cwd": { "type": "string", "description": "Legacy alias for worktreePath." },
    "worktreeId": { "type": "string", "description": "Worktree id (wt_…). Takes precedence over a path." },
    "worktreePath": { "type": "string", "description": "Absolute worktree path." },
    "prNumber": { "type": "integer", "minimum": 1, "description": "The PR number (positive integer)." }
  }
}`)

func newForgeGetPRTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getPR",
		Description: "Get a single forge (GitHub) pull request by number. " +
			"For several known numbers use forge.getPRs — one call, with a per-number status.",
		Risk: domain.RiskRead,
		// Independent per-PR read over the forge MCP, no ordering dependency on siblings:
		// checking several PRs at once overlaps their round-trips. See terminal.extract.
		Parallelizable: true,
		Schema:         forgeGetPRSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetPRArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetPRArgs
			if res, ok := strictDecode(args, "forge.getPR", &a); !ok {
				return res
			}
			if a.PRNumber <= 0 {
				return tools.Fail(codeInvalidArgs, "forge.getPR: prNumber must be a positive integer")
			}
			fwd := map[string]any{"prNumber": a.PRNumber}
			a.forwardLocation(fwd)
			return passthrough(ctx, tctx, "forge.getPR", fwd, "")
		},
	}
}

// forgeGetPRsArgs is the batch by-number read: 2-20 DISTINCT positive numbers plus
// the same optional worktree locator the singular read takes.
//
// The bounds are the forge action's own, restated here because StrictDecoder runs no
// schema engine — the declared minItems/maxItems would otherwise be advisory, and a
// 200-number call would be refused by Daintree after the round trip instead of before.
type forgeGetPRsArgs struct {
	CWD          string `json:"cwd,omitempty"`
	WorktreeID   string `json:"worktreeId,omitempty"`
	WorktreePath string `json:"worktreePath,omitempty"`
	PRNumbers    []int  `json:"prNumbers"`
}

// forgeGetPRsMin/Max are the batch bounds Daintree enforces. One number is excluded at
// the bottom deliberately: a singleton batch is forge.getPR with a worse result shape,
// and admitting it here would split one question across two tools for no gain.
const (
	forgeGetPRsMin = 2
	forgeGetPRsMax = 20
)

func (a forgeGetPRsArgs) forwardLocation(fwd map[string]any) {
	forgeGetPRArgs{CWD: a.CWD, WorktreeID: a.WorktreeID, WorktreePath: a.WorktreePath}.forwardLocation(fwd)
}

// The locator descriptions are terser than forgeGetPRSchema's on purpose. Every schema
// is re-sent on every model round (internal/app/toolbudget_test.go), the three fields
// are documented in full one tool away, and the meaning a batch adds is entirely in
// prNumbers — so that is where the bytes go.
var forgeGetPRsSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["prNumbers"],
  "properties": {
    "cwd": { "type": "string", "description": "Legacy alias for worktreePath." },
    "worktreeId": { "type": "string", "description": "Worktree id; beats a path." },
    "worktreePath": { "type": "string", "description": "Absolute worktree path." },
    "prNumbers": {
      "type": "array",
      "minItems": 2,
      "maxItems": 20,
      "uniqueItems": true,
      "items": { "type": "integer", "minimum": 1 },
      "description": "2-20 DISTINCT numbers you already know, from a listing or the user."
    }
  }
}`)

// newForgeGetPRsTool is the batch by-number read.
//
// It exists because the singular tool's own description used to teach the workaround:
// "to check several PRs, emit one call each in one batch". That was the best available
// answer while the forge exposed no plural lookup, and it cost one forge round trip per
// number — a ten-PR review spent ten of them, which is what daintreehq/daintree#12157
// was opened about.
//
// The per-number STATUS is the reason this is not just a speed-up, and why the
// description spends its bytes on it. The forge distinguishes three outcomes and the
// old IPC handler collapsed two of them by dropping the entry: `not_found` means the
// forge was asked and said the PR is not there, while `unresolved` means nothing was
// learned (rate limit, transport failure, a repo it could not reach). Reading the
// second as the first is how an agent closes work that exists, so the wrapper refuses
// to let the distinction stay implicit.
func newForgeGetPRsTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getPRs",
		Description: "Get 2-20 known forge (GitHub) pull requests in ONE call, instead of repeating forge.getPR. " +
			"Each number carries a status: `found`, `not_found` (asked; not there) or `unresolved` (nothing learned — rate limit or transport). " +
			"NEVER read `unresolved` as `not_found` — it is not evidence a PR is gone, and acting on it closes live work; re-read those numbers. " +
			"Not a search; discover with forge.listPRs.",
		Risk: domain.RiskRead,
		// One bounded MCP snapshot read like its siblings, so a batch of forge reads
		// still overlaps round-trips up to the governor's in-flight cap.
		Parallelizable: true,
		Schema:         forgeGetPRsSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetPRsArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetPRsArgs
			if res, ok := strictDecode(args, "forge.getPRs", &a); !ok {
				return res
			}
			if len(a.PRNumbers) < forgeGetPRsMin {
				// Name the singular tool rather than just the bound: a one-number call
				// is a real question with a real answer, and the recovery is a
				// different tool, not a longer list.
				return tools.Fail(codeInvalidArgs, fmt.Sprintf(
					"forge.getPRs: needs at least %d PR numbers (got %d). For a single PR call forge.getPR with its prNumber.",
					forgeGetPRsMin, len(a.PRNumbers)))
			}
			if len(a.PRNumbers) > forgeGetPRsMax {
				return tools.Fail(codeInvalidArgs, fmt.Sprintf(
					"forge.getPRs: at most %d PR numbers per call (got %d). Split them across calls — several forge reads in ONE batch run concurrently.",
					forgeGetPRsMax, len(a.PRNumbers)))
			}
			// Duplicates are rejected rather than deduplicated. Silently collapsing
			// [9,9,9] would let a three-number call clear the floor and return one
			// result, so the caller's list and the reply's would disagree about how
			// many PRs were asked about.
			seen := make(map[int]bool, len(a.PRNumbers))
			for _, n := range a.PRNumbers {
				if n <= 0 {
					return tools.Fail(codeInvalidArgs, fmt.Sprintf(
						"forge.getPRs: prNumbers must all be positive integers (got %d).", n))
				}
				if seen[n] {
					return tools.Fail(codeInvalidArgs, fmt.Sprintf(
						"forge.getPRs: prNumbers must be distinct; %d appears more than once.", n))
				}
				seen[n] = true
			}
			fwd := map[string]any{"prNumbers": a.PRNumbers}
			a.forwardLocation(fwd)
			return passthrough(ctx, tctx, "forge.getPRs", fwd, "")
		},
	}
}

// forgeChecksLagMs is roughly how far behind reality a CI reading can be. The forge
// caches check state provider-side, so a fresh call can already be about a minute old —
// which is why an immediate re-read may just return the same cached value, and why a
// single `success` is not a settled verdict.
const forgeChecksLagMs = 60000

// forgeCIStates is the closed set the forge action can return. An unrecognised value is
// downgraded rather than echoed: passing an unknown state through with conclusive=true
// would let a future provider spelling ("green") be read as a verdict this CLI never
// agreed to.
var forgeCIStates = map[string]bool{
	"success": true, "failure": true, "pending": true, "neutral": true, "unknown": true,
}

// newForgeGetChecksTool is the typed CI-state reader.
//
// It exists because "is this PR green?" is the most frequent question in the whole CI
// workflow, and it was the ONE common question with no typed wrapper — so the base
// prompt's "prefer the typed wrapper over the raw daintree.call escape hatch" pointed the
// model straight at the system-tier, confirmation-gated path, on nearly every turn of a
// fix-and-verify loop. Two runbooks had grown prose documenting that exception, which is a
// sign the tool surface was missing something rather than that the runbooks needed better
// wording.
//
// The value beyond ergonomics is that the raw result is genuinely easy to misread, and
// every consumer was re-learning the same traps from prose. The fix is not more prose: it
// is a `conclusive` field. Two different payloads mean "I could not tell you whether this
// is safe to merge" — a null status, and a status reporting ZERO required checks (which
// the forge also returns when it could not read the required list in full). The second is
// the dangerous one, because it arrives as `state: "success"`, and a caveat in a summary
// string is weaker than a machine-readable field saying "success". So the verdict and its
// trustworthiness are separate values, and a fix-and-verify loop can gate on the second.
//
// What it deliberately does NOT promise is per-check detail. The forge rolls every check
// up to one state and dispatch strips `rawData`, so "which check failed?" is unanswerable
// here (tracked as daintreehq/daintree#11786). Saying so in the result beats leaving the
// model to hunt for a field that does not exist.
func newForgeGetChecksTool() *tools.Tool {
	return &tools.Tool{
		Name: "forge.getChecks",
		Description: "Read a pull request's CI check state by PR number. " +
			"Gate on `conclusive`, NOT on `state`: conclusive=false means the state could not be determined — the forge returned nothing, or reported ZERO required checks, which it also does when it could not read the required list. That is never evidence a merge is safe, even when state is \"success\". " +
			"Counts cover REQUIRED checks only. Readings are provider-cached (~1m), so an immediate re-read may return the same value. No per-check breakdown exists, so this cannot say WHICH check failed. " +
			"PARALLEL: forge.* reads batched in ONE reply run concurrently.",
		Risk:           domain.RiskRead,
		Parallelizable: true,
		Schema:         forgeGetPRSchema,
		Decode:         tools.StrictDecoder(func() any { return &forgeGetPRArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a forgeGetPRArgs
			if res, ok := strictDecode(args, "forge.getChecks", &a); !ok {
				return res
			}
			if a.PRNumber <= 0 {
				return tools.Fail(codeInvalidArgs, "forge.getChecks: prNumber must be a positive integer")
			}
			fwd := map[string]any{"prNumber": a.PRNumber}
			a.forwardLocation(fwd)
			res := passthrough(ctx, tctx, "forge.getCIStatus", fwd, "")
			if !res.Ok {
				return res
			}
			return shapeForgeChecks(a.PRNumber, res)
		},
	}
}

// shapeForgeChecks turns the raw `{ciStatus: {...}|null}` passthrough into a verdict plus
// an explicit statement of whether that verdict can be trusted.
func shapeForgeChecks(prNumber int, res tools.ToolResult) tools.ToolResult {
	status, ok := forgeCIStatusFrom(res.Result)
	if !ok {
		// Deliberately no counts: a zero here would read as "0 checks failed", which is
		// the false reassurance this branch exists to avoid.
		return tools.Ok(
			fmt.Sprintf("PR #%d CI: INCONCLUSIVE — the forge reported no check state. This is not the same as having no checks, and is not evidence a merge is safe.", prNumber),
			map[string]any{
				"prNumber":       prNumber,
				"state":          "unknown",
				"conclusive":     false,
				"reported":       false,
				"perCheckDetail": false,
			})
	}

	state, _ := status["state"].(string)
	if !forgeCIStates[state] {
		state = "unknown"
	}
	out := map[string]any{
		"prNumber":   prNumber,
		"state":      state,
		"reported":   true,
		"mayLagByMs": forgeChecksLagMs,
		// Named rather than implied: every consumer was re-deriving this from prose.
		"countsCoverRequiredChecksOnly": true,
		"perCheckDetail":                false,
	}
	// Only counts that actually decoded. A missing or wrongly-typed field must not
	// silently become a zero the summary then reports as fact.
	counts := map[string]int{}
	for _, k := range []string{"total", "passed", "failed", "pending"} {
		if n, ok := numberFrom(status[k]); ok {
			counts[k] = n
			out[k] = n
		}
	}
	if v, present := status["requiredChecksPassing"].(bool); present {
		out["requiredChecksPassing"] = v
	}

	total, haveTotal := counts["total"]
	// The verdict is trustworthy only when the forge reported a state we recognise AND
	// actually saw required checks to judge. `total: 0` is the trap: it arrives with
	// state "success" and also means "the required-check list could not be read".
	conclusive := state != "unknown" && haveTotal && total > 0
	out["conclusive"] = conclusive

	if !conclusive {
		reason := "the forge reported no required checks (it returns this when the required-check list could not be read too)"
		if state == "unknown" {
			reason = "the forge reported an unknown state"
		}
		return tools.Ok(
			fmt.Sprintf("PR #%d CI: INCONCLUSIVE (state %q) — %s. Not evidence a merge is safe.", prNumber, state, reason),
			out)
	}
	summary := fmt.Sprintf("PR #%d CI: %s — %d/%d required passed", prNumber, state, counts["passed"], total)
	if n, ok := counts["failed"]; ok {
		summary += fmt.Sprintf(", %d failed", n)
	}
	if n, ok := counts["pending"]; ok {
		summary += fmt.Sprintf(", %d pending", n)
	}
	return tools.Ok(summary, out)
}

// forgeCIStatusFrom digs the ciStatus object out of a passthrough result, returning false
// for the null case and for any shape it does not recognise.
func forgeCIStatusFrom(result any) (map[string]any, bool) {
	top, ok := result.(map[string]any)
	if !ok {
		return nil, false
	}
	structured, ok := top["structuredContent"].(map[string]any)
	if !ok {
		return nil, false
	}
	status, ok := structured["ciStatus"].(map[string]any)
	if !ok || status == nil {
		return nil, false
	}
	return status, true
}

// numberFrom reads a JSON number in whichever Go form it decoded to.
func numberFrom(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}
