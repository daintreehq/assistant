package mcpwrap

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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

// projectCheckSettleMargin is how much longer we wait on the WIRE than the check is
// allowed to run. When timeoutMs elapses Daintree kills the process tree and then still
// has to drain the pipes, assemble the output and marshal a result — work that happens
// after the nominal deadline. A wire deadline equal to timeoutMs would abort during
// exactly that settlement and report a transport error for a check that did finish and
// had an answer to give.
const projectCheckSettleMargin = 30 * time.Second

// projectCheckOutputBudget bounds the `output` field this wrapper returns. A check's
// output is unbounded by nature (a failing test suite prints megabytes), and the whole
// serialized ToolResult must stay under domain.MaxToolResultChars or the runtime pages
// it into an artifact — turning a verdict the model needs THIS round into a fetch it
// must make next round. Only `output` is shrunk, and never silently: the result says how
// much was dropped and from which end.
//
// Deliberately well under the cap: the verdict fields, the command, the runner name and
// the summary all share that budget, and the tail is the useful half of a check's output
// (the failure summary a runner prints last), so it is the tail we keep.
const projectCheckOutputBudget = 5_000

type projectRunCheckArgs struct {
	ProjectID string `json:"projectId"`
	RunnerID  string `json:"runnerId"`
	CWD       string `json:"cwd,omitempty"`
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
			"verdict, not the call. Never point it at a long-lived server — it blocks until the timeout expires. Long output " +
			"is trimmed from the FRONT and says so.",
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
			if a.CWD != "" {
				fwd["cwd"] = a.CWD
			}
			// Forward only what was actually supplied; the default stays Daintree's.
			budget := projectCheckDefaultTimeoutMS
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
	"exitCode", "signalName", "durationMs", "timedOut", "aborted", "outputTruncated",
}

// shapeProjectCheck turns the raw check result into a verdict the model can gate on.
//
// The one transformation is on `output`: it is trimmed to a budget so the whole result
// stays inline. Trimming happens from the FRONT because a runner prints its failure
// summary last, and it is announced in a dedicated field rather than by an ellipsis a
// reader has to notice — an unannounced trim reads as "this is the whole output", which
// is how a model concludes a suite passed from a fragment that never reached the errors.
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

	output, _ := obj["output"].(string)
	kept, dropped := trimHead(output, projectCheckOutputBudget)
	out["output"] = kept
	// Two distinct truncations: Daintree's own (it caps what it captures) and ours.
	// Reported separately because they mean different things — the host's says the
	// check produced more than it recorded, ours says we did not relay all it recorded.
	out["outputTrimmedByAssistant"] = dropped > 0
	if dropped > 0 {
		out["outputCharsDropped"] = dropped
		out["outputTrimmedFrom"] = "start"
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
	if dropped > 0 {
		summary += fmt.Sprintf("; output trimmed by %d chars from the start", dropped)
	}
	return tools.Ok(summary+".", out)
}

// trimHead keeps the last `budget` runes of s, returning the kept text and how many
// runes were dropped. Rune-based to match the serializer's own character accounting, so
// a multibyte-heavy log is measured the way the cap measures it.
func trimHead(s string, budget int) (string, int) {
	n := utf8.RuneCountInString(s)
	if n <= budget {
		return s, 0
	}
	runes := []rune(s)
	return string(runes[n-budget:]), n - budget
}

/* --------------------------- project.detectRunners ------------------------- */

type projectDetectRunnersArgs struct {
	ProjectID string `json:"projectId,omitempty"`
}

var projectDetectRunnersSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "projectId": { "type": "string", "minLength": 1, "description": "The project to inspect. Omit for the active project; the call fails when none is active." }
  }
}`)

// newProjectDetectRunnersTool lists the runnable commands a project defines.
//
// It is here because project.runCheck REQUIRES a runnerId and there is no other way to
// obtain one — detection is the only source, and a runner id is not derivable from a
// package.json by inspection (Daintree synthesizes conventional commands for some
// frameworks that are declared nowhere). Wrapping the check without wrapping its one
// discovery step would leave the model reaching for the system-tier daintree.call escape
// hatch to take the first step of every verification loop, which is exactly the
// second-class path issue #367 exists to remove. Pure query on Daintree's side
// (kind: "query", no process spawn), so read risk.
func newProjectDetectRunnersTool() *tools.Tool {
	return &tools.Tool{
		Name: "project.detectRunners",
		Description: "List the runnable commands a project defines, by inspecting its manifests. The ONLY source of the " +
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
			if a.ProjectID != "" {
				fwd["projectId"] = a.ProjectID
			}
			res := passthrough(ctx, tctx, "project.detectRunners", fwd, "")
			if !res.Ok {
				return res
			}
			obj, ok := structuredFrom(res.Result)
			if !ok {
				return failMalformed("project.detectRunners")
			}
			runners, _ := obj["runners"].([]any)
			return tools.Ok(fmt.Sprintf("Detected %d runnable command(s).", len(runners)),
				map[string]any{"runners": runners})
		},
	}
}
