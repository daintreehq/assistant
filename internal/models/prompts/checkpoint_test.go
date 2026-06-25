package prompts

import "testing"

func TestParseCheckpointValidObject(t *testing.T) {
	raw := `{
  "goal": "ship issue 256",
  "active_terminals": ["term_a1b2 — running tests"],
  "active_watchers": ["wch_12345678 — watching agent"],
  "workflow_run_ids": ["wfr_deadbeef"],
  "decisions": ["use a struct, not prose"],
  "pending_tool_state": ["awaiting terminal.read"],
  "next_actions": ["open the PR"],
  "open_questions": ["what schema?"],
  "approvals_grants": ["grt_cafef00d — git ops"],
  "preserved_ids": ["agt_00112233"]
}`
	cp := ParseCheckpoint(raw)
	if cp.Goal != "ship issue 256" {
		t.Fatalf("goal = %q", cp.Goal)
	}
	if len(cp.ActiveTerminals) != 1 || cp.ActiveTerminals[0] != "term_a1b2 — running tests" {
		t.Fatalf("active_terminals = %+v", cp.ActiveTerminals)
	}
	if len(cp.ApprovalsGrants) != 1 || cp.ApprovalsGrants[0] != "grt_cafef00d — git ops" {
		t.Fatalf("approvals_grants = %+v", cp.ApprovalsGrants)
	}
	if len(cp.WorkflowRunIDs) != 1 || len(cp.OpenQuestions) != 1 || len(cp.PreservedIDs) != 1 {
		t.Fatalf("unexpected list lengths: %+v", cp)
	}
}

func TestParseCheckpointStripsFence(t *testing.T) {
	raw := "```json\n{\"goal\":\"fenced\"}\n```"
	cp := ParseCheckpoint(raw)
	if cp.Goal != "fenced" {
		t.Fatalf("goal = %q, want fenced (fence not stripped)", cp.Goal)
	}
}

func TestParseCheckpointProseReturnsZero(t *testing.T) {
	cp := ParseCheckpoint("Sorry, here is a prose summary of the conversation.")
	if cp.Goal != "" || cp.ActiveTerminals != nil || cp.PreservedIDs != nil {
		t.Fatalf("prose reply must parse to the zero value, got %+v", cp)
	}
}

func TestParseCheckpointEmptyReturnsZero(t *testing.T) {
	if cp := ParseCheckpoint("   "); cp.Goal != "" || cp.ActiveTerminals != nil {
		t.Fatalf("blank reply must parse to the zero value, got %+v", cp)
	}
}

func TestParseCheckpointNormalizesLists(t *testing.T) {
	raw := `{"goal":"  trimmed  ","decisions":[" keep "," keep ","","drop-blank-neighbour"]}`
	cp := ParseCheckpoint(raw)
	if cp.Goal != "trimmed" {
		t.Fatalf("goal not trimmed: %q", cp.Goal)
	}
	// " keep " appears twice (dedup, case-insensitive) and an empty string is dropped.
	if len(cp.Decisions) != 2 || cp.Decisions[0] != "keep" || cp.Decisions[1] != "drop-blank-neighbour" {
		t.Fatalf("decisions not normalized: %+v", cp.Decisions)
	}
}
