package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

/* ------------------------- project.runCheck / detectRunners ---------------- */

// Daintree's own bounds for project.runCheck's timeoutMs (shared/types/projectCheck.ts).
// Restated here because they are the schema this wrapper advertises to the model, and a
// bound the model can read is a bound it can respect; a bound only the server knows
// becomes a mid-turn 400 instead.
const (
	projectCheckMinTimeoutMS     = 1_000
	projectCheckDefaultTimeoutMS = 600_000   // 10 minutes
	projectCheckMaxTimeoutMS     = 3_600_000 // 1 hour
)

// projectCheckSettleMargin is how much longer we wait on the WIRE than the check itself
// is allowed to run, and it covers work on BOTH SIDES of the host's timeoutMs clock:
//
//   - BEFORE it starts. timeoutMs bounds the command, not the call. Daintree still has to
//     resolve the cwd and detect runners first, and cwd resolution can reach git — work
//     that is bounded but not instant, and that the check's own budget does not count.
//   - AFTER it elapses. Daintree kills the process tree, then drains the pipes, assembles
//     up to 50 KiB of output and marshals a result.
//
// A wire deadline of exactly timeoutMs would abort during either phase and report a
// transport error for a check that ran fine and had an answer to give — the failure mode
// that is worst here, because it is indistinguishable from a real one and wastes the
// entire (possibly hour-long) run. Erring large costs nothing: the host's own timeout
// still stops the command on schedule, and this deadline only ever backstops a server
// that has gone silent.
const projectCheckSettleMargin = 2 * time.Minute

// NO OUTPUT TRUNCATION HERE — deliberately.
//
// An earlier draft trimmed `output` to a few thousand characters so the whole result
// would stay inline under domain.MaxToolResultChars. That was wrong, and the reason is
// worth recording so it is not re-introduced as an optimisation.
//
// When a serialized ToolResult exceeds the cap, agent.SerializeToolResult does NOT drop
// the excess: it archives the FULL result as an artifact and returns a preview plus
// instructions to page it with artifact.read (internal/agent/serialize.go). So the
// platform already has a LOSSLESS overflow path. A wrapper that pre-truncates destroys
// output that path would have preserved — and because project.runCheck is on the
// daintree.call denylist, the raw route cannot recover it either. The dropped half of a
// failing test suite would simply be gone.
//
// The cost of passing it through is a paged read on a large failure, which is a round.
// The cost of trimming is diagnostic output that no longer exists anywhere. Those are not
// comparable, so the output is forwarded exactly as Daintree captured it (the host caps
// its own capture at 50 KiB and reports that in `outputTruncated`).
//
// The verdict survives the overflow regardless: the preview is the head of the serialized
// envelope, which begins with `ok` and `summary`, and the summary carries pass/fail, the
// exit code and the timed-out/aborted caveats.

type projectRunCheckArgs struct {
	ProjectID string `json:"projectId"`
	RunnerID  string `json:"runnerId"`
	// Pointer, not string+omitempty. StrictDecoder decodes and then RE-MARSHALS, so a
	// plain string cannot tell an explicit "" from an absent key at any point after
	// decoding — and dropping an explicit "" would silently retarget the run at the
	// project root, which is not what the caller asked for. The host declares this
	// `.min(1)`, so an empty value is an error there too.
	CWD *string `json:"cwd,omitempty"`
	// Pointer so "omitted" is distinguishable from an explicit value. Omitted must
	// stay omitted on the wire: the DEFAULT is Daintree's to apply, and forwarding our
	// idea of it would silently pin a value the host is free to change.
	TimeoutMS *int `json:"timeoutMs,omitempty"`
}

// Validate re-states the bounds the declared JSON Schema advertises. StrictDecoder
// rejects unknown fields and type mismatches but runs no schema engine, so minLength
// and the numeric range would be advisory only without this.
func (a projectRunCheckArgs) Validate() error {
	if strings.TrimSpace(a.ProjectID) == "" {
		return fmt.Errorf("projectId is required — pass the project's id (it rides this turn's runtime context)")
	}
	if strings.TrimSpace(a.RunnerID) == "" {
		return fmt.Errorf("runnerId is required — get it from project.detectRunners; runner ids are never guessable")
	}
	if a.CWD != nil && strings.TrimSpace(*a.CWD) == "" {
		return fmt.Errorf("cwd is empty; omit it to run in the project root rather than passing a blank path")
	}
	if a.TimeoutMS != nil && (*a.TimeoutMS < projectCheckMinTimeoutMS || *a.TimeoutMS > projectCheckMaxTimeoutMS) {
		return fmt.Errorf("timeoutMs is %d; Daintree accepts %d–%d (omit it for the %d default)",
			*a.TimeoutMS, projectCheckMinTimeoutMS, projectCheckMaxTimeoutMS, projectCheckDefaultTimeoutMS)
	}
	return nil
}

var projectRunCheckSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["projectId", "runnerId"],
  "properties": {
    "projectId": { "type": "string", "minLength": 1, "description": "The project whose command to run; its id rides this turn's runtime context." },
    "runnerId": { "type": "string", "minLength": 1, "description": "Which detected command to run — an id from project.detectRunners. Never guess one." },
    "cwd": { "type": "string", "minLength": 1, "description": "Run directory: the project root (default) or one of its worktrees. A PATH, not a worktree id." },
    "timeoutMs": { "type": "integer", "minimum": 1000, "maximum": 3600000, "default": 600000, "description": "Wall-clock ceiling in ms. Daintree kills the process tree when it elapses and reports timedOut:true." }
  }
}`)

// newProjectRunCheckTool runs one of a project's detected commands and reports a
// structured verdict.
//
// RISK. This is domain.RiskProject, not read, and the reasoning is Daintree's own: the
// action spawns a real child process (electron/services/mcp-server/projectCheck.ts),
// declares mcpAnnotations {readOnlyHint:false, idempotentHint:false}, and sits in
// DENY_PLUGIN_DISPATCH_ACTION_IDS next to terminal.sendCommand with the comment "same
// reasoning as terminal.sendCommand". Its `danger: "safe"` tier is advisory (see
// docs/DAINTREE_MCP.md) and describes intent, not effect. Running a project-defined
// shell command is the riskiest thing this handler does, and that is what Risk names.
// recipe.run — also a process spawn — is classified the same way here.
//
// It is therefore NOT parallelizable either: concurrent checks would contend for the
// same build directory and caches, and the double-gate on RiskRead would refuse the flag
// regardless.
//
// A FAILING CHECK IS A SUCCESSFUL CALL. The verdict lives in `passed`, so a red test
// suite returns ok with passed:false — never a tool error. Collapsing the two would make
// "the check failed" and "the check could not be run" the same result, and a fix-and-
// verify loop has to tell them apart.
func newProjectRunCheckTool() *tools.Tool {
	return &tools.Tool{
		Name: "project.runCheck",
		Description: "Run ONE of a project's detected commands (test, lint, build, …) and report its exit code and output. " +
			"Get runnerId from project.detectRunners — never guess one, and confirm what an unfamiliar id runs, since " +
			"detection surfaces every script, not only checks. A command that FAILS returns ok with passed:false: read the " +
			"verdict, not the call. Never point it at a long-lived server — it blocks until the timeout expires.",
		Consequence: "Runs a project-defined shell command to completion (up to its timeout) outside any visible terminal, then reports its exit code and output.",
		Risk:        domain.RiskProject,
		Schema:      projectRunCheckSchema,
		Decode:      tools.StrictDecoder(func() any { return &projectRunCheckArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a projectRunCheckArgs
			if res, ok := strictDecode(args, "project.runCheck", &a); !ok {
				return res
			}
			fwd := map[string]any{"projectId": a.ProjectID, "runnerId": a.RunnerID}
			if a.CWD != nil {
				fwd["cwd"] = *a.CWD
			}
			// Forward only what was actually supplied; the default stays Daintree's.
			//
			// When it is omitted we size the wire deadline from the host MAXIMUM rather
			// than from our copy of its default. Sizing it from the default would couple
			// this deadline to a number we deliberately do not forward — so if Daintree
			// ever raised its default, we would abort at OUR ten minutes while the host
			// was still legitimately working, and the drift would show up as a transport
			// error nobody could trace back to a constant. The deadline is only an upper
			// bound on a silent server; the host enforces the real timeout either way, so
			// the conservative choice costs nothing and cannot drift.
			budget := projectCheckMaxTimeoutMS
			if a.TimeoutMS != nil {
				fwd["timeoutMs"] = *a.TimeoutMS
				budget = *a.TimeoutMS
			}
			res := passthroughWithOptions(ctx, tctx, "project.runCheck", fwd, "",
				tools.MCPCallOptions{Timeout: time.Duration(budget)*time.Millisecond + projectCheckSettleMargin})
			if !res.Ok {
				return res
			}
			return shapeProjectCheck(a.RunnerID, res)
		},
	}
}

// projectCheckPassthroughFields are the verdict fields copied through verbatim. Named
// rather than copied wholesale so a field Daintree adds later stays out of the result
// until someone decides it belongs there — the same allowlist discipline the host uses
// on its own side.
var projectCheckPassthroughFields = []string{
	"projectId", "cwd", "runnerId", "runnerName", "command",
	"exitCode", "signalName", "durationMs", "timedOut", "aborted", "output", "outputTruncated",
}

// shapeProjectCheck turns the raw check result into a verdict the model can gate on.
//
// It reshapes rather than transforms: every field is copied through as Daintree sent it
// (see the no-truncation note at the top of this file), and the work is in the SUMMARY —
// naming pass/fail, and disclaiming the verdict when the check timed out or was aborted,
// because `passed:false` alone reads as "the code is broken" in both of those cases.
func shapeProjectCheck(runnerID string, res tools.ToolResult) tools.ToolResult {
	obj, ok := structuredFrom(res.Result)
	if !ok {
		return failMalformed("project.runCheck")
	}
	// `passed` is the whole point of the call. Absent or mistyped, there is no verdict
	// to report, and defaulting it either way would manufacture one.
	passed, hasPassed := obj["passed"].(bool)
	if !hasPassed {
		return failMalformed("project.runCheck")
	}

	out := map[string]any{"passed": passed}
	for _, k := range projectCheckPassthroughFields {
		if v, present := obj[k]; present {
			out[k] = v
		}
	}

	name, _ := obj["runnerName"].(string)
	if name == "" {
		name = runnerID
	}
	verdict := "FAILED"
	if passed {
		verdict = "passed"
	}
	summary := fmt.Sprintf("Check %s %s", name, verdict)
	if code, ok := numberFrom(obj["exitCode"]); ok {
		summary += fmt.Sprintf(" (exit %d)", code)
	}
	if t, _ := obj["timedOut"].(bool); t {
		summary += " — TIMED OUT, so this is not a verdict on the code"
	}
	if ab, _ := obj["aborted"].(bool); ab {
		summary += " — ABORTED before finishing, so this is not a verdict on the code"
	}
	return tools.Ok(summary+".", out)
}

/* --------------------------- project.detectRunners ------------------------- */

// ProjectID is a pointer, and there is deliberately NO minLength in the schema: the host
// declares a plain optional string and its `projectId ?? ctx.projectId` fallback keeps an
// explicit "" (only null/undefined fall through). So an explicit empty id is a value the
// host will reject on its own terms, and swallowing it here would silently retarget the
// call at the ACTIVE project — a different question than the one that was asked.
type projectDetectRunnersArgs struct {
	ProjectID *string `json:"projectId,omitempty"`
}

var projectDetectRunnersSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "projectId": { "type": "string", "description": "The project to inspect. Omit for the active project; the call fails when none is active." }
  }
}`)

