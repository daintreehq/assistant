// Package e2e drives the assistant end-to-end against in-process fakes: a fake
// DeepSeek SSE server (OpenAI-compatible /chat/completions) and a fake Daintree
// MCP server (go-sdk Streamable HTTP over httptest). These exercise the full wiring
// — app.Create → Session.Send → tool dispatch → result feedback → persistence — and
// the binary-level --json one-shot, which no unit test covers (jsonout_test.go
// exercises the sink in isolation, not the wired binary).
package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// sseRound is one scripted streaming response from the fake DeepSeek server. The
// server replays one round per /chat/completions request, in order: the first
// round typically streams a little prose then a tool call; the second streams the
// final answer. Each round is rendered as an OpenAI-compatible SSE body.
type sseRound struct {
	// contentTokens are streamed as individual delta.content chunks (in order).
	contentTokens []string
	// toolName/toolArgs, when toolName != "", append a single fragmented tool call
	// (id + name in one chunk, args split across two) and a tool_calls finish.
	toolName string
	toolArgs string
	// usage, when non-nil, rides the final usage-only chunk (empty choices).
	usage *fakeUsage
}

type fakeUsage struct {
	prompt, completion, total, cached int
}

// fakeDeepSeek is a scripted OpenAI-compatible /chat/completions SSE server. It
// hands out one round per request from rounds[], and records every request body so
// a test can assert what was sent (model, tool result feedback, prompt_cache_key).
type fakeDeepSeek struct {
	srv      *httptest.Server
	mu       sync.Mutex
	calls    int
	rounds   []sseRound
	requests []map[string]any
}

// newFakeDeepSeek starts a fake server that replays the given rounds in order.
func newFakeDeepSeek(t *testing.T, rounds ...sseRound) *fakeDeepSeek {
	t.Helper()
	f := &fakeDeepSeek{rounds: rounds}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// baseURL is the value to feed DEEPSEEK_BASE_URL — the client appends
// /chat/completions, so the base must NOT include that suffix.
func (f *fakeDeepSeek) baseURL() string { return f.srv.URL }

func (f *fakeDeepSeek) handle(w http.ResponseWriter, r *http.Request) {
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

	write := func(s string) {
		_, _ = io.WriteString(w, s)
		if flusher != nil {
			flusher.Flush()
		}
	}

	for _, tok := range round.contentTokens {
		chunk := map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": tok}}},
		}
		write("data: " + mustJSON(chunk) + "\n\n")
	}

	if round.toolName != "" {
		// id + name in one chunk; args split across two fragments (exercises the
		// streamed tool-call accumulator).
		half := len(round.toolArgs) / 2
		write("data: " + mustJSON(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_e2e",
					"function": map[string]any{"name": round.toolName, "arguments": round.toolArgs[:half]},
				}},
			}}},
		}) + "\n\n")
		write("data: " + mustJSON(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index":    0,
					"function": map[string]any{"arguments": round.toolArgs[half:]},
				}},
			}}},
		}) + "\n\n")
		write("data: " + mustJSON(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
		}) + "\n\n")
	} else {
		write("data: " + mustJSON(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}},
		}) + "\n\n")
	}

	if round.usage != nil {
		write("data: " + mustJSON(map[string]any{
			"choices": []any{},
			"usage": map[string]any{
				"prompt_tokens":         round.usage.prompt,
				"completion_tokens":     round.usage.completion,
				"total_tokens":          round.usage.total,
				"prompt_tokens_details": map[string]any{"cached_tokens": round.usage.cached},
			},
		}) + "\n\n")
	}

	write("data: [DONE]\n\n")
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// lastRequestMessages returns the messages array sent on the Nth request (0-based),
// flattened to role/content for assertions about tool-result feedback.
func (f *fakeDeepSeek) requestMessages(n int) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		return nil
	}
	raw, _ := f.requests[n]["messages"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// callCount returns how many /chat/completions requests the server has served.
func (f *fakeDeepSeek) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// requestField returns the value of a top-level body key (e.g. "model",
// "reasoning_effort") on the Nth request (0-based), or nil when out of range or
// absent — used to assert which model id and reasoning effort each round was sent on.
func (f *fakeDeepSeek) requestField(n int, key string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n >= len(f.requests) {
		return nil
	}
	return f.requests[n][key]
}

// containsToolMessage reports whether any message in the request is a tool-result
// message (role:"tool") whose content mentions the given substring — used to assert
// the dispatched tool's result was fed back into the next model round.
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
