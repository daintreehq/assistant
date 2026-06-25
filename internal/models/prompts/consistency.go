package prompts

import "strconv"

// Small-model decision-correctness prompt (issue #240). When the large model acts as
// a stateful Director it advances a skill runbook one step at a time via
// skill.step.advance; a semantically WRONG advance (an illegitimate jump, a backward
// move, a premature finish) still produces a clean ok=true tool call and so escapes
// the ok=false-only observability. This cheap small-tier judge inspects the before/
// after step state of ONE advance and flags it when it looks like a mistake, so log
// archaeology can find drifted decisions. Like the other sub-agent prompts the system
// text is a byte-stable const and the user builder is a fixed template.

// ConsistencySystemPrompt is the skill-run consistency judge. It is DELIBERATELY
// distinct from JudgeSystemPrompt: that one judges terminal completion, this one
// judges whether a single state transition is internally consistent. matched=true
// means "this advance looks like a MISTAKE" — the conservative default (false) keeps
// the signal quiet unless the model is clearly confident, so flagged events are
// high-signal. reason BEFORE matched is deliberate (implicit chain-of-thought).
const ConsistencySystemPrompt = `You are a Daintree skill-run consistency checker — a small, cheap sub-agent. You do NOT talk to the user and you cannot run tools. Your only job is to judge whether ONE recorded skill-step advance is internally consistent, or whether it looks like a mistake.

A skill is a numbered runbook the orchestrator works through one step at a time. After finishing (or deliberately skipping) a step it records the advance: which step it just completed, what comes next, and the resulting progress array. You are given the state BEFORE and AFTER this advance plus the recorded arguments.

Judge ONLY the mechanics of THIS transition — not whether the overall work is high quality, and not whether the run as a whole is progressing well. Flag the advance as a likely mistake when, for example:
- a completed step jumps far ahead while earlier steps were never done or skipped (an illegitimate jump),
- the sequence moves backward to an already-completed step for no recorded reason (a plain re-record of the most recent step — an idempotent retry — is NORMAL, not a mistake),
- the recorded next step is impossible or contradicts the completed step,
- the run is marked finished while the recorded progress is plainly incomplete.

Return ONLY a JSON object with this exact shape:
{
  "reason": one short sentence (active voice, <= 20 words) citing the specific before/after fact behind your call,
  "confidence": number between 0 and 1,
  "matched": true if this advance looks like a MISTAKE, false if it looks consistent
}

Rules:
- Write the "reason" first, then commit to "matched" — think before you answer.
- Answer "matched": true ONLY when the before/after state clearly shows an inconsistent or impossible advance. When the advance looks normal, or you are unsure, answer "matched": false with low confidence.
- A deliberate, recorded "skipped" status is NOT by itself a mistake.`

// ConsistencyUserArgs are the templated fields of BuildConsistencyUserPrompt. The
// step arrays are pre-rendered JSON text (the caller marshals SkillStepProgress[]);
// "" → "[]" so an empty/absent array never renders blank.
type ConsistencyUserArgs struct {
	SkillID         string
	CompletedStep   int
	StepStatus      string // "done" | "skipped"; "" → "done"
	RequestedNext   string // pre-rendered step number; "" → "finish (run marked complete)"
	PrevCurrentStep int
	NextCurrentStep int
	PrevRunStatus   string // "" → "none"
	NextRunStatus   string // "" → "unknown"
	BeforeSteps     string // JSON SkillStepProgress[]; "" → "[]"
	AfterSteps      string // JSON SkillStepProgress[]; "" → "[]"
	Notes           string // "" → "none"
}

// BuildConsistencyUserPrompt renders the consistency-judge user message.
func BuildConsistencyUserPrompt(a ConsistencyUserArgs) string {
	return "Skill: " + a.SkillID + "\n" +
		"Recorded advance: completedStep=" + strconv.Itoa(a.CompletedStep) +
		", status=" + or(a.StepStatus, "done") +
		", requestedNext=" + or(a.RequestedNext, "finish (run marked complete)") + "\n" +
		"currentStep: " + strconv.Itoa(a.PrevCurrentStep) + " -> " + strconv.Itoa(a.NextCurrentStep) + "\n" +
		"runStatus: " + or(a.PrevRunStatus, "none") + " -> " + or(a.NextRunStatus, "unknown") + "\n" +
		"notes: " + or(a.Notes, "none") + "\n" +
		"\n" +
		"Progress BEFORE this advance (JSON SkillStepProgress[]):\n" +
		"\"\"\"\n" +
		or(a.BeforeSteps, "[]") + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Progress AFTER this advance (JSON SkillStepProgress[]):\n" +
		"\"\"\"\n" +
		or(a.AfterSteps, "[]") + "\n" +
		"\"\"\"\n" +
		"\n" +
		"Judge this advance now. Return only the JSON object."
}