// newProjectDetectRunnersTool lists the runnable commands a project defines.
//
// It is here because project.runCheck REQUIRES a runnerId, and a runner id is not
// derivable from a package.json by inspection — Daintree synthesizes conventional
// commands for some frameworks that are declared nowhere. This is the CANONICAL, direct
// source for one: workflow.prepBranchForReview also returns detectedRunners, but only as
// a by-product of a much larger readiness-and-git operation that is wrong to invoke just
// to learn a command's id. Without a direct wrapper the model would reach for the
// system-tier daintree.call escape hatch to take the FIRST step of every verification
// loop, which is exactly the second-class path issue #367 exists to remove. Pure query on
// Daintree's side (kind: "query", no process spawn), so read risk.
func newProjectDetectRunnersTool() *tools.Tool {
	return &tools.Tool{
		Name: "project.detectRunners",
		Description: "List the runnable commands a project defines, by inspecting its manifests. The direct source of the " +
			"runnerId project.runCheck requires — never guess one. It returns every script it finds, publish and deploy " +
			"included, and synthesizes conventional commands for some frameworks, so check what an unfamiliar runner runs " +
			"before running it.",
		Risk: domain.RiskRead,
		// Independent, bounded manifest read with no ordering dependency on its batch
		// siblings — the same opt-in as the forge reads.
		Parallelizable: true,
		Schema:         projectDetectRunnersSchema,
		Decode:         tools.StrictDecoder(func() any { return &projectDetectRunnersArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a projectDetectRunnersArgs
			if res, ok := strictDecode(args, "project.detectRunners", &a); !ok {
				return res
			}
			fwd := map[string]any{}
			if a.ProjectID != nil {
				fwd["projectId"] = *a.ProjectID
			}
			res := passthrough(ctx, tctx, "project.detectRunners", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("project.detectRunners")
			}
			// Presence-checked, not defaulted: a missing or mistyped `runners` decoded
			// with `_` would become a nil slice reported as "Detected 0 runnable
			// command(s)" — a confident claim that the project defines none, built from
			// a payload that said nothing of the kind.
			runners, ok2 := obj["runners"].([]any)
			if !ok2 {
				return failMalformed("project.detectRunners")
			}
			return tools.Ok(fmt.Sprintf("Detected %d runnable command(s).", len(runners)),
				map[string]any{"runners": runners})
		},
	}
}
