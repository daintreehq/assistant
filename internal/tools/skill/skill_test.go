package skill

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memStore struct {
	rec *domain.SkillRunStateRecord
}

func (m *memStore) GetSkillRunState(_ context.Context, _, _ string) (*domain.SkillRunStateRecord, error) {
	return m.rec, nil
}
func (m *memStore) InsertSkillRunState(_ context.Context, rec domain.SkillRunStateRecord) (string, error) {
	m.rec = &rec
	return rec.ID, nil
}
func (m *memStore) UpdateSkillRunState(_ context.Context, rec domain.SkillRunStateRecord) error {
	m.rec = &rec
	return nil
}

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// step.advance without a live session fails SKILL_RUN_NO_SESSION.
func TestStepAdvanceRequiresSession(t *testing.T) {
	tool := find(Tools(Deps{Store: &memStore{}}), "skill.step.advance")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeNoSession {
		t.Fatalf("expected SKILL_RUN_NO_SESSION, got %+v", res)
	}
}

// Omitting nextStep finishes the run and stamps completedAt exactly once.
func TestStepAdvanceFinishStampsCompletedAt(t *testing.T) {
	st := &memStore{}
	tool := find(Tools(Deps{Store: st}), "skill.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":3}`), ctx)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.rec.Status != domain.SkillRunCompleted {
		t.Fatalf("expected completed status, got %s", st.rec.Status)
	}
	if st.rec.CompletedAt == nil {
		t.Fatal("expected completedAt stamped on finish")
	}
	if st.rec.CurrentStep != 3 {
		t.Fatalf("currentStep on finish: got %d want 3", st.rec.CurrentStep)
	}
}

// currentStep never regresses below the prior current step.
func TestStepAdvanceNoRegress(t *testing.T) {
	st := &memStore{rec: &domain.SkillRunStateRecord{
		ID: "rrs_x", SessionID: "sess1", SkillID: "s", CurrentStep: 5, StepsJson: "[]", Status: domain.SkillRunActive,
	}}
	tool := find(Tools(Deps{Store: st}), "skill.step.advance")
	ctx := &tools.ToolContext{SessionID: "sess1"}
	// Advance "back" to nextStep:2 — current must stay at 5.
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"s","completedStep":1,"nextStep":2}`), ctx)
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if st.rec.CurrentStep != 5 {
		t.Fatalf("currentStep regressed: got %d want 5", st.rec.CurrentStep)
	}
	if st.rec.CompletedAt != nil {
		t.Fatal("non-finish advance must not stamp completedAt")
	}
}

// skill.load without a Source returns SKILL_SOURCE_UNAVAILABLE.
func TestSkillLoadNoSource(t *testing.T) {
	tool := find(Tools(Deps{}), "skill.load")
	res := tool.Handle(context.Background(), json.RawMessage(`{"skillId":"x"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeSourceUnavailable {
		t.Fatalf("expected SKILL_SOURCE_UNAVAILABLE, got %+v", res)
	}
}

// skill.find with no matched skill is a normal OK answer, not a failure. Its summary
// must be the exact sentinel the cockpit keys on to keep the plain ✓ (not the ✦).
func TestSkillFindNoMatchIsOK(t *testing.T) {
	deps := Deps{FindSkills: func(context.Context, string) (SkillFindResult, error) {
		return SkillFindResult{OK: true, Matched: false, ActiveSkillIDs: []string{"a"}}, nil
	}}
	tool := find(Tools(deps), "skill.find")
	res := tool.Handle(context.Background(), json.RawMessage(`{"query":"foo"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok for no-match, got %+v", res.Error)
	}
	if res.Summary != "No skill matched the query." {
		t.Fatalf("no-match summary = %q, want the UI sentinel", res.Summary)
	}
}

// A matched skill.find summarizes with the loaded skill's TITLE (not a bare count),
// so the cockpit's "Loading skill" row names what was pulled.
func TestSkillFindSummaryNamesSkill(t *testing.T) {
	deps := Deps{FindSkills: func(context.Context, string) (SkillFindResult, error) {
		return SkillFindResult{
			OK:      true,
			Matched: true,
			Selected: []SkillInfo{
				{ID: "daintree.orchestration.multi-agent", Title: "Orchestrate multiple agents on one problem"},
			},
			ActiveSkillIDs: []string{"daintree.orchestration.multi-agent"},
		}, nil
	}}
	tool := find(Tools(deps), "skill.find")
	res := tool.Handle(context.Background(), json.RawMessage(`{"query":"vote"}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if res.Summary != "Orchestrate multiple agents on one problem" {
		t.Fatalf("matched summary = %q, want the skill title", res.Summary)
	}
}
