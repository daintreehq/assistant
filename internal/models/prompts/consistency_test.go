package prompts

import (
	"strings"
	"testing"
)

// The consistency user prompt must render every transition fact the small judge needs.
func TestBuildConsistencyUserPromptRendersFields(t *testing.T) {
	out := BuildConsistencyUserPrompt(ConsistencyUserArgs{
		SkillID:         "daintree.orchestration.basic",
		CompletedStep:   3,
		StepStatus:      "done",
		RequestedNext:   "4",
		PrevCurrentStep: 2,
		NextCurrentStep: 4,
		PrevRunStatus:   "active",
		NextRunStatus:   "active",
		BeforeSteps:     `[{"index":1,"status":"done"}]`,
		AfterSteps:      `[{"index":3,"status":"done"}]`,
		Notes:           "looks fine",
	})
	for _, want := range []string{
		"daintree.orchestration.basic",
		"completedStep=3",
		"status=done",
		"requestedNext=4",
		"currentStep: 2 -> 4",
		"runStatus: active -> active",
		"notes: looks fine",
		`[{"index":1,"status":"done"}]`,
		`[{"index":3,"status":"done"}]`,
		"Return only the JSON object",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, out)
		}
	}
}

// Empty fields fall back to readable placeholders (never blank), so the judge always
// sees a well-formed transition.
func TestBuildConsistencyUserPromptFallbacks(t *testing.T) {
	out := BuildConsistencyUserPrompt(ConsistencyUserArgs{SkillID: "s", CompletedStep: 1})
	for _, want := range []string{
		"status=done", // empty StepStatus → done
		"requestedNext=finish (run marked complete)", // empty RequestedNext → finish
		"runStatus: none -> unknown",                 // empty statuses → none/unknown
		"notes: none",                                // empty notes
		"[]",                                         // empty step arrays
	} {
		if !strings.Contains(out, want) {
			t.Errorf("fallback prompt missing %q\n---\n%s", want, out)
		}
	}
}

// The consistency judge must be framed around skill-step transitions — NOT terminal
// completion — so it cannot drift into the terminal-judge's domain.
func TestConsistencySystemPromptDomain(t *testing.T) {
	sys := ConsistencySystemPrompt
	if !strings.Contains(sys, "skill") {
		t.Error("consistency system prompt should mention skills")
	}
	if strings.Contains(strings.ToLower(sys), "terminal") {
		t.Error("consistency system prompt must not mention terminals")
	}
	if strings.Contains(strings.ToLower(sys), "task completion") {
		t.Error("consistency system prompt must not be about task completion")
	}
	if !strings.Contains(sys, `"matched"`) {
		t.Error("consistency system prompt should specify the matched field")
	}
}
