package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/prompts"
	"github.com/daintreehq/assistant/internal/tools/worktreepin"
)

func TestMessageWorktreeOverridesWarmCacheAndLaterSelection(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "final"},
		{Content: "next"},
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	pin := worktreepin.New()
	deps.WorktreePin = pin
	deps.CurrentWorktreeFetcher = func(context.Context) *prompts.WorktreeContext {
		return &prompts.WorktreeContext{Present: true, ID: "wt-c", Path: "/c", Branch: "later"}
	}
	s := NewSession(deps)
	s.worktreeSnap = &prompts.WorktreeContext{Present: true, ID: "wt-a", Path: "/a", Branch: "opened-here"}
	s.worktreeFetchedAt = time.Now()
	b := &prompts.WorktreeContext{Present: true, ID: "wt-b", Path: "/b", Branch: "send-here"}
	if _, err := s.Send(context.Background(), "ask five agents for a fact", SendOptions{Worktree: b}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork()
	for i := 0; i < 2; i++ {
		w := be.runtimeAt(i).Worktree
		if w == nil || w.Current == nil || w.Current.ID != "wt-b" {
			t.Fatalf("round %d used %+v", i, w)
		}
	}
	if pin.ID() != "wt-b" {
		t.Fatalf("spawn target = %q", pin.ID())
	}
	if _, err := s.Send(context.Background(), "new task without selection", SendOptions{Worktree: &prompts.WorktreeContext{}}); err != nil {
		t.Fatal(err)
	}
	s.DrainBackgroundWork()
	if pin.ID() != "" {
		t.Fatalf("no selection reused %q", pin.ID())
	}
	if w := be.runtimeAt(2).Worktree; w == nil || w.Current != nil {
		t.Fatalf("no selection became %+v", w)
	}
}

func TestQueuedMessageKeepsLocationForNextRound(t *testing.T) {
	pin := worktreepin.New()
	pin.Offer("job-a", "/a", "a", true)
	s := NewSession(SessionDeps{WorktreePin: pin})
	w := &prompts.WorktreeContext{Present: true, ID: "view-b", Path: "/b", Branch: "b"}
	s.InjectUserPrompt(UserPrompt{Text: "ask the contest agents to vote", Worktree: w})
	p, ok := s.RetractUserPrompt()
	if !ok || p.Worktree.ID != "view-b" || p.Text != "ask the contest agents to vote" {
		t.Fatalf("reclaimed %+v, %v", p, ok)
	}
	s.InjectUserPrompt(p)
	texts := s.drainPendingInjections()
	if len(texts) != 1 || texts[0] != p.Text {
		t.Fatalf("UI text = %v", texts)
	}
	if pin.ID() != "" {
		t.Fatalf("next round retained prior default %q", pin.ID())
	}
	if s.currentMessageWorktree().ID != "view-b" {
		t.Fatal("next round lost latest message location")
	}
	if !strings.Contains(p.ContextualText(), `"id":"view-b"`) {
		t.Fatal("history lost send-time location")
	}
}

func TestMidTurnMessageChangesOnlyFutureDefaultWorktree(t *testing.T) {
	r := &injectRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{}`)}},
		{Content: "done"},
	}}
	deps, be := recordingDeps(r, &fakeTools{result: domain.Ok("ok", nil)})
	pin := worktreepin.New()
	deps.WorktreePin = pin
	s := NewSession(deps)
	r.onRound = func(round int) {
		if round == 0 {
			s.InjectUserPrompt(UserPrompt{Text: "also start a new job here", Worktree: &prompts.WorktreeContext{Present: true, ID: "b", Path: "/b"}})
			if pin.ID() != "a" {
				t.Fatalf("input changed the target inside an unfinished batch: %q", pin.ID())
			}
		}
	}
	_, err := s.Send(context.Background(), "first job", SendOptions{Worktree: &prompts.WorktreeContext{Present: true, ID: "a", Path: "/a"}})
	if err != nil {
		t.Fatal(err)
	}
	if be.runtimeAt(0).Worktree.Current.ID != "a" || be.runtimeAt(1).Worktree.Current.ID != "b" {
		t.Fatal("rounds did not follow message boundaries")
	}
	if pin.ID() != "b" {
		t.Fatalf("new work still defaults to %q", pin.ID())
	}
	if !userTextSeen(r.seen[1], `"id":"b"`) {
		t.Fatal("model history lost interjection location")
	}
	s.DrainBackgroundWork()
}
