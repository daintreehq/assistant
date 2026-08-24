// Package runbook holds the local runbook RUN-STATE tools: runbook.run.get and
// runbook.step.advance. These track stepwise progress keyed to the live session so a
// multi-step runbook can resume.
//
// Runbook SELECTION (runbook.find / runbook.load) is NOT here: it is server-owned. The
// backend's selector picks and injects runbooks; the CLI only records progress
// through the steps the backend prompt directs the model to advance.
package runbook

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/assistant/internal/debuglog"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

const (
	codeInvalidArgs = "INVALID_ARGS"
	codeNoSession   = "RUNBOOK_RUN_NO_SESSION"
)

// RunbookStore is the slice of storage the run-state tools touch. Natural key is
// (sessionId, runbookId).
type RunbookStore interface {
	GetRunbookRunState(ctx context.Context, sessionID, runbookID string) (*domain.RunbookRunStateRecord, error)
	InsertRunbookRunState(ctx context.Context, rec domain.RunbookRunStateRecord) (string, error)
	UpdateRunbookRunState(ctx context.Context, rec domain.RunbookRunStateRecord) error
}

// Deps is the dependency set for the runbook family. Any of these may be nil; the
// corresponding tool then returns its specific "unavailable" code (so a stripped
// test/non-main context fails gracefully rather than panicking).
//
// runbook.find / runbook.load are NOT part of this family anymore: runbook selection is
// server-owned (the backend's selector picks and injects runbooks), so the CLI only
// keeps the local run-state tools (runbook.run.get / runbook.step.advance) the backend
// prompt drives.
type Deps struct {
	Store RunbookStore
	// CheckConsistency optionally runs a small-tier judge over a runbook.step.advance
	// transition and returns its verdict — surfacing semantically-wrong Director
	// decisions (a bad jump, a regression, a premature finish) that a clean ok=true
	// tool call would otherwise hide (issue #240). nil ⇒ the judge is skipped (tests,
	// contexts without model access) at zero cost; the free state-delta log still
	// fires. The call itself is also gated on debug logging by observeStepAdvance, so
	// a wired checker never runs in a normal (non-debug) session.
	CheckConsistency func(ctx context.Context, in ConsistencyCheckInput) (domain.ModelJudgeAnswer, error)
}

// ConsistencyCheckInput is the before/after snapshot of one runbook.step.advance handed
// to a small-tier consistency judge. It carries ONLY state-transition shape (step
// indices, statuses, the progress arrays) — never the runbook body — so the check
// stays cheap, and uses plain domain values so the runbook package needs no model
// imports.
type ConsistencyCheckInput struct {
	RunbookID       string
	RunID           string
	SessionID       string
	CompletedStep   int
	NextStep        *int // nil ⇒ this advance finished the run
	StepStatus      domain.RunbookStepStatus
	Notes           string
	PrevCurrentStep int
	NextCurrentStep int
	PrevRunStatus   domain.RunbookRunStatus
	NextRunStatus   domain.RunbookRunStatus
	BeforeSteps     []domain.RunbookStepProgress
	AfterSteps      []domain.RunbookStepProgress
}

// Tools returns the runbook run-state tool family (runbook.run.get / runbook.step.advance).
// Selection (runbook.find / runbook.load) is server-owned and intentionally absent.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newRunGetTool(deps),
		newStepAdvanceTool(deps),
	}
}

// --- runbook.run.get ---

type runGetArgs struct {
	RunbookID string `json:"runbookId"`
}

var runGetSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["runbookId"],
  "properties": { "runbookId": { "type": "string", "minLength": 1 } }
}`)

func newRunGetTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "runbook.run.get",
		Description: "Read your stepwise progress through a loaded runbook in THIS session: currentStep, per-step status (done|skipped) with notes and timestamps, and the run status. Absence is a NORMAL ok answer ({state: null}) — it means no step has been advanced yet, not an error. Use it after a compaction or a wake turn to find where you left off. Requires a live session.",
		Risk:        domain.RiskRead,
		Schema:      runGetSchema,
		Decode:      tools.StrictDecoder(func() any { return &runGetArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a runGetArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for runbook.run.get: "+err.Error())
			}
			id := strings.TrimSpace(a.RunbookID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "runbook.run.get: runbookId is required")
			}
			if tctx.SessionID == "" {
				return tools.Fail(codeNoSession, "runbook.run.get requires a live session.", tools.Unrecoverable())
			}
			if deps.Store == nil {
				return tools.Ok("No runbook run state.", map[string]any{"state": nil})
			}
			rec, err := deps.Store.GetRunbookRunState(ctx, tctx.SessionID, id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "runbook.run.get: "+err.Error())
			}
			if rec == nil {
				return tools.Ok("No runbook run state.", map[string]any{"state": nil})
			}
			return tools.Ok("Runbook run state for "+id+".", map[string]any{"state": runStateView(rec)})
		},
	}
}

// --- runbook.step.advance ---

type stepAdvanceArgs struct {
	RunbookID     string `json:"runbookId"`
	CompletedStep int    `json:"completedStep"`
	NextStep      *int   `json:"nextStep,omitempty"`
	Status        string `json:"status,omitempty"` // done | skipped (default done)
	Notes         string `json:"notes,omitempty"`
}

var stepAdvanceSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["runbookId", "completedStep"],
  "properties": {
    "runbookId": { "type": "string", "minLength": 1 },
    "completedStep": { "type": "integer", "minimum": 1 },
    "nextStep": { "type": "integer", "minimum": 1 },
    "status": { "type": "string", "enum": ["done", "skipped"] },
    "notes": { "type": "string" }
  }
}`)

func newStepAdvanceTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "runbook.step.advance",
		Description: "Record that step N of a loaded runbook is done (or skipped) and move to nextStep; OMIT nextStep to finish the run. Call it as you complete each step, not in one batch at the end — this is the progress record a compaction or a wake turn reads back through runbook.run.get. currentStep never regresses. notes is an optional one-liner about what the step produced. Requires a live session.",
		Risk:        domain.RiskLocal,
		Schema:      stepAdvanceSchema,
		Decode:      tools.StrictDecoder(func() any { return &stepAdvanceArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a stepAdvanceArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for runbook.step.advance: "+err.Error())
			}
			id := strings.TrimSpace(a.RunbookID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "runbook.step.advance: runbookId is required")
			}
			if a.CompletedStep < 1 {
				return tools.Fail(codeInvalidArgs, "runbook.step.advance: completedStep must be >= 1")
			}
			if a.NextStep != nil && *a.NextStep < 1 {
				return tools.Fail(codeInvalidArgs, "runbook.step.advance: nextStep must be >= 1")
			}
			stepStatus := domain.RunbookStepDone
			switch a.Status {
			case "", "done":
				stepStatus = domain.RunbookStepDone
			case "skipped":
				stepStatus = domain.RunbookStepSkipped
			default:
				return tools.Fail(codeInvalidArgs, "runbook.step.advance: status must be done|skipped")
			}
			if tctx.SessionID == "" {
				return tools.Fail(codeNoSession, "runbook.step.advance requires a live session.", tools.Unrecoverable())
			}
			if deps.Store == nil {
				return tools.Fail(domain.CodeInternal, "runbook.step.advance: storage unavailable")
			}

			now := domain.NowMS()
			existing, err := deps.Store.GetRunbookRunState(ctx, tctx.SessionID, id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "runbook.step.advance: "+err.Error())
			}

			finished := a.NextStep == nil

			var steps []domain.RunbookStepProgress
			var prevCurrent int
			var prevRunStatus domain.RunbookRunStatus
			if existing != nil {
				steps = decodeSteps(existing.StepsJson)
				prevCurrent = existing.CurrentStep
				prevRunStatus = existing.Status
			}
			// issue #240: snapshot the pre-advance progress for the decision-correctness
			// side-channel BEFORE upsertStep mutates `steps` in place. Only clone when
			// debug logging is on — the snapshot is unused otherwise (zero normal-run cost).
			var beforeSteps []domain.RunbookStepProgress
			if tctx.Config.DebugLog {
				beforeSteps = append([]domain.RunbookStepProgress(nil), steps...)
			}
			// Upsert the completed step into the sorted array.
			var notesPtr *string
			if a.Notes != "" {
				n := a.Notes
				notesPtr = &n
			}
			upsertStep(&steps, domain.RunbookStepProgress{
				Index:  a.CompletedStep,
				Status: stepStatus,
				Notes:  notesPtr,
				Ts:     now,
			})

			// currentStep: on finish it's the completed step; otherwise it never
			// regresses below the prior current.
			currentStep := a.CompletedStep
			if !finished {
				currentStep = *a.NextStep
				if prevCurrent > currentStep {
					currentStep = prevCurrent
				}
			}

			stepsJSON, _ := json.Marshal(steps)
			runStatus := domain.RunbookRunActive
			if finished {
				runStatus = domain.RunbookRunCompleted
			}

			// issue #240 decision-correctness side-channel input, populated once; it is
			// emitted only AFTER a successful persist in each branch below (a failed DB
			// write must not log an "advance occurred" event). `steps` is the post-upsert
			// (after) state; beforeSteps holds the pre-upsert snapshot.
			observeIn := ConsistencyCheckInput{
				RunbookID:       id,
				RunID:           tctx.RunID,
				SessionID:       tctx.SessionID,
				CompletedStep:   a.CompletedStep,
				NextStep:        a.NextStep,
				StepStatus:      stepStatus,
				Notes:           a.Notes,
				PrevCurrentStep: prevCurrent,
				NextCurrentStep: currentStep,
				PrevRunStatus:   prevRunStatus,
				NextRunStatus:   runStatus,
				BeforeSteps:     beforeSteps,
				AfterSteps:      steps,
			}

			if existing == nil {
				rec := domain.RunbookRunStateRecord{
					ID:          domain.NewID(domain.PrefixRunbookRun),
					SessionID:   tctx.SessionID,
					RunbookID:   id,
					CurrentStep: currentStep,
					StepsJson:   string(stepsJSON),
					Status:      runStatus,
					StartedAt:   now,
					UpdatedAt:   now,
				}
				// Stamp completedAt ONLY on finish (the SQL-NULL hazard: a non-finish
				// upsert must never set it).
				if finished {
					rec.CompletedAt = &now
				}
				if _, err := deps.Store.InsertRunbookRunState(ctx, rec); err != nil {
					return tools.Fail(domain.CodeInternal, "runbook.step.advance: "+err.Error())
				}
				observeStepAdvance(ctx, deps, tctx, observeIn)
				return tools.Ok(advanceSummary(id, finished, currentStep), map[string]any{"state": runStateView(&rec)})
			}

			existing.CurrentStep = currentStep
			existing.StepsJson = string(stepsJSON)
			existing.Status = runStatus
			existing.UpdatedAt = now
			// Preserve a prior completedAt; only stamp it on this finish if unset.
			if finished && existing.CompletedAt == nil {
				existing.CompletedAt = &now
			}
			if err := deps.Store.UpdateRunbookRunState(ctx, *existing); err != nil {
				return tools.Fail(domain.CodeInternal, "runbook.step.advance: "+err.Error())
			}
			observeStepAdvance(ctx, deps, tctx, observeIn)
			return tools.Ok(advanceSummary(id, finished, currentStep), map[string]any{"state": runStateView(existing)})
		},
	}
}

