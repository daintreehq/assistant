// Package skill holds the skill runbook tools: skill.find, skill.load,
// skill.run.get, skill.step.advance. Skills are runbooks the model pulls on
// demand (skill.find → small-model selector → inject). The run-state tools track
// stepwise progress keyed to the live session so a multi-step runbook can resume.
//
// The core ToolContext does not expose skillSource/loadSkills/findSkills, so this
// family takes them through Deps (consumer-defined interfaces) rather than
// reaching into ToolContext.
package skill

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

const (
	codeInvalidArgs       = "INVALID_ARGS"
	codeNoSession         = "SKILL_RUN_NO_SESSION"
	codeFindUnavailable   = "SKILL_FIND_UNAVAILABLE"
	codeFindFailed        = "SKILL_FIND_FAILED"
	codeSourceUnavailable = "SKILL_SOURCE_UNAVAILABLE"
	codeNotFound          = "SKILL_NOT_FOUND"
	codeLoadUnavailable   = "SKILL_LOAD_UNAVAILABLE"
)

// SkillInfo is a read-only library view of a skill.
type SkillInfo struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
}

// SkillFindResult is the NL→skills resolution. matched=false is a normal "no
// match" (not an error); ok=false is a genuine failure.
type SkillFindResult struct {
	OK             bool        `json:"ok"`
	Matched        bool        `json:"matched"`
	Selected       []SkillInfo `json:"selected"`
	Reason         string      `json:"reason,omitempty"`
	ActiveSkillIDs []string    `json:"activeSkillIds"`
}

// SkillSource is the read-only skill library (skill.load reads it). nil ⇒
// SKILL_SOURCE_UNAVAILABLE.
type SkillSource interface {
	Get(id string) (*SkillInfo, bool)
}

// SkillStore is the slice of storage the run-state tools touch. Natural key is
// (sessionId, skillId).
type SkillStore interface {
	GetSkillRunState(ctx context.Context, sessionID, skillID string) (*domain.SkillRunStateRecord, error)
	InsertSkillRunState(ctx context.Context, rec domain.SkillRunStateRecord) (string, error)
	UpdateSkillRunState(ctx context.Context, rec domain.SkillRunStateRecord) error
}

// Deps is the dependency set for the skill family. Any of these may be nil; the
// corresponding tool then returns its specific "unavailable" code (so a stripped
// test/non-main context fails gracefully rather than panicking).
type Deps struct {
	Store SkillStore
	// Source is the read-only skill library for skill.load.
	Source SkillSource
	// LoadSkills merges skill ids mid-turn (main actor only). Returns the new
	// active skill id set. nil ⇒ SKILL_LOAD_UNAVAILABLE.
	LoadSkills func(ids []string) []string
	// FindSkills resolves NL → skills (main actor only). nil ⇒ SKILL_FIND_UNAVAILABLE.
	FindSkills func(ctx context.Context, query string) (SkillFindResult, error)
	// CheckConsistency optionally runs a small-tier judge over a skill.step.advance
	// transition and returns its verdict — surfacing semantically-wrong Director
	// decisions (a bad jump, a regression, a premature finish) that a clean ok=true
	// tool call would otherwise hide (issue #240). nil ⇒ the judge is skipped (tests,
	// contexts without model access) at zero cost; the free state-delta log still
	// fires. The call itself is also gated on debug logging by observeStepAdvance, so
	// a wired checker never runs in a normal (non-debug) session.
	CheckConsistency func(ctx context.Context, in ConsistencyCheckInput) (domain.ModelJudgeAnswer, error)
}

// ConsistencyCheckInput is the before/after snapshot of one skill.step.advance handed
// to a small-tier consistency judge. It carries ONLY state-transition shape (step
// indices, statuses, the progress arrays) — never the runbook body — so the check
// stays cheap, and uses plain domain values so the skill package needs no model
// imports.
type ConsistencyCheckInput struct {
	SkillID         string
	RunID           string
	SessionID       string
	CompletedStep   int
	NextStep        *int // nil ⇒ this advance finished the run
	StepStatus      domain.SkillStepStatus
	Notes           string
	PrevCurrentStep int
	NextCurrentStep int
	PrevRunStatus   domain.SkillRunStatus
	NextRunStatus   domain.SkillRunStatus
	BeforeSteps     []domain.SkillStepProgress
	AfterSteps      []domain.SkillStepProgress
}

