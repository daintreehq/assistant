// Package e2e drives the assistant end-to-end against in-process fakes: a fake
// Daintree BACKEND server (the native /v1/daintree/* protocol over httptest) and a
// fake Daintree MCP server (go-sdk Streamable HTTP over httptest). These exercise the
// full wiring — app.Create → Session.Send → backend respond stream → tool dispatch →
// result feedback → persistence — and the binary-level --json one-shot, which no unit
// test covers (jsonout_test.go exercises the sink in isolation, not the wired binary).
//
// The CLI no longer talks to DeepSeek directly: it speaks the Daintree-native wire
// protocol to the backend, which owns the system prompt, runbook selection, model
// routing, and the upstream model credentials. So the fake here is a fake BACKEND, not
// a fake DeepSeek — it serves named-event SSE on /v1/daintree/respond and JSON utility
// results on /v1/daintree/tasks. The binary is pointed at it via DAINTREE_BACKEND_URL
// (the dev/test endpoint override app.Create reads); in-process tests set the same env
// so app.Create builds a real backend.Client to the fake.
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// sseRound is one scripted streaming response from the fake backend. The server
// replays one round per /v1/daintree/respond request, in order: the first round
// typically streams a little prose then a tool call; the second streams the final
// answer. Each round is rendered as a Daintree-native named-event SSE body
// (meta → delta… → done).
type sseRound struct {
	// contentTokens are streamed as individual `delta` events with content (in order).
	contentTokens []string
	// tokenDelay, when > 0, pauses between content tokens so a PTY test can observe the
	// stream MID-FLIGHT (e.g. assert early prose reached scrollback before the paragraph seals).
	tokenDelay time.Duration
	// toolName/toolArgs, when toolName != "", append a single fragmented tool call
	// (id + name in one delta, args split across two) and a tool_calls finish. toolName
	// is the WIRE name the model emits (double-underscore, e.g. memory__list), which the
	// session resolves back to its internal dotted name (memory.list).
	toolName string
	toolArgs string
	// usage, when non-nil, rides the terminal `done` event.
	usage *fakeUsage
	// runbooks, when non-nil, replaces the default empty runbooks block on this round's
	// `meta` event. It is the WIRE shape (snake_case, as the backend really sends it),
	// so a test exercises the client's decode and the session's projection rather than
	// a pre-shaped struct.
	runbooks map[string]any
	// errorMessage, when non-empty, ends the round with a terminal `error` event instead
	// of `done` — a backend-side failure mid-stream, which is how a real turn fails. The
	// meta event still goes out first, because the client requires it before anything
	// else and a failure that skipped it would be testing the wrong error path.
	errorMessage string
}

// defaultRunbooksBlock is the "selector did not run" block every round sends unless the
// round scripts its own.
func defaultRunbooksBlock() map[string]any {
	return map[string]any{
		"active":       []any{},
		"newly_loaded": []any{},
		"prelude":      map[string]any{"tool_executions": []any{}},
		"selector":     map[string]any{"ran": false, "degraded": false},
	}
}

// runbooksBlock builds a wire-shaped runbooks block. active/newlyLoaded are id→title pairs
// given as flat [id, title, id, title, …] so a round stays readable at the call site.
func runbooksBlock(degraded bool, active []string, newlyLoaded []string) map[string]any {
	refs := func(flat []string) []any {
		out := make([]any, 0, len(flat)/2)
		for i := 0; i+1 < len(flat); i += 2 {
			out = append(out, map[string]any{"id": flat[i], "title": flat[i+1]})
		}
		return out
	}
	return map[string]any{
		"active":       refs(active),
		"newly_loaded": refs(newlyLoaded),
		"prelude":      map[string]any{"tool_executions": []any{}},
		"selector": map[string]any{
			"ran": true, "degraded": degraded, "task_type": "orchestration",
			"confidence": 0.9, "reason": "scripted",
		},
	}
}

type fakeUsage struct {
	prompt, completion, total, cached int
}

// fakeBackend is a scripted Daintree-native backend server. It hands out one round
// per /v1/daintree/respond request from rounds[], records every respond request body
// (so a test can assert what was sent — input.messages, input.tools, round counter),
// records every /v1/daintree/tasks request, and serves the health/version/capabilities
// probes. It is safe for concurrent use (the binary streams on its own goroutine).
type fakeBackend struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    int              // /v1/daintree/respond requests served
	rounds   []sseRound       // scripted respond rounds, consumed in order
	requests []map[string]any // recorded respond request bodies (generic JSON)
	tasks    []map[string]any // recorded task request bodies (generic JSON)
}

