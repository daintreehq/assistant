package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// countingTools counts OpenAITools calls so the projection cache can be asserted:
// a stable offered toolset must project ONCE, not once per loop iteration / turn.
type countingTools struct {
	*fakeTools
	projectCalls int
}

func (c *countingTools) OpenAITools(filter []string) ([]models.ChatTool, error) {
	c.projectCalls++
	out := make([]models.ChatTool, 0, len(filter))
	for _, n := range filter {
		out = append(out, models.ChatTool{Function: models.ChatToolFunc{Name: strings.ReplaceAll(n, ".", "__")}})
	}
	return out, nil
}

// TestToolProjectionCachedAcrossIterations: three iterations of one turn with no
// skill mutation ⇒ the projection is built once and reused for every iteration.
func TestToolProjectionCachedAcrossIterations(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__read", `{}`)}},
		{ToolCalls: []models.ToolCallRequest{toolCall("c2", "fs__read", `{}`)}},
		{Content: "done"},
	}}
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, realRegistry(t), r, tr)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tr.projectCalls != 1 {
		t.Fatalf("OpenAITools should be projected once across 3 iterations, got %d", tr.projectCalls)
	}
}

// TestToolProjectionReusedAcrossTurns: two turns with an unchanged skill set reuse
// the same cached projection — the cache survives across turns.
func TestToolProjectionReusedAcrossTurns(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{Content: "first"},  // turn 1: immediate answer
		{Content: "second"}, // turn 2: immediate answer
	}}
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, realRegistry(t), r, tr)

	if _, err := s.Send(context.Background(), "one", SendOptions{}); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if _, err := s.Send(context.Background(), "two", SendOptions{}); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if tr.projectCalls != 1 {
		t.Fatalf("projection should be reused across turns (1 build), got %d", tr.projectCalls)
	}
}

// TestToolProjectionReadOnlyKeyDistinct: a read-only wake turn and a normal turn
// have distinct projection keys (the read-only flag is part of the cache identity),
// so each builds its own projection.
func TestToolProjectionReadOnlyKeyDistinct(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{Content: "ro"}, // turn 1: read-only wake, immediate answer
		{Content: "rw"}, // turn 2: normal, immediate answer
	}}
	tr := &countingTools{fakeTools: &fakeTools{
		result:   domain.Ok("ok", nil),
		readOnly: []string{"fs.read", "fs.list"},
	}}
	s := skillSession(t, realRegistry(t), r, tr)

	if _, err := s.Send(context.Background(), "wake", SendOptions{ReadOnly: true}); err != nil {
		t.Fatalf("Send RO: %v", err)
	}
	if _, err := s.Send(context.Background(), "normal", SendOptions{}); err != nil {
		t.Fatalf("Send RW: %v", err)
	}
	if tr.projectCalls != 2 {
		t.Fatalf("read-only and normal turns have distinct keys ⇒ 2 builds, got %d", tr.projectCalls)
	}
}

// TestToolProjectionInvalidatedOnSkillLoadThenReused: a mid-turn skill.load rewrites
// the active set (round 0 full → invalidate → round 1 narrowed), and the unchanged
// narrowed set in round 2 is a cache hit. Exactly two projections: full, then the
// narrowed set reused. midturnLoader records one entry per OpenAITools call, so
// len(projects) IS the build count.
func TestToolProjectionInvalidatedOnSkillLoadThenReused(t *testing.T) {
	reg := realRegistry(t)
	loadCall := toolCall("c1", "skill__load", `{"skillIds":["`+skills.IDSpawnAgentForEdits+`"]}`)
	readCall := toolCall("c2", "fs__read", `{}`)
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{loadCall}}, // round 0: skill.load (mutates the set)
		{ToolCalls: []models.ToolCallRequest{readCall}}, // round 1: narrowed-set build
		{Content: "done"}, // round 2: same narrowed key ⇒ cache hit
	}}
	loader := &midturnLoader{
		fakeTools: &fakeTools{result: domain.Ok("ok", nil)},
		loadID:    skills.IDSpawnAgentForEdits,
	}
	s := skillSession(t, reg, r, loader)
	loader.session = s

	if _, err := s.Send(context.Background(), "set up an edit agent", SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(loader.projects) != 2 {
		t.Fatalf("expected exactly 2 projections (full, then narrowed-reused), got %d: %v",
			len(loader.projects), loader.projects)
	}
	// Round 0 is the full registry (nil filter); round 1 is the narrowed skill-aware
	// set (non-empty) — the invalidation actually rebuilt rather than serving stale.
	if len(loader.projects[0]) != 0 {
		t.Fatalf("round 0 should be the full registry (nil filter), got %v", loader.projects[0])
	}
	if len(loader.projects[1]) == 0 {
		t.Fatal("round 1 should be the rebuilt narrowed set, not empty")
	}
}