// observeStepAdvance emits the issue #240 decision-correctness side-channels for a
// SUCCESSFUL runbook.step.advance: a free before/after state-delta line, and — when a
// consistency judge is wired — its verdict. BOTH are gated on debug logging (zero
// overhead in normal runs) and the whole body runs inside a recover() so a logging or
// model-call failure can NEVER break the tool result (the same best-effort guarantee
// dispatch.audit() gives the audit/debug side-channels). The verdict event records
// checkOk=false with the error when the judge itself fails, so log archaeology can
// tell "model said the advance is fine" apart from "the check could not run".
func observeStepAdvance(ctx context.Context, deps Deps, tctx *tools.ToolContext, in ConsistencyCheckInput) {
	if tctx == nil || !tctx.Config.DebugLog {
		return
	}
	defer func() { _ = recover() }()

	cfg := debuglog.Config{DebugLog: tctx.Config.DebugLog, LogDir: tctx.Config.LogDir}

	// nextStep renders as the literal "null" when the advance finished the run.
	var nextStep any = debuglog.Null
	if in.NextStep != nil {
		nextStep = *in.NextStep
	}
	debuglog.LogDebug(cfg, "runbook.step.delta", map[string]any{
		"runId":           in.RunID,
		"sessionId":       in.SessionID,
		"runbookId":       in.RunbookID,
		"completedStep":   in.CompletedStep,
		"nextStep":        nextStep,
		"status":          string(in.StepStatus),
		"prevCurrentStep": in.PrevCurrentStep,
		"nextCurrentStep": in.NextCurrentStep,
		"prevRunStatus":   string(in.PrevRunStatus),
		"nextRunStatus":   string(in.NextRunStatus),
		"before":          in.BeforeSteps,
		"after":           in.AfterSteps,
	})

	if deps.CheckConsistency != nil {
		// NOTE: the judge runs SYNCHRONOUSLY here (after the DB write, before the tool
		// returns). That is acceptable because the whole side-channel is debug-gated —
		// a developer explicitly opted in — and it keeps the log ordering deterministic
		// and the {input → verdict} pair testable. The trade-off: in a slow/hung small-
		// tier environment it adds the model round-trip's latency to runbook.step.advance
		// (the delta line above has already been written, so logs are unaffected).
		logConsistencyCheck(ctx, cfg, deps.CheckConsistency, in)
	}
}