// Tools returns the skill tool family.
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newFindTool(deps),
		newLoadTool(deps),
		newRunGetTool(deps),
		newStepAdvanceTool(deps),
	}
}

// --- skill.find ---

type findArgs struct {
	Query string `json:"query"`
}

var findSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["query"],
  "properties": { "query": { "type": "string", "minLength": 1 } }
}`)

func newFindTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "skill.find",
		Description: "Find and inject the most relevant skill runbook(s) for a natural-language query.",
		Risk:        domain.RiskRead,
		Schema:      findSchema,
		Decode:      tools.StrictDecoder(func() any { return &findArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a findArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for skill.find: "+err.Error())
			}
			query := strings.TrimSpace(a.Query)
			if query == "" {
				return tools.Fail(codeInvalidArgs, "skill.find: query is required")
			}
			if deps.FindSkills == nil {
				return tools.Fail(codeFindUnavailable, "skill.find is not available in this context.", tools.Unrecoverable())
			}
			result, err := deps.FindSkills(ctx, query)
			if err != nil {
				return tools.Fail(codeFindFailed, "skill.find failed: "+err.Error())
			}
			if !result.OK {
				reason := result.Reason
				if reason == "" {
					reason = "skill selection failed"
				}
				return tools.Fail(codeFindFailed, "skill.find failed: "+reason)
			}
			active := result.ActiveSkillIDs
			if active == nil {
				active = []string{}
			}
			if !result.Matched {
				return tools.Ok("No skill matched the query.", map[string]any{
					"query":          query,
					"selected":       []any{},
					"reason":         result.Reason,
					"activeSkillIds": active,
				})
			}
			return tools.Ok(loadedSkillsSummary(result.Selected), map[string]any{
				"query":          query,
				"selected":       result.Selected,
				"reason":         result.Reason,
				"activeSkillIds": active,
			})
		},
	}
}

// loadedSkillsSummary renders the result gist for a successful skill.find: the
// title(s) of the skill(s) just loaded, comma-joined. The cockpit shows this as the
// "Loading skill" row's detail (internal/ui/render_activity.go), and the model reads
// it as the tool-result summary — so it names what was actually pulled rather than a
// bare count. Callers only reach here on a match, so the slice is non-empty.
func loadedSkillsSummary(sel []SkillInfo) string {
	titles := make([]string, 0, len(sel))
	for _, s := range sel {
		titles = append(titles, skillTitle(s.Title, s.ID))
	}
	return strings.Join(titles, ", ")
}

// skillTitle is the title, falling back to the id when a skill has no title (so a
// row/summary is never blank).
func skillTitle(title, id string) string {
	if t := strings.TrimSpace(title); t != "" {
		return t
	}
	return id
}

// --- skill.load ---

type loadArgs struct {
	SkillID string `json:"skillId"`
}

var loadSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["skillId"],
  "properties": { "skillId": { "type": "string", "minLength": 1 } }
}`)

func newLoadTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "skill.load",
		Description: "Load a specific skill runbook by id into the active set.",
		Risk:        domain.RiskRead,
		Schema:      loadSchema,
		Decode:      tools.StrictDecoder(func() any { return &loadArgs{} }),
		Handle: func(_ context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a loadArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for skill.load: "+err.Error())
			}
			id := strings.TrimSpace(a.SkillID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "skill.load: skillId is required")
			}
			if deps.Source == nil {
				return tools.Fail(codeSourceUnavailable, "skill.load is not available in this context.", tools.Unrecoverable())
			}
			info, ok := deps.Source.Get(id)
			if !ok || info == nil {
				return tools.Fail(codeNotFound, "skill.load: no such skill: "+id)
			}
			if deps.LoadSkills == nil {
				return tools.Fail(codeLoadUnavailable, "skill.load cannot inject skills in this context.", tools.Unrecoverable())
			}
			active := deps.LoadSkills([]string{id})
			if active == nil {
				active = []string{}
			}
			return tools.Ok(skillTitle(info.Title, info.ID), map[string]any{
				"id":             info.ID,
				"title":          info.Title,
				"summary":        info.Summary,
				"activeSkillIds": active,
			})
		},
	}
}

// --- skill.run.get ---

type runGetArgs struct {
	SkillID string `json:"skillId"`
}

var runGetSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["skillId"],
  "properties": { "skillId": { "type": "string", "minLength": 1 } }
}`)

func newRunGetTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "skill.run.get",
		Description: "Get the stepwise progress state of a skill run in this session (absence is a normal OK answer).",
		Risk:        domain.RiskRead,
		Schema:      runGetSchema,
		Decode:      tools.StrictDecoder(func() any { return &runGetArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a runGetArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for skill.run.get: "+err.Error())
			}
			id := strings.TrimSpace(a.SkillID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "skill.run.get: skillId is required")
			}
			if tctx.SessionID == "" {
				return tools.Fail(codeNoSession, "skill.run.get requires a live session.", tools.Unrecoverable())
			}
			if deps.Store == nil {
				return tools.Ok("No skill run state.", map[string]any{"state": nil})
			}
			rec, err := deps.Store.GetSkillRunState(ctx, tctx.SessionID, id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "skill.run.get: "+err.Error())
			}
			if rec == nil {
				return tools.Ok("No skill run state.", map[string]any{"state": nil})
			}
			return tools.Ok("Skill run state for "+id+".", map[string]any{"state": runStateView(rec)})
		},
	}
}

// --- skill.step.advance ---

type stepAdvanceArgs struct {
	SkillID       string `json:"skillId"`
	CompletedStep int    `json:"completedStep"`
	NextStep      *int   `json:"nextStep,omitempty"`
	Status        string `json:"status,omitempty"` // done | skipped (default done)
	Notes         string `json:"notes,omitempty"`
}

var stepAdvanceSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["skillId", "completedStep"],
  "properties": {
    "skillId": { "type": "string", "minLength": 1 },
    "completedStep": { "type": "integer", "minimum": 1 },
    "nextStep": { "type": "integer", "minimum": 1 },
    "status": { "type": "string", "enum": ["done", "skipped"] },
    "notes": { "type": "string" }
  }
}`)

func newStepAdvanceTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name:        "skill.step.advance",
		Description: "Record that a skill step is complete (or skipped) and advance to the next step. Omitting nextStep finishes the run.",
		Risk:        domain.RiskLocal,
		Schema:      stepAdvanceSchema,
		Decode:      tools.StrictDecoder(func() any { return &stepAdvanceArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, tctx *tools.ToolContext) tools.ToolResult {
			var a stepAdvanceArgs
			if err := tools.DecodeStrict(args, &a); err != nil {
				return tools.Fail(codeInvalidArgs, "Invalid arguments for skill.step.advance: "+err.Error())
			}
			id := strings.TrimSpace(a.SkillID)
			if id == "" {
				return tools.Fail(codeInvalidArgs, "skill.step.advance: skillId is required")
			}
			if a.CompletedStep < 1 {
				return tools.Fail(codeInvalidArgs, "skill.step.advance: completedStep must be >= 1")
			}
			if a.NextStep != nil && *a.NextStep < 1 {
				return tools.Fail(codeInvalidArgs, "skill.step.advance: nextStep must be >= 1")
			}
			stepStatus := domain.SkillStepDone
			switch a.Status {
			case "", "done":
				stepStatus = domain.SkillStepDone
			case "skipped":
				stepStatus = domain.SkillStepSkipped
			default:
				return tools.Fail(codeInvalidArgs, "skill.step.advance: status must be done|skipped")
			}
			if tctx.SessionID == "" {
				return tools.Fail(codeNoSession, "skill.step.advance requires a live session.", tools.Unrecoverable())
			}
			if deps.Store == nil {
				return tools.Fail(domain.CodeInternal, "skill.step.advance: storage unavailable")
			}

			now := domain.NowMS()
			existing, err := deps.Store.GetSkillRunState(ctx, tctx.SessionID, id)
			if err != nil {
				return tools.Fail(domain.CodeInternal, "skill.step.advance: "+err.Error())
			}

			finished := a.NextStep == nil

			var steps []domain.SkillStepProgress
			var prevCurrent int
			var prevRunStatus domain.SkillRunStatus
			if existing != nil {
				steps = decodeSteps(existing.StepsJson)
				prevCurrent = existing.CurrentStep
				prevRunStatus = existing.Status
			}
			// issue #240: snapshot the pre-advance progress for the decision-correctness
			// side-channel BEFORE upsertStep mutates `steps` in place. Only clone when
			// debug logging is on — the snapshot is unused otherwise (zero normal-run cost).
			var beforeSteps []domain.SkillStepProgress
			if tctx.Config.DebugLog {
				beforeSteps = append([]domain.SkillStepProgress(nil), steps...)
			}
			// Upsert the completed step into the sorted array.
			var notesPtr *string
			if a.Notes != "" {
				n := a.Notes
				notesPtr = &n
			}
			upsertStep(&steps, domain.SkillStepProgress{
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
			runStatus := domain.SkillRunActive
			if finished {
				runStatus = domain.SkillRunCompleted
			}

			// issue #240 decision-correctness side-channel input, populated once; it is
			// emitted only AFTER a successful persist in each branch below (a failed DB
			// write must not log an "advance occurred" event). `steps` is the post-upsert
			// (after) state; beforeSteps holds the pre-upsert snapshot.
			observeIn := ConsistencyCheckInput{
				SkillID:         id,
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
				rec := domain.SkillRunStateRecord{
					ID:          domain.NewID(domain.PrefixSkillRun),
					SessionID:   tctx.SessionID,
					SkillID:     id,
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
				if _, err := deps.Store.InsertSkillRunState(ctx, rec); err != nil {
					return tools.Fail(domain.CodeInternal, "skill.step.advance: "+err.Error())
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
			if err := deps.Store.UpdateSkillRunState(ctx, *existing); err != nil {
				return tools.Fail(domain.CodeInternal, "skill.step.advance: "+err.Error())
			}
			observeStepAdvance(ctx, deps, tctx, observeIn)
			return tools.Ok(advanceSummary(id, finished, currentStep), map[string]any{"state": runStateView(existing)})
		},
	}
}

// observeStepAdvance emits the issue #240 decision-correctness side-channels for a
// SUCCESSFUL skill.step.advance: a free before/after state-delta line, and — when a
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
	debuglog.LogDebug(cfg, "skill.step.delta", map[string]any{
		"runId":           in.RunID,
		"sessionId":       in.SessionID,
		"skillId":         in.SkillID,
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

	if deps.CheckConsistency == nil {
		return
	}
	ans, err := deps.CheckConsistency(ctx, in)
	if err != nil {
		debuglog.LogDebug(cfg, "skill.step.consistency", map[string]any{
			"runId":         in.RunID,
			"skillId":       in.SkillID,
			"completedStep": in.CompletedStep,
			"checkOk":       false,
			"error":         err.Error(),
		})
		return
	}
	debuglog.LogDebug(cfg, "skill.step.consistency", map[string]any{
		"runId":         in.RunID,
		"skillId":       in.SkillID,
		"completedStep": in.CompletedStep,
		"checkOk":       true,
		"flagged":       ans.Matched,
		"confidence":    ans.Confidence,
		"reason":        ans.Reason,
	})
}

func advanceSummary(id string, finished bool, currentStep int) string {
	if finished {
		return "Completed skill run for " + id + "."
	}
	return fmt.Sprintf("Advanced skill %s to step %d.", id, currentStep)
}

// upsertStep inserts or replaces the step with the same Index, keeping the slice
// sorted ascending by Index.
func upsertStep(steps *[]domain.SkillStepProgress, step domain.SkillStepProgress) {
	for i := range *steps {
		if (*steps)[i].Index == step.Index {
			(*steps)[i] = step
			return
		}
	}
	*steps = append(*steps, step)
	sort.Slice(*steps, func(i, j int) bool { return (*steps)[i].Index < (*steps)[j].Index })
}

// decodeSteps parses a stored SkillStepProgress[], DROPPING corrupted entries
// (non-object members, or a member with an unrecognized status). Go's slice
// unmarshal aborts on the first bad element and leaves a zero-value placeholder,
// so we decode per-entry and keep only well-formed steps — tolerating a
// corrupted checkpoint blob.
func decodeSteps(raw string) []domain.SkillStepProgress {
	var entries []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		return nil
	}
	var out []domain.SkillStepProgress
	for _, e := range entries {
		var s domain.SkillStepProgress
		if err := json.Unmarshal(e, &s); err != nil {
			continue
		}
		if s.Status != domain.SkillStepDone && s.Status != domain.SkillStepSkipped {
			continue
		}
		out = append(out, s)
	}
	return out
}

func runStateView(rec *domain.SkillRunStateRecord) map[string]any {
	steps := decodeSteps(rec.StepsJson)
	if steps == nil {
		steps = []domain.SkillStepProgress{}
	}
	view := map[string]any{
		"id":          rec.ID,
		"sessionId":   rec.SessionID,
		"skillId":     rec.SkillID,
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
