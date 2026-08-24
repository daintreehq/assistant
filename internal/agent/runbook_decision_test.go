package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/models"
)

// runbook_decision_test.go covers the committed per-round runbook decision: the projection
// from the backend's RunbooksBlock, and the guarantee that it is emitted on EVERY committed
// round — including the rounds the eager runbook:loaded cue is silent for, which is the gap
// the whole event exists to close.

// quietRunbookBackend commits meta with an active set but NO newly-loaded delta: the shape
// of every round after the first, and the case the eager OnRunbookLoaded callback never
// fires for (backend/sse.go only calls it when NewlyLoaded is non-empty).
type quietRunbookBackend struct {
	degraded bool
}

func (b quietRunbookBackend) RespondStream(_ context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	meta := backend.StreamMeta{
		Model: "daintree-assistant",
		State: "dst1.quiet",
		Runbooks: backend.RunbooksBlock{
			Active:      []backend.RunbookRef{{ID: "multi_agent", Title: "Multi-agent orchestration"}},
			NewlyLoaded: nil, // nothing changed this round
			Selector: backend.SelectorMeta{
				Ran:      true,
				Degraded: b.degraded,
				Reason:   "reused the prior active set",
			},
		},
	}
	if cb.OnRunbookLoaded != nil && len(meta.Runbooks.NewlyLoaded) > 0 {
		cb.OnRunbookLoaded(meta.Runbooks.NewlyLoaded)
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

func (quietRunbookBackend) RunTask(context.Context, backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{}, nil
}

// decisionSink records only the decisions, so a test can assert how many rounds reported
// one without threading through the rest of the event vocabulary.
type decisionSink struct {
	NoopEventSink
	decisions []RunbookDecisionEvent
	loads     int
}

func (s *decisionSink) RunbookDecision(ev RunbookDecisionEvent) {
	s.decisions = append(s.decisions, ev)
}
func (s *decisionSink) RunbookLoaded([]string) { s.loads++ }

// The load-bearing case: a round where nothing new loaded still reports what is active.
// Without this, a consumer would have to reconstruct the active set by replaying every
// prior round's delta — which is both the burden the issue objects to and impossible to
// do correctly, since a degraded selector changes the set without a delta.
func TestRunbookDecisionEmittedOnRoundWithNoNewLoads(t *testing.T) {
	sink := &decisionSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = quietRunbookBackend{}
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	if sink.loads != 0 {
		t.Fatalf("eager RunbookLoaded fired %d time(s) on a no-delta round; the fixture is "+
			"not exercising the gap this event closes", sink.loads)
	}
	if len(sink.decisions) != 1 {
		t.Fatalf("RunbookDecision fired %d time(s), want 1 per committed round", len(sink.decisions))
	}
	got := sink.decisions[0]
	if len(got.Active) != 1 || got.Active[0].ID != "multi_agent" {
		t.Fatalf("active = %#v, want the retained multi_agent runbook", got.Active)
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
func TestRunbookDecisionCarriesDegradedSelector(t *testing.T) {
	sink := &decisionSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = quietRunbookBackend{degraded: true}
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "carry on", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(sink.decisions) != 1 {
		t.Fatalf("RunbookDecision fired %d time(s), want 1", len(sink.decisions))
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
// find — unlike runbookLabels, whose title fallback serves a human-readable replay.
func TestRunbookDecisionFromCopiesRefsVerbatim(t *testing.T) {
	conf := 0.42
	got := runbookDecisionFrom(backend.RunbooksBlock{
		Active: []backend.RunbookRef{
			{ID: "b", Title: "Beta"},
			{ID: "a"}, // no title: NOT back-filled from the id
			{},        // malformed: NOT dropped
		},
		NewlyLoaded: []backend.RunbookRef{{ID: "a"}},
		Selector: backend.SelectorMeta{
			Ran: true, TaskType: "review", Confidence: &conf, Reason: "because",
		},
	})

	// Assert the WHOLE projection, field by field. A subset check here would pass an
	// implementation that dropped every title, or returned an empty NewlyLoaded.
	wantActive := []RunbookRef{
		{ID: "b", Title: "Beta"}, // order preserved: the backend's ranking is information
		{ID: "a"},                // title NOT back-filled from the id
		{},                       // malformed ref NOT dropped
	}
	if !equalRunbookRefs(got.Active, wantActive) {
		t.Fatalf("active = %#v, want %#v", got.Active, wantActive)
	}
	wantNewly := []RunbookRef{{ID: "a"}}
	if !equalRunbookRefs(got.NewlyLoaded, wantNewly) {
		t.Fatalf("newlyLoaded = %#v, want %#v", got.NewlyLoaded, wantNewly)
	}

	if got.Selector.Confidence == nil || *got.Selector.Confidence != 0.42 {
		t.Fatalf("confidence = %#v, want 0.42 carried by pointer", got.Selector.Confidence)
	}
	if got.Selector.TaskType != "review" || got.Selector.Reason != "because" || !got.Selector.Ran {
		t.Fatalf("selector = %#v", got.Selector)
	}
	if got.Selector.Degraded {
		t.Fatalf("selector.Degraded invented: %#v", got.Selector)
	}
}

func equalRunbookRefs(a, b []RunbookRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Duplicate ids and characters that need JSON escaping are carried through untouched —
// the projection must not dedupe, reorder, or sanitize what the backend said.
func TestRunbookDecisionFromPreservesDuplicatesAndEscapableText(t *testing.T) {
	got := runbookDecisionFrom(backend.RunbooksBlock{
		Active: []backend.RunbookRef{
			{ID: "dup", Title: "First"},
			{ID: "dup", Title: "Second"},
			{ID: `quote"back\slash`, Title: "line\nbreak — ünïcode"},
		},
	})
	if len(got.Active) != 3 {
		t.Fatalf("duplicate ids were collapsed: %#v", got.Active)
	}
	if got.Active[0].Title != "First" || got.Active[1].Title != "Second" {
		t.Fatalf("duplicate ids lost their distinct titles: %#v", got.Active)
	}
	if got.Active[2].ID != `quote"back\slash` || got.Active[2].Title != "line\nbreak — ünïcode" {
		t.Fatalf("escapable text was altered: %#v", got.Active[2])
	}
}

// divergentRunbookBackend fires the EAGER cue with one runbook, then commits meta describing
// a DIFFERENT active set — the retry shape, where attempt 1 loaded something the
// committed attempt did not keep. Session must wire the decision to the committed meta,
// not to the eager cue.
type divergentRunbookBackend struct{}

func (divergentRunbookBackend) RespondStream(_ context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	// What attempt 1 eagerly reported, before it failed and was retried.
	eager := []backend.RunbookRef{{ID: "abandoned", Title: "Abandoned by the retry"}}
	if cb.OnRunbookLoaded != nil {
		cb.OnRunbookLoaded(eager)
	}
	// What the attempt that actually committed selected.
	meta := backend.StreamMeta{
		State: "dst1.committed",
		Runbooks: backend.RunbooksBlock{
			Active:      []backend.RunbookRef{{ID: "committed", Title: "Committed runbook"}},
			NewlyLoaded: nil,
			Selector:    backend.SelectorMeta{Ran: true},
		},
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

func (divergentRunbookBackend) RunTask(context.Context, backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{}, nil
}

// The authoritative-source guarantee. Wiring the decision to the eager refs would be an
// easy and invisible mistake — both callbacks carry []backend.RunbookRef — and it would
// silently make the event report a selection the round never used.
func TestRunbookDecisionSourcedFromCommittedMetaNotEagerCue(t *testing.T) {
	sink := &decisionSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = divergentRunbookBackend{}
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if sink.loads != 1 {
		t.Fatalf("eager RunbookLoaded fired %d time(s), want 1 — the fixture is not "+
			"exercising the divergence", sink.loads)
	}
	if len(sink.decisions) != 1 {
		t.Fatalf("RunbookDecision fired %d time(s), want exactly 1", len(sink.decisions))
	}
	got := sink.decisions[0]
	if len(got.Active) != 1 || got.Active[0].ID != "committed" {
		t.Fatalf("active = %#v, want the COMMITTED set; the decision is wired to the "+
			"eager cue rather than to the committed meta", got.Active)
	}
	for _, ref := range got.Active {
		if ref.ID == "abandoned" {
			t.Fatal("the abandoned attempt's runbook leaked into the committed decision")
		}
	}
}

// Both slices are allocated even when the backend sent nothing at all, so the JSON sinks
// emit [] rather than null without each having to normalize.
func TestRunbookDecisionFromAllocatesEmptySlices(t *testing.T) {
	got := runbookDecisionFrom(backend.RunbooksBlock{})
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
func TestRunbookDecisionEventHasNoUsageOrPrelude(t *testing.T) {
	got := runbookDecisionFrom(backend.RunbooksBlock{
		Selector: backend.SelectorMeta{Usage: &backend.Usage{}},
		Prelude:  backend.Prelude{ToolExecutions: []backend.PreludeExecution{{}}},
	})
	// A compile-time guarantee as much as a runtime one: RunbookDecisionEvent has exactly
	// three fields, none of which can carry either.
	if got.Selector != (RunbookSelectorOutcome{}) {
		t.Fatalf("selector = %#v, want the zero outcome (usage must not leak in)", got.Selector)
	}
}
