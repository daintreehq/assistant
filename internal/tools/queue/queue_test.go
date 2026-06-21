package queue

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

type memQueue struct {
	lastOpts domain.QueueDigestOptions
	resolved bool
	pubCount int
}

func (m *memQueue) Publish(_ context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error) {
	return domain.QueueEvent{ID: "evt_1", Title: args.Title, Count: m.pubCount}, nil
}
func (m *memQueue) Digest(_ context.Context, opts domain.QueueDigestOptions) ([]domain.QueueEvent, error) {
	m.lastOpts = opts
	return []domain.QueueEvent{{ID: "evt_1"}}, nil
}
func (m *memQueue) Resolve(context.Context, string) (bool, error) { return m.resolved, nil }
func (m *memQueue) Format([]domain.QueueEvent) string             { return "digest-text" }

func find(ts []*tools.Tool, name string) *tools.Tool {
	for _, t := range ts {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// publish on a dedupe (count>1) annotates the summary with (×N).
func TestPublishDedupeSummary(t *testing.T) {
	q := &memQueue{pubCount: 3}
	tool := find(Tools(Deps{Queue: q}), "queue.publish")
	args := json.RawMessage(`{"source":"system","severity":"info","title":"T","summary":"S"}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if want := "Published \"T\" to the inbox (×3)."; res.Summary != want {
		t.Fatalf("summary: got %q want %q", res.Summary, want)
	}
}

// digest threads severityAtLeast + maxItems into the query options.
func TestDigestOptions(t *testing.T) {
	q := &memQueue{}
	tool := find(Tools(Deps{Queue: q}), "queue.digest")
	args := json.RawMessage(`{"severityAtLeast":"attention","maxItems":5}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("expected ok, got %+v", res.Error)
	}
	if q.lastOpts.SeverityAtLeast == nil || *q.lastOpts.SeverityAtLeast != domain.SeverityAttention {
		t.Fatalf("severityAtLeast not threaded: %+v", q.lastOpts)
	}
	if q.lastOpts.MaxItems == nil || *q.lastOpts.MaxItems != 5 {
		t.Fatalf("maxItems not threaded: %+v", q.lastOpts)
	}
}

// resolve of a missing event is a non-recoverable QUEUE_NOT_FOUND.
func TestResolveNotFound(t *testing.T) {
	q := &memQueue{resolved: false}
	tool := find(Tools(Deps{Queue: q}), "queue.resolve")
	res := tool.Handle(context.Background(), json.RawMessage(`{"id":"evt_x"}`), &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeQueueNotFound || res.Error.Recoverable {
		t.Fatalf("expected non-recoverable QUEUE_NOT_FOUND, got %+v", res)
	}
}

// publish rejects an invalid severity (enum guard).
func TestPublishInvalidSeverity(t *testing.T) {
	tool := find(Tools(Deps{Queue: &memQueue{}}), "queue.publish")
	args := json.RawMessage(`{"source":"system","severity":"nope","title":"T","summary":"S"}`)
	res := tool.Handle(context.Background(), args, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeInvalidArgs {
		t.Fatalf("expected INVALID_ARGS, got %+v", res)
	}
}
