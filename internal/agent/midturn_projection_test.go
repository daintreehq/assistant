package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// midturnLoader is a ToolRunner that records EVERY per-iteration tool projection
// and, the first time it dispatches skill.load, mutates the live session by
// loading a skill — mimicking the real skill.load tool's effect on the active set
// mid-turn. We then assert that loading a skill mid-turn does NOT narrow the offered
// toolset: every iteration still projects the FULL registry (nil filter), because
// skills are guidance and must never limit which tools the model can call.
type midturnLoader struct {
	*fakeTools
	session  *Session
	loadID   string
	projects [][]string // one entry per OpenAITools call, in order
	loaded   bool
}

func (m *midturnLoader) OpenAITools(filter []string) ([]models.ChatTool, error) {
	m.projects = append(m.projects, append([]string(nil), filter...))
	out := make([]models.ChatTool, 0, len(filter))
	for _, n := range filter {
		out = append(out, models.ChatTool{Function: models.ChatToolFunc{Name: strings.ReplaceAll(n, ".", "__")}})
	}
	return out, nil
}

func (m *midturnLoader) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	if name == "skill.load" && !m.loaded {
		m.loaded = true
		m.session.LoadAdditionalSkills([]string{m.loadID})
	}
	return domain.Ok("ok", nil)
}

func TestMidTurnSkillLoadDoesNotNarrowToolset(t *testing.T) {
	reg := realRegistry(t)

	// Round 0: the model calls skill.load. Round 1: it answers (no tool calls).
	loadCall := models.ToolCallRequest{
		ID:       "call_1",
		Type:     "function",
		Function: models.ToolCallFunction{Name: "skill__load", Arguments: `{"skillIds":["` + skills.IDSpawnAgentForEdits + `"]}`},
	}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{loadCall}}, // round 0 → tool call
		{Content: "all done"},                           // round 1 → final answer
	}}

	loader := &midturnLoader{
		fakeTools: &fakeTools{result: domain.Ok("ok", nil)},
		loadID:    skills.IDSpawnAgentForEdits,
	}
	s := skillSession(t, reg, r, loader)
	loader.session = s

	// Start with NO skill loaded — round 0's projection is the full registry (nil filter).
	if _, err := s.Send(context.Background(), "please set up an edit agent", SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if len(loader.projects) < 2 {
		t.Fatalf("expected at least 2 per-iteration projections, got %d", len(loader.projects))
	}
	// Round 0: no skill loaded yet ⇒ nil/empty filter (full registry).
	if len(loader.projects[0]) != 0 {
		t.Fatalf("round 0 should project the full registry (nil filter), got %v", loader.projects[0])
	}
	// Round 1: a mid-turn skill.load must STILL project the full registry (nil filter)
	// — skills never narrow the toolset, so loading one mid-turn cannot shrink what the
	// model can call.
	if len(loader.projects[1]) != 0 {
		t.Fatalf("a mid-turn skill load must NOT narrow the toolset; round 1 should still be the full registry (nil filter), got %v", loader.projects[1])
	}
}
