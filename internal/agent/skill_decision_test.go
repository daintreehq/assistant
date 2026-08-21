package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// skill_decision_test.go covers the committed per-round skill decision: the projection
// from the backend's SkillsBlock, and the guarantee that it is emitted on EVERY committed
// round — including the rounds the eager skill:loaded cue is silent for, which is the gap
// the whole event exists to close.

// quietSkillBackend commits meta with an active set but NO newly-loaded delta: the shape
// of every round after the first, and the case the eager OnSkillLoaded callback never
// fires for (backend/sse.go only calls it when NewlyLoaded is non-empty).
type quietSkillBackend struct {
	degraded bool
}

func (b quietSkillBackend) RespondStream(_ context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	meta := backend.StreamMeta{
		Model: "daintree-assistant",
		State: "dst1.quiet",
		Skills: backend.SkillsBlock{
			Active:      []backend.SkillRef{{ID: "multi_agent", Title: "Multi-agent orchestration"}},
			NewlyLoaded: nil, // nothing changed this round
			Selector: backend.SelectorMeta{
				Ran:      true,
				Degraded: b.degraded,
				Reason:   "reused the prior active set",
			},
		},
	}
	if cb.OnSkillLoaded != nil && len(meta.Skills.NewlyLoaded) > 0 {
		cb.OnSkillLoaded(meta.Skills.NewlyLoaded)
	}
	if cb.OnMeta != nil {
		cb.OnMeta(meta)
	}
	if cb.OnContent != nil {
		cb.OnContent("answer")
	}
	return backend.RespondResult{
		Meta:    meta,
		Message: backend.RespondMessage{Role: "assistant", Content: "answer"},
	}, nil
}

func (quietSkillBackend) RunTask(context.Context, backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{}, nil
}

// decisionSink records only the decisions, so a test can assert how many rounds reported
// one without threading through the rest of the event vocabulary.
type decisionSink struct {
	NoopEventSink
	decisions []SkillDecisionEvent
	loads     int
}

func (s *decisionSink) SkillDecision(ev SkillDecisionEvent) {
	s.decisions = append(s.decisions, ev)
}
func (s *decisionSink) SkillLoaded([]string) { s.loads++ }

// The load-bearing case: a round where nothing new loaded still reports what is active.
// Without this, a consumer would have to reconstruct the active set by replaying every
// prior round's delta — which is both the burden the issue objects to and impossible to
// do correctly, since a degraded selector changes the set without a delta.
func TestSkillDecisionEmittedOnRoundWithNoNewLoads(t *testing.T) {
	sink := &decisionSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = quietSkillBackend{}
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if sink.loads != 0 {
		t.Fatalf("eager SkillLoaded fired %d time(s) on a no-delta round; the fixture is "+
			"not exercising the gap this event closes", sink.loads)
	}
	if len(sink.decisions) != 1 {
		t.Fatalf("SkillDecision fired %d time(s), want 1 per committed round", len(sink.decisions))
	}
	got := sink.decisions[0]
	if len(got.Active) != 1 || got.Active[0].ID != "multi_agent" {
		t.Fatalf("active = %#v, want the retained multi_agent skill", got.Active)
	}
	// Empty, never nil — the sinks marshal this straight to JSON, where nil would be null.
	if got.NewlyLoaded == nil {
		t.Fatal("NewlyLoaded is nil; it must be an allocated empty slice so it marshals as []")
	}
	if len(got.NewlyLoaded) != 0 {
		t.Fatalf("newlyLoaded = %#v, want empty", got.NewlyLoaded)
	}
}

// A degraded selector fails open into the prior set. The active set alone looks perfectly
// healthy, so the flag is the only thing that distinguishes "chose this runbook" from
// "kept this runbook because deciding failed".
func TestSkillDecisionCarriesDegradedSelector(t *testing.T) {
	sink := &decisionSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = quietSkillBackend{degraded: true}
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.decisions) != 1 {
		t.Fatalf("SkillDecision fired %d time(s), want 1", len(sink.decisions))
	}
	if !sink.decisions[0].Selector.Degraded {
		t.Fatal("Selector.Degraded lost in projection; a fail-open round would look clean")
	}
	if sink.decisions[0].Selector.Reason != "reused the prior active set" {
		t.Fatalf("Selector.Reason = %q", sink.decisions[0].Selector.Reason)
	}
}

// The projection copies refs VERBATIM. A diagnostic that quietly repaired or dropped a
// malformed backend ref would hide exactly the selector bug someone is reading it to
// find — unlike skillLabels, whose title fallback serves a human-readable replay.
func TestSkillDecisionFromCopiesRefsVerbatim(t *testing.T) {
	conf := 0.42
	got := skillDecisionFrom(backend.SkillsBlock{
		Active: []backend.SkillRef{
			{ID: "b", Title: "Beta"},
			{ID: "a"}, // no title: NOT back-filled from the id
			{},        // malformed: NOT dropped
		},
		NewlyLoaded: []backend.SkillRef{{ID: "a"}},
		Selector: backend.SelectorMeta{
			Ran: true, TaskType: "review", Confidence: &conf, Reason: "because",
		},
	})

	if len(got.Active) != 3 {
		t.Fatalf("active = %#v, want all 3 refs preserved including the malformed one", got.Active)
	}
	// Order preserved: the backend's ranking is information.
	if got.Active[0].ID != "b" || got.Active[1].ID != "a" {
		t.Fatalf("active order changed: %#v", got.Active)
	}
	if got.Active[1].Title != "" {
		t.Fatalf("active[1].Title = %q; the id must not be laundered into the title", got.Active[1].Title)
	}
	if got.Active[2] != (SkillRef{}) {
		t.Fatalf("active[2] = %#v, want the malformed ref preserved as-is", got.Active[2])
	}

	if got.Selector.Confidence == nil || *got.Selector.Confidence != 0.42 {
		t.Fatalf("confidence = %#v, want 0.42 carried by pointer", got.Selector.Confidence)
	}
	if got.Selector.TaskType != "review" || got.Selector.Reason != "because" || !got.Selector.Ran {
		t.Fatalf("selector = %#v", got.Selector)
	}
}

// Both slices are allocated even when the backend sent nothing at all, so the JSON sinks
// emit [] rather than null without each having to normalize.
func TestSkillDecisionFromAllocatesEmptySlices(t *testing.T) {
	got := skillDecisionFrom(backend.SkillsBlock{})
	if got.Active == nil || got.NewlyLoaded == nil {
		t.Fatalf("nil slice(s) from an empty block: active=%#v newlyLoaded=%#v",
			got.Active, got.NewlyLoaded)
	}
	if len(got.Active) != 0 || len(got.NewlyLoaded) != 0 {
		t.Fatalf("expected empty, got %#v / %#v", got.Active, got.NewlyLoaded)
	}
}

// Selector usage and the vestigial Prelude are dropped on purpose; pinned so a later
// "just pass the whole block through" refactor has to argue with a test.
func TestSkillDecisionEventHasNoUsageOrPrelude(t *testing.T) {
	got := skillDecisionFrom(backend.SkillsBlock{
		Selector: backend.SelectorMeta{Usage: &backend.Usage{}},
		Prelude:  backend.Prelude{ToolExecutions: []backend.PreludeExecution{{}}},
	})
	// A compile-time guarantee as much as a runtime one: SkillDecisionEvent has exactly
	// three fields, none of which can carry either.
	if got.Selector != (SkillSelectorOutcome{}) {
		t.Fatalf("selector = %#v, want the zero outcome (usage must not leak in)", got.Selector)
	}
}