// logConsistencyCheck runs the wired judge and records its verdict. It carries its OWN
// recover so a PANICKING judge still surfaces a checkOk=false event — without this, a
// crashed check would be swallowed by observeStepAdvance's outer guard and become
// indistinguishable from "no checker wired" in the log. A model/decode error is logged
// the same truthful way (checkOk=false + the error) so log archaeology can tell a model
// that judged the advance fine apart from a check that never produced a verdict.
func logConsistencyCheck(ctx context.Context, cfg debuglog.Config, check func(context.Context, ConsistencyCheckInput) (domain.ModelJudgeAnswer, error), in ConsistencyCheckInput) {
	defer func() {
		if r := recover(); r != nil {
			debuglog.LogDebug(cfg, "runbook.step.consistency", map[string]any{
				"runId":         in.RunID,
				"runbookId":     in.RunbookID,
				"completedStep": in.CompletedStep,
				"checkOk":       false,
				"error":         fmt.Sprintf("panic: %v", r),
			})
		}
	}()
	ans, err := check(ctx, in)
	if err != nil {
		debuglog.LogDebug(cfg, "runbook.step.consistency", map[string]any{
			"runId":         in.RunID,
			"runbookId":     in.RunbookID,
			"completedStep": in.CompletedStep,
			"checkOk":       false,
			"error":         err.Error(),
		})
		return
	}
	debuglog.LogDebug(cfg, "runbook.step.consistency", map[string]any{
		"runId":         in.RunID,
		"runbookId":     in.RunbookID,
		"completedStep": in.CompletedStep,
		"checkOk":       true,
		"flagged":       ans.Matched,
		"confidence":    ans.Confidence,
		"reason":        ans.Reason,
	})
}

func advanceSummary(id string, finished bool, currentStep int) string {
	if finished {
		return "Completed runbook run for " + id + "."
	}
	return fmt.Sprintf("Advanced runbook %s to step %d.", id, currentStep)
}

// upsertStep inserts or replaces the step with the same Index, keeping the slice
// sorted ascending by Index.
func upsertStep(steps *[]domain.RunbookStepProgress, step domain.RunbookStepProgress) {
	for i := range *steps {
		if (*steps)[i].Index == step.Index {
			(*steps)[i] = step
			return
		}
	}
	*steps = append(*steps, step)
	sort.Slice(*steps, func(i, j int) bool { return (*steps)[i].Index < (*steps)[j].Index })
}

// decodeSteps parses a stored RunbookStepProgress[], DROPPING corrupted entries
// (non-object members, or a member with an unrecognized status). Go's slice
// unmarshal aborts on the first bad element and leaves a zero-value placeholder,
// so we decode per-entry and keep only well-formed steps — tolerating a
// corrupted checkpoint blob.
func decodeSteps(raw string) []domain.RunbookStepProgress {
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	var out []domain.RunbookStepProgress
	for _, e := range entries {
		var s domain.RunbookStepProgress
		if err := json.Unmarshal(e, &s); err != nil {
			continue
		}
		if s.Status != domain.RunbookStepDone && s.Status != domain.RunbookStepSkipped {
			continue
		}
		out = append(out, s)
	}
	return out
}

func runStateView(rec *domain.RunbookRunStateRecord) map[string]any {
	steps := decodeSteps(rec.StepsJson)
	if steps == nil {
		steps = []domain.RunbookStepProgress{}
	}
	view := map[string]any{
		"id":          rec.ID,
		"sessionId":   rec.SessionID,
		"runbookId":   rec.RunbookID,
		"currentStep": rec.CurrentStep,
		"steps":       steps,
		"status":      rec.Status,
		"startedAt":   rec.StartedAt,
		"updatedAt":   rec.UpdatedAt,
	}
	if rec.CompletedAt != nil {
		view["completedAt"] = *rec.CompletedAt
	}
	return view
}