// newFakeBackend starts a fake backend that replays the given respond rounds in order.
func newFakeBackend(t *testing.T, rounds ...sseRound) *fakeBackend {
	t.Helper()
	f := &fakeBackend{rounds: rounds}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/daintree/respond", f.handleRespond)
	mux.HandleFunc("/v1/daintree/tasks", f.handleTasks)
	mux.HandleFunc("/v1/daintree/capabilities", f.handleCapabilities)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"status": "ok"}) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]any{"status": "ready"}) })
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]any{"server_version": "test", "protocol": map[string]any{"min": 2, "max": 2}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// baseURL is the value to feed DAINTREE_BACKEND_URL — the client appends
// /v1/daintree/respond (etc.), so the base must NOT include that suffix.
func (f *fakeBackend) baseURL() string { return f.srv.URL }

// handleRespond serves one scripted round as a Daintree-native named-event SSE stream
// (meta first, then delta events, then a terminal done). It flushes after each event so
// the binary streams token by token.
func (f *fakeBackend) handleRespond(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)

	f.mu.Lock()
	idx := f.calls
	f.calls++
	f.requests = append(f.requests, parsed)
	var round sseRound
	if idx < len(f.rounds) {
		round = f.rounds[idx]
	} else if len(f.rounds) > 0 {
		round = f.rounds[len(f.rounds)-1] // replay the last round if over-driven
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	writeEvent := func(event string, data any) {
		_, _ = io.WriteString(w, "event: "+event+"\n")
		_, _ = io.WriteString(w, "data: "+mustJSON(data)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}

	// meta ALWAYS first, before any token — carries the opaque state token, the runbooks
	// outcome (empty here), and version markers. The client errors if it never arrives.
	runbooks := round.runbooks
	if runbooks == nil {
		runbooks = defaultRunbooksBlock()
	}
	writeEvent("meta", map[string]any{
		"protocol_version": 2,
		"request_id":       "req_1",
		"model":            "daintree-assistant",
		"runbooks":           runbooks,
		"state":            "dst1.test",
		"catalog_revision": "sha256:test",
		"prompt_version":   "test",
		"warnings":         []any{},
	})

	for i, tok := range round.contentTokens {
		if round.tokenDelay > 0 && i > 0 {
			time.Sleep(round.tokenDelay)
		}
		writeEvent("delta", map[string]any{"content": tok})
	}

	finish := "stop"
	if round.toolName != "" {
		finish = "tool_calls"
		// id + name in one fragment; args split across two (exercises the streamed
		// tool-call accumulator). OpenAI-style tool_call deltas accumulated by index.
		half := len(round.toolArgs) / 2
		writeEvent("delta", map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_e2e", "type": "function",
				"function": map[string]any{"name": round.toolName, "arguments": round.toolArgs[:half]},
			}},
		})
		writeEvent("delta", map[string]any{
			"tool_calls": []any{map[string]any{
				"index":    0,
				"function": map[string]any{"arguments": round.toolArgs[half:]},
			}},
		})
	}

	usage := map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0, "cached_tokens": 0}
	if round.usage != nil {
		usage = map[string]any{
			"prompt_tokens":     round.usage.prompt,
			"completion_tokens": round.usage.completion,
			"total_tokens":      round.usage.total,
			"cached_tokens":     round.usage.cached,
		}
	}
	if round.errorMessage != "" {
		// The NESTED envelope the client actually decodes (backend.Envelope →
		// EnvelopeError). A flat {message,type} payload decodes to an empty error and
		// the client substitutes a generic "stream_error", so the test would pass while
		// exercising the malformed-payload fallback instead of real error decoding.
		writeEvent("error", map[string]any{"error": map[string]any{
			"type": "api_error", "code": "upstream_error", "message": round.errorMessage,
		}})
		return
	}
	writeEvent("done", map[string]any{"finish_reason": finish, "usage": usage})
}

