package backend

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// The feature-off wire contract: a TurnContext without workflow state must
// serialize byte-identically to the pre-feature shape (no workflow_state key),
// because the backend validates with extra="forbid" and an unknown field would
// 422 every turn on a backend without the matching contract.
func TestTurnContextWorkflowStateOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(&TurnContext{Goal: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "workflow_state") {
		t.Fatalf("empty workflow state must be omitted from the wire: %s", b)
	}

	withState, err := json.Marshal(&TurnContext{
		Goal:          "g",
		WorkflowState: []WorkflowDigest{{ID: "wfg_1", Goal: "fix", Status: "active"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withState), `"workflow_state"`) {
		t.Fatalf("populated workflow state must serialize: %s", withState)
	}
}

func TestWorkflowDigestClampMirrorsBackendLimits(t *testing.T) {
	long := strings.Repeat("x", 2000)
	d := WorkflowDigest{
		ID: long, Goal: long, Status: long, Progress: long,
		ActiveNodes: []string{long}, Resources: []string{long}, Blockers: []string{long},
		NextAction: long, LastEvent: long,
	}.Clamp()
	checks := []struct {
		name string
		got  int
		max  int
	}{
		{"id", len([]rune(d.ID)), 128},
		{"goal", len([]rune(d.Goal)), 512},
		{"status", len([]rune(d.Status)), 64},
		{"progress", len([]rune(d.Progress)), 512},
		{"next_action", len([]rune(d.NextAction)), 512},
		{"last_event", len([]rune(d.LastEvent)), 512},
		{"active_nodes[0]", len([]rune(d.ActiveNodes[0])), 512},
		{"resources[0]", len([]rune(d.Resources[0])), 512},
		{"blockers[0]", len([]rune(d.Blockers[0])), 512},
	}
	for _, c := range checks {
		if c.got > c.max {
			t.Errorf("%s: %d exceeds backend max %d", c.name, c.got, c.max)
		}
	}
}

// The three workflow task helpers send the right task ids with snake_case
// input fields (the backend pydantic models are strict).
func TestWorkflowTaskWireShapes(t *testing.T) {
	var got TaskRequest
	runner := taskRunnerFunc(func(_ context.Context, req TaskRequest) (TaskResult, error) {
		got = req
		return TaskResult{Output: json.RawMessage(`{"nodes": []}`)}, nil
	})

	_, _ = RunWorkflowPlan(context.Background(), runner, WorkflowPlanInput{
		Goal: "g", ActiveSkillIDs: []string{"s1"},
		ToolInventory: []WorkflowToolInfo{{Name: "fs.read", Risk: "read"}},
	})
	if got.Task != TaskWorkflowPlan {
		t.Fatalf("want %s, got %s", TaskWorkflowPlan, got.Task)
	}
	if _, ok := got.Input["active_skill_ids"]; !ok {
		t.Fatalf("plan input must use snake_case keys, got %v", got.Input)
	}

	_, _ = RunWorkflowReconcile(context.Background(), runner, WorkflowReconcileInput{
		Workflow: map[string]any{"id": "wfg_1"}, Reason: "wake",
	})
	if got.Task != TaskWorkflowReconcile || got.Input["reason"] != "wake" {
		t.Fatalf("reconcile wire mismatch: %s %v", got.Task, got.Input)
	}

	_, _ = RunWorkflowResumeDigest(context.Background(), runner, WorkflowResumeDigestInput{
		UserMessage: "where were we?",
	})
	if got.Task != TaskWorkflowResumeDigest || got.Input["user_message"] != "where were we?" {
		t.Fatalf("resume wire mismatch: %s %v", got.Task, got.Input)
	}
}

// taskRunnerFunc adapts a func to TaskRunner.
type taskRunnerFunc func(ctx context.Context, req TaskRequest) (TaskResult, error)

func (f taskRunnerFunc) RunTask(ctx context.Context, req TaskRequest) (TaskResult, error) {
	return f(ctx, req)
}
