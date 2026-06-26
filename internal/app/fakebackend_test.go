package app

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

// fakeBackend is a scripted backend.Backend for the app unit tests. The real client
// points at the hardcoded dev endpoint (backend.DefaultBaseURL) and must never be
// required by a unit test, so any test that runs a turn or a utility task injects one
// of these via CreateOptions{BackendOverride: ...}. It satisfies the full
// backend.Backend surface (RespondStream, RunTask, Capabilities, Version, Health,
// Ready, BaseURL) and is safe for concurrent use.
type fakeBackend struct {
	// respond overrides RespondStream; nil ⇒ the default single-round "ok" reply that
	// drives cb.OnMeta + cb.OnContent and returns matching content with no tool calls.
	respond func(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error)
	// task overrides RunTask; nil ⇒ an empty-object output for the requested task.
	task func(ctx context.Context, req backend.TaskRequest) (backend.TaskResult, error)

	mu       sync.Mutex
	lastReq  backend.RespondRequest
	respondN int
}

func (f *fakeBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	f.mu.Lock()
	f.lastReq = req
	f.respondN++
	f.mu.Unlock()
	if f.respond != nil {
		return f.respond(ctx, req, cb)
	}
	// Default: a clean single-round prose reply with no tool calls.
	meta := backend.StreamMeta{Model: "daintree-assistant", State: "st.test"}
	if cb.OnMeta != nil {
		cb.OnMeta(meta)
	}
	if cb.OnContent != nil {
		cb.OnContent("ok")
	}
	return backend.RespondResult{
		Meta:    meta,
		Message: backend.RespondMessage{Role: "assistant", Content: "ok"},
	}, nil
}

func (f *fakeBackend) RunTask(ctx context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	if f.task != nil {
		return f.task(ctx, req)
	}
	return backend.TaskResult{Task: req.Task, Output: json.RawMessage("{}"), Model: "daintree-assistant"}, nil
}

func (f *fakeBackend) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{}, nil
}
func (f *fakeBackend) Version(context.Context) (backend.Version, error) {
	return backend.Version{}, nil
}
func (f *fakeBackend) Health(context.Context) error { return nil }
func (f *fakeBackend) Ready(context.Context) error  { return nil }
func (f *fakeBackend) BaseURL() string              { return "http://fake.test" }

// lastRequest returns a copy of the most recent RespondRequest the backend saw.
func (f *fakeBackend) lastRequest() backend.RespondRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastReq
}
