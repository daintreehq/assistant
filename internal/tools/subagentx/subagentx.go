// Package subagentx is the delegation tool family: subagent.run dispatches a
// bounded, read-only sub-agent (internal/subagent) to answer one question in its
// own conversation, and returns a compact report.
//
// It is the model-facing half of the context-isolation design. The runner does
// the work; this package's job is to make the SHAPE of a good delegation obvious
// from the schema alone, because the failure mode here is not a crash — it is a
// vague brief that spends ten rounds and comes back with "there are several
// candidates". So the arguments are deliberately not a single free-text prompt:
// task, context, and deliverable are separate fields precisely so the model has
// to state what it wants back, and the descriptions carry worked examples rather
// than abstractions.
//
// The result is small on purpose. What crosses back is the report, the counters,
// and a transcript id — never the sub-agent's tool output. A caller that needs
// more pages the transcript with artifact.read, which keeps the expensive detail
// one explicit step away instead of in the conversation by default.
package subagentx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/subagent"
	"github.com/daintreehq/assistant/internal/tools"
)

// Tool-error codes.
const (
	codeUnavailable = "SUBAGENT_UNAVAILABLE"
	codeFailed      = "SUBAGENT_FAILED"
)

// Bounds on the brief text itself. A task longer than this is not a delegation,
// it is the caller trying to paste its own context into the sub-agent — which
// defeats the purpose and is what the `context` field exists to do properly.
const (
	maxTaskRunes        = 4_000
	maxContextRunes     = 8_000
	maxDeliverableRunes = 1_000
)

// Runner is the seam over *subagent.Runner (a fake satisfies it in tests).
type Runner interface {
	Run(ctx context.Context, brief subagent.Brief, progress subagent.Progress) subagent.Report
}

// Deps wires the family. A nil Runner leaves the tool registered but failing
// cleanly, so the projected inventory stays stable across runs and configurations
// — the same posture questionx takes for a non-interactive session.
type Deps struct {
	Runner Runner
}

type runArgs struct {
	Task        string `json:"task"`
	Context     string `json:"context,omitempty"`
	Deliverable string `json:"deliverable,omitempty"`
	MaxRounds   *int   `json:"maxRounds,omitempty"`
}

// Validate enforces the bounds as the STRICT decoder's validation step, so a
// violation is an INVALID_ARGS the model can correct rather than a silent clamp
// it never learns about.
func (a *runArgs) Validate() error {
	if strings.TrimSpace(a.Task) == "" {
		return fmt.Errorf("task is required — state the question the sub-agent must answer")
	}
	if overLimit(a.Task, maxTaskRunes) {
		return fmt.Errorf("task is too long (max %d characters) — a brief this size means you are pasting context; put background in 'context' instead", maxTaskRunes)
	}
	if overLimit(a.Context, maxContextRunes) {
		return fmt.Errorf("context is too long (max %d characters)", maxContextRunes)
	}
	if overLimit(a.Deliverable, maxDeliverableRunes) {
		return fmt.Errorf("deliverable is too long (max %d characters) — name the shape of the answer, don't write it", maxDeliverableRunes)
	}
	if a.MaxRounds != nil && (*a.MaxRounds < 1 || *a.MaxRounds > subagent.MaxRoundsCeiling) {
		return fmt.Errorf("maxRounds must be between 1 and %d", subagent.MaxRoundsCeiling)
	}
	return nil
}

func overLimit(s string, n int) bool { return len([]rune(s)) > n }

var runSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "task": {
      "type": "string",
      "description": "The ONE question the sub-agent must answer, as a standalone brief — it sees none of this conversation. Name the thing concretely: \"Find the GitHub issue describing terrain mesh flicker at chunk borders\". Not a topic (\"terrain bugs\"), not a plan, not a multi-part errand."
    },
    "context": {
      "type": "string",
      "description": "Optional. Background it cannot discover itself: a constraint the user stated, a decision made earlier, an id you already hold. Never paste file contents or tool output — it can fetch those itself, and pasting re-spends the context this call exists to save."
    },
    "deliverable": {
      "type": "string",
      "description": "Optional but strongly recommended: the shape of the answer you need, e.g. \"the issue number, title, and URL\". This is what separates a report you can act on from one you have to re-read."
    },
    "maxRounds": {
      "type": "number",
      "minimum": 1,
      "maximum": 24,
      "description": "Optional round budget (default 10). A round is one sub-agent message plus its tool calls. Raise it only for a genuinely wide search; a well-aimed brief finishes in three to six."
    }
  },
  "required": ["task"]
}`)

// Tools returns the delegation family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{{
		Name: "subagent.run",
		// Compacted to the ordinary 600-char description budget (see
		// app/toolbudget_test.go). What is here is what the model cannot infer from the
		// schema and gets WRONG without being told: that the report is all that comes
		// back, that the brief must stand alone, that it cannot mutate or ask, and that
		// calls fan out. The worked examples of a good brief live on `task` itself,
		// beside the field they govern.
		Description: "Delegate ONE research question to a read-only sub-agent working in its own separate conversation; only its short report comes back. " +
			"Use it when FINDING an answer costs far more context than the answer — one issue among thousands, a symbol across an unfamiliar tree, exploring a tree before a copyTree scope. Nothing it reads enters this conversation. " +
			"It has read-only tools only (no edits, spawns, terminal input, forge writes) and cannot ask questions, so the brief must stand alone. " +
			"Fan out independent questions as several calls in one batch. Its transcriptId pages the full run via artifact.read.",
		Risk:   domain.RiskRead,
		Schema: runSchema,
		Decode: tools.StrictDecoder(func() any { return &runArgs{} }),
		// Read-only, side-effect-free, and independent of its batch siblings — the
		// exact contract Parallelizable requires. This is the concurrency that
		// matters for delegation: three questions fan out into three sub-agents that
		// run at once, instead of three minutes of serial searching.
		Parallelizable: true,
		Handle: func(ctx context.Context, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			return handle(ctx, deps, raw, tctx)
		},
	}}
}

func handle(ctx context.Context, deps Deps, raw json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
	var a runArgs
	_ = json.Unmarshal(raw, &a)

	if deps.Runner == nil {
		return tools.Fail(codeUnavailable,
			"Delegation is not available in this context, so this question has to be answered on the main thread.",
			tools.Unrecoverable())
	}

	brief := subagent.Brief{
		Task:        strings.TrimSpace(a.Task),
		Context:     strings.TrimSpace(a.Context),
		Deliverable: strings.TrimSpace(a.Deliverable),
	}
	if a.MaxRounds != nil {
		brief.MaxRounds = *a.MaxRounds
	}

	// Forward the sub-agent's round-by-round progress to this call's activity row.
	// Without it the cockpit shows one frozen row for the whole run — which can be
	// a minute — and a delegation that looks hung is one the user cancels.
	progress := progressForwarder(tctx)

	rep := deps.Runner.Run(ctx, brief, progress)

	if rep.Status == subagent.StatusFailed {
		// A failed sub-agent is a RECOVERABLE tool failure: the caller can retry
		// with a tighter brief, or simply do the work itself. Marking it
		// unrecoverable would tell the model to give up on the question, which is
		// never the right conclusion from "the delegate could not run".
		return tools.Fail(codeFailed, failMessage(rep), tools.WithDetails(rep))
	}
	return tools.Ok(summarize(rep), rep)
}

// progressForwarder adapts the runner's progress callback to the registry's
// ReportProgress beat. Nil-safe at both ends: a ToolContext without a progress
// hook (tests, non-interactive actors) simply yields no forwarder.
func progressForwarder(tctx *tools.ToolContext) subagent.Progress {
	if tctx == nil || tctx.ReportProgress == nil {
		return nil
	}
	report := tctx.ReportProgress
	return func(msg string) {
		report(tools.ToolProgress{Phase: tools.ProgressRunning, Message: msg})
	}
}

// summarize is the one line a human sees in the activity tree and the model reads
// first. It leads with the outcome, because a PARTIAL report that reads like a
// complete one is the single most damaging thing this tool can return.
func summarize(rep subagent.Report) string {
	// No "Sub-agent" prefix: the activity row already renders the verb, and
	// repeating it produced "Sub-agent  Sub-agent reported back" — the same stutter
	// terminal.close's "Ended" verb exists to avoid.
	var sb strings.Builder
	switch rep.Status {
	case subagent.StatusCompleted:
		sb.WriteString("Reported back")
	case subagent.StatusExhausted:
		// PARTIAL leads, because this string is budgeted and truncated by the row
		// renderer — and a partial finding that reads as a settled one is the most
		// damaging thing this tool can return.
		sb.WriteString("PARTIAL — stopped early")
	case subagent.StatusCancelled:
		sb.WriteString("Cancelled")
	default:
		sb.WriteString("Ended (" + string(rep.Status) + ")")
	}
	sb.WriteString(fmt.Sprintf(" · %d round%s, %d tool call%s",
		rep.Rounds, plural(rep.Rounds), rep.ToolCalls, plural(rep.ToolCalls)))
	if rep.FailedCalls > 0 {
		sb.WriteString(fmt.Sprintf(" (%d failed)", rep.FailedCalls))
	}
	if rep.DurationMS > 0 {
		sb.WriteString(fmt.Sprintf(" · %.1fs", float64(rep.DurationMS)/1000))
	}
	return sb.String()
}

func failMessage(rep subagent.Report) string {
	msg := "The sub-agent could not complete the task"
	if rep.Note != "" {
		msg += ": " + rep.Note
	} else {
		msg += "."
	}
	return msg + " Retry with a narrower brief, or do the work on this thread."
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
