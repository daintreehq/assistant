package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
)

// These tests pin the post-migration session-history + tool-projection contract.
// Skills are SERVER-OWNED now (the backend's selector picks and injects runbooks), so
// the Session no longer holds a skill catalog and never narrows the toolset — every
// turn offers the full registry.

// captureStreamTools records the projected tool wire-names from a stream so the
// per-turn projection can be asserted (skills never narrow the toolset, so this should
// always be the full registry — a nil filter).
type captureStreamTools struct {
	*fakeTools
	full []models.ChatTool // returned by OpenAITools for a nil filter
	last []string          // internal names of the last OpenAITools projection
}

func (c *captureStreamTools) OpenAITools(filter []string) ([]models.ChatTool, error) {
	if filter == nil {
		c.last = nil
		out := make([]models.ChatTool, len(c.full))
		copy(out, c.full)
		return out, nil
	}
	c.last = append([]string{}, filter...)
	out := make([]models.ChatTool, 0, len(filter))
	for _, n := range filter {
		out = append(out, models.ChatTool{Function: models.ChatToolFunc{Name: strings.ReplaceAll(n, ".", "__")}})
	}
	return out, nil
}

// skillSession builds a session wired through the backend-from-router adapter. The
// Session no longer consumes a skill catalog — the backend owns skill selection.
func skillSession(t *testing.T, r Router, tr ToolRunner) *Session {
	t.Helper()
	deps := SessionDeps{
		Backend:   backendFromRouter{r: r},
		Router:    r,
		Tools:     tr,
		SessionID: "ses_skills",
		Events:    NoopEventSink{},
		PromptContext: prompts.MainPromptContext{
			Tier: domain.TierOperator, ProjectPath: "/proj",
			MCPConnected: true, MCPStatusLine: "connected",
			SchedulerActive: true,
		},
	}
	return NewSession(deps)
}

func plainRouter() *fakeRouter {
	return &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
}

// --- fresh session / history shape ---

// TestFreshSessionStartsEmpty pins the new reality: the CLI holds no client-side control
// prefix (the backend owns the system prompt + skills), so a fresh session begins with an
// EMPTY visible history. The first turn appends the user message at index 0.
func TestFreshSessionStartsEmpty(t *testing.T) {
	s := skillSession(t, plainRouter(), &fakeTools{})
	if got := len(s.Messages()); got != 0 {
		t.Fatalf("fresh session messages = %d want 0 (no client-side control prefix)", got)
	}
}

// TestSendAppendsUserThenAssistant proves the first turn's history begins at index 0 with
// the user message, then the assistant reply — no leading system/control rows.
func TestSendAppendsUserThenAssistant(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "hi"}}}
	s := skillSession(t, r, &fakeTools{})
	if _, err := s.Send(context.Background(), "hello there", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	msgs := s.Messages()
	if len(msgs) < 2 {
		t.Fatalf("messages = %d want >= 2 (user + assistant)", len(msgs))
	}
	if msgs[0].Role != "user" || msgs[0].StringContent != "hello there" {
		t.Fatalf("msg[0] = %+v want user/hello there", msgs[0])
	}
	if msgs[1].Role != "assistant" {
		t.Fatalf("msg[1] role = %q want assistant", msgs[1].Role)
	}
}

// --- tool projection (always the full registry) ---

func TestSendFullRegistryWhenNoSkillActive(t *testing.T) {
	full := []models.ChatTool{
		{Function: models.ChatToolFunc{Name: "fs__read"}},
		{Function: models.ChatToolFunc{Name: "timer__schedule"}},
	}
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}, full: full}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, r, tools)
	if _, err := s.Send(context.Background(), "simple question", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// nil filter ⇒ the full registry is offered (last==nil).
	if tools.last != nil {
		t.Fatalf("expected a nil (full-registry) filter, got %v", tools.last)
	}
}

func TestSendNeverEmptyToolListUnconstrained(t *testing.T) {
	full := []models.ChatTool{{Function: models.ChatToolFunc{Name: "fs__read"}}}
	tools := &captureStreamTools{fakeTools: &fakeTools{result: domain.Ok("ok", nil)}, full: full}
	r := &fakeRouter{results: []models.ChatResult{{Content: "ok"}}}
	s := skillSession(t, r, tools)
	if _, err := s.Send(context.Background(), "hi", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// An unconstrained turn projects the full registry (nil filter), never an empty slice.
	if tools.last != nil {
		t.Fatalf("unconstrained turn must use the full registry, got filter %v", tools.last)
	}
}
