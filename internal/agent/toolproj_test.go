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

// TestProjectToolsNilVsEmptyDistinctIdentity: a nil filter (full registry) and a
// non-nil empty filter (no tools) are DISTINCT cache identities — the unconstrained
// sentinel must not let slices.Equal(nil, []string{}) collapse them. Exercises
// projectToolsLocked directly under the lock it requires.
func TestProjectToolsNilVsEmptyDistinctIdentity(t *testing.T) {
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, realRegistry(t), plainRouter(), tr)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.projectToolsLocked(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.projectToolsLocked([]string{}); err != nil {
		t.Fatal(err)
	}
	if tr.projectCalls != 2 {
		t.Fatalf("nil (full registry) and empty (no tools) are distinct identities ⇒ 2 builds, got %d", tr.projectCalls)
	}
	// Re-projecting the same empty key is a cache hit (no third build).
	if _, err := s.projectToolsLocked([]string{}); err != nil {
		t.Fatal(err)
	}
	if tr.projectCalls != 2 {
		t.Fatalf("repeat empty key should hit the cache, got %d builds", tr.projectCalls)
	}
}

// TestConcurrentSlashFindDuringTurnNoRace: a /skills find slash command calls
// FindSkills off the turn goroutine while a multi-iteration turn projects tools.
// Both touch s.toolProj/s.activeSkills; resolveTurnTools and applySkillBundleLocked
// must serialize under s.mu. This is the regression test for the cache-vs-slash-find
// data race — run the suite under -race to exercise it.
func TestConcurrentSlashFindDuringTurnNoRace(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__read", `{}`)}},
		{ToolCalls: []models.ToolCallRequest{toolCall("c2", "fs__read", `{}`)}},
		{ToolCalls: []models.ToolCallRequest{toolCall("c3", "fs__read", `{}`)}},
		{Content: "done"},
	}}
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, realRegistry(t), r, tr)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			// Mirrors handlers_ui.go's /skills find path: FindSkills off the turn goroutine.
			s.FindSkills(context.Background(), "edit a file")
		}
	}()
	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	<-done
}

// TestToolProjectionInvalidatedOnSkillLoadThenReused: a mid-turn skill.load still
// invalidates the projection cache, so round 1 REBUILDS — but because skills never
// narrow the toolset, the rebuilt projection is the SAME full registry (nil filter)
// as round 0. Round 2, with the cache valid and unchanged, is a hit. Exactly two
// OpenAITools builds; both project the full registry. midturnLoader records one entry
// per OpenAITools call, so len(projects) IS the build count.
func TestToolProjectionInvalidatedOnSkillLoadThenReused(t *testing.T) {
	reg := realRegistry(t)
	loadCall := toolCall("c1", "skill__load", `{"skillIds":["`+skills.IDSpawnAgentForEdits+`"]}`)
	readCall := toolCall("c2", "fs__read", `{}`)
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{loadCall}}, // round 0: skill.load (invalidates the cache)
		{ToolCalls: []models.ToolCallRequest{readCall}}, // round 1: rebuild (still full)
		{Content: "done"}, // round 2: unchanged full key ⇒ cache hit
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
		t.Fatalf("expected exactly 2 projections (full, then full-rebuilt), got %d: %v",
			len(loader.projects), loader.projects)
	}
	// Both rounds project the full registry (nil filter): the skill load invalidated and
	// rebuilt the cache, but a skill never narrows, so the rebuilt set is still full.
	if len(loader.projects[0]) != 0 {
		t.Fatalf("round 0 should be the full registry (nil filter), got %v", loader.projects[0])
	}
	if len(loader.projects[1]) != 0 {
		t.Fatalf("round 1 should REBUILD to the full registry (nil filter), not a narrowed set, got %v", loader.projects[1])
	}
}
