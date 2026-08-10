package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
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

// TestToolProjectionCachedAcrossIterations: three iterations of one turn ⇒ the projection
// is built once and reused for every iteration (the offered toolset never changes —
// skills no longer narrow it, so the projection key is stable).
func TestToolProjectionCachedAcrossIterations(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__read", `{}`)}},
		{ToolCalls: []models.ToolCallRequest{toolCall("c2", "fs__read", `{}`)}},
		{Content: "done"},
	}}
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, r, tr)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if tr.projectCalls != 1 {
		t.Fatalf("OpenAITools should be projected once across 3 iterations, got %d", tr.projectCalls)
	}
}

// TestToolProjectionReusedAcrossTurns: two turns with an unchanged toolset reuse the same
// cached projection — the cache survives across turns.
func TestToolProjectionReusedAcrossTurns(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{
		{Content: "first"},  // turn 1: immediate answer
		{Content: "second"}, // turn 2: immediate answer
	}}
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, r, tr)

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

// TestProjectToolsNilVsEmptyDistinctIdentity: a nil filter (full registry) and a non-nil
// empty filter (no tools) are DISTINCT cache identities — the unconstrained sentinel must
// not let slices.Equal(nil, []string{}) collapse them. Exercises projectToolsLocked
// directly under the lock it requires.
func TestProjectToolsNilVsEmptyDistinctIdentity(t *testing.T) {
	tr := &countingTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}}
	s := skillSession(t, plainRouter(), tr)

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

func TestBackendToolProjectionReused(t *testing.T) {
	tools := []models.ChatTool{{Function: models.ChatToolFunc{Name: "fs__read"}}}
	s := &Session{toolProj: toolProjCache{valid: true, unconstrained: true, tools: tools}}
	first, err := s.toBackendToolsCached(tools)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.toBackendToolsCached(tools)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || len(second) != 1 || &first[0] != &second[0] {
		t.Fatal("stable tool inventory should reuse the cached backend projection")
	}
}