// handleTasks serves a daintree.task.result JSON body for any utility task. The CLI
// sends task DATA only; the backend owns the prompt/model/schema. The fake returns a
// minimal valid output per known task id (checkpoint / memory_distill) and an
// empty object otherwise.
func (f *fakeBackend) handleTasks(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed map[string]any
	_ = json.Unmarshal(body, &parsed)
	task, _ := parsed["task"].(string)

	f.mu.Lock()
	f.tasks = append(f.tasks, parsed)
	f.mu.Unlock()

	var output any
	switch task {
	case "checkpoint":
		output = map[string]any{"goal": "test goal", "next_actions": []string{"continue"}}
	case "memory_distill":
		output = map[string]any{"facts": []any{}}
	default:
		output = map[string]any{}
	}

	writeJSON(w, map[string]any{
		"id":             "t",
		"object":         "daintree.task.result",
		"task":           task,
		"model":          "m",
		"output":         output,
		"finish_reason":  "stop",
		"usage":          map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2, "cached_tokens": 0},
		"prompt_version": task,
	})
}

// handleCapabilities serves a minimal but valid capabilities descriptor.
func (f *fakeBackend) handleCapabilities(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"server_version": "test",
		"protocol":       map[string]any{"min": 2, "max": 2},
		"respond": map[string]any{
			"endpoint":                 "/v1/daintree/respond",
			"model":                    "daintree-assistant",
			"streaming":                true,
			"stream_events":            []string{"meta", "delta", "done", "error"},
			"system_messages_accepted": false,
			"max_active_runbooks":        3,
			"metadata_transport":       "sse",
			// Advertise the terminal-geometry contract so the PTY runs exercise the OPEN
			// gate. A fake that withheld it would leave every end-to-end path testing only
			// the fail-closed branch, which is the branch that cannot regress.
			"display_context": true,
		},
		"runbooks": map[string]any{
			"catalog_revision": "sha256:test",
			"manual_resolve":   false,
			// Advertised for the same reason display_context above is: a fake that withheld
			// it would leave every end-to-end path exercising only the fail-closed branch,
			// which is the branch that cannot regress. With it on, an e2e --runbook run
			// actually reaches the wire.
			"pinned_runbook_ids": true,
			"catalog": []map[string]any{
				{"id": "daintree.foundation", "title": "Foundation"},
				{"id": "daintree.orchestration.multi-agent", "title": "Multi-agent orchestration"},
			},
		},
		"tasks": []string{
			"checkpoint", "memory_distill", "watcher_classify", "terminal_judge",
			"terminal_summarize", "terminal_extract_text", "terminal_extract_json",
			"extraction_verdict", "runbook_step_consistency",
		},
		"limits": map[string]any{"request_bytes": 1 << 20, "tools": 256},
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, mustJSON(v))
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// requestMessages returns the input.messages array sent on the Nth /v1/daintree/respond
// request (0-based), flattened to role/content maps for assertions about tool-result
// feedback. The backend request body is {protocol_version, session, input:{messages,
// tools, …}, …}, so the conversation lives under input.messages (NOT a top-level
// "messages" array as the old OpenAI body had).
func (f *fakeBackend) requestMessages(n int) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		return nil
	}
	input, _ := f.requests[n]["input"].(map[string]any)
	raw, _ := input["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// request returns the WHOLE decoded body of the Nth respond request (0-based), for
// assertions about the structured envelope outside input.messages — startup, runtime,
// turn — or about what must never appear anywhere in it.
func (f *fakeBackend) request(n int) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		return nil
	}
	return f.requests[n]
}

// requestTools returns the input.tools array sent on the Nth respond request, as
// generic maps — used to assert the local tool inventory reached the backend.
func (f *fakeBackend) requestTools(n int) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		return nil
	}
	input, _ := f.requests[n]["input"].(map[string]any)
	raw, _ := input["tools"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, tdef := range raw {
		if mm, ok := tdef.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// callCount returns how many /v1/daintree/respond requests the server has served.
func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// containsToolMessage reports whether any message in the request is a tool-result
// message (role:"tool") whose content mentions the given substring — used to assert
// the dispatched tool's result was fed back into the next backend round. On the
// Daintree wire a tool message's content is a JSON string, so it decodes to a Go
// string here exactly as the old OpenAI body did.
func containsToolMessage(msgs []map[string]any, substr string) bool {
	for _, m := range msgs {
		if role, _ := m["role"].(string); role != "tool" {
			continue
		}
		if c, _ := m["content"].(string); strings.Contains(c, substr) {
			return true
		}
	}
	return false
}
