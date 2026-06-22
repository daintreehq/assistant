package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// TestDispatchStampsRunID: a dispatch within a run carries
// ctx.RunID onto its audit row, and a scheduler-built ctx (no RunID) leaves it
// absent.
func TestDispatchStampsRunID(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("x.read", domain.RiskRead))
	s := &fakeStore{}

	withRun := baseCtx(s, nil, domain.TierOperator, domain.ActorMain)
	withRun.RunID = "run_dead00"
	r.Dispatch(context.Background(), "x.read", json.RawMessage(`{"x":1}`), withRun)

	withoutRun := baseCtx(s, nil, domain.TierOperator, domain.ActorMain)
	r.Dispatch(context.Background(), "x.read", json.RawMessage(`{"x":1}`), withoutRun)

	if len(s.audits) != 2 {
		t.Fatalf("want 2 audit rows, got %d", len(s.audits))
	}
	var stamped, unstamped *domain.AuditRecord
	for i := range s.audits {
		if s.audits[i].RunID != nil && *s.audits[i].RunID == "run_dead00" {
			stamped = &s.audits[i]
		} else {
			unstamped = &s.audits[i]
		}
	}
	if stamped == nil {
		t.Fatal("a run dispatch must stamp RunID on the audit row")
	}
	if unstamped == nil || unstamped.RunID != nil {
		t.Fatal("a scheduler dispatch must leave RunID absent")
	}
}

// countingQueue records every published dedupeKey so we can assert the registry
// emits a STABLE (tick-free) dedupeKey across repeated denials of the same
// (actor, tool) — a real storage Queue collapses those into one count-bumped row.
type countingQueue struct{ keys []string }

func (q *countingQueue) Publish(_ context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	q.keys = append(q.keys, args.DedupeKey)
	return domain.QueueEvent{}, nil
}

// countingCtx builds a watcher ToolContext wired to a countingQueue (baseCtx is
// typed to *fakeQueue, so denial-dedupe tests build their own ctx).
func countingCtx(q *countingQueue, actorID string) *ToolContext {
	return &ToolContext{
		Config:  config.AppConfig{Tier: domain.TierSystem},
		DB:      &fakeStore{},
		Queue:   q,
		Actor:   domain.ActorWatcher,
		ActorID: actorID,
	}
}

// TestDispatchRepeatedDenialsShareDedupeKey: repeated
// autonomous denials of the same tool by the same actor publish a single STABLE
// dedupeKey (the storage Queue then count-bumps one inbox row).
func TestDispatchRepeatedDenialsShareDedupeKey(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	q := &countingQueue{}
	// Watcher actor, no actorId → blocked → denial published; do it twice.
	ctx := countingCtx(q, "")
	r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctx)
	r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":2}`), ctx)

	if len(q.keys) != 2 {
		t.Fatalf("each denial should publish, got %d publishes", len(q.keys))
	}
	if q.keys[0] != "denied:watcher:g.echo" || q.keys[1] != "denied:watcher:g.echo" {
		t.Fatalf("repeated denials must share a tick-free dedupeKey, got %v", q.keys)
	}
}

// TestDispatchDistinctActorsDoNotCollapse: distinct actor
// ids must NOT collapse — the actorId segment keeps each watcher/timer separate.
func TestDispatchDistinctActorsDoNotCollapse(t *testing.T) {
	r := NewRegistry()
	_ = r.Register(echoTool("g.echo", domain.RiskGit))
	q := &countingQueue{}

	ctxA := countingCtx(q, "wch_a")
	ctxB := countingCtx(q, "wch_b")
	r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctxA)
	r.Dispatch(context.Background(), "g.echo", json.RawMessage(`{"x":1}`), ctxB)

	if len(q.keys) != 2 {
		t.Fatalf("want 2 publishes, got %d", len(q.keys))
	}
	if q.keys[0] == q.keys[1] {
		t.Fatalf("distinct actors must not share a dedupeKey, both %q", q.keys[0])
	}
	want := map[string]bool{
		"denied:watcher:wch_a:g.echo": true,
		"denied:watcher:wch_b:g.echo": true,
	}
	for _, k := range q.keys {
		if !want[k] {
			t.Errorf("unexpected dedupeKey %q", k)
		}
	}
}
