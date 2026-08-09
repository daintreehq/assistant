package backend_test

// Live integration tests against a REAL Daintree assistant backend on
// 127.0.0.1:8473. They are skip-guarded: every test first probes /health with a
// explicit opt-in and then probe /health, so `go test ./...` stays hermetic even
// when a developer happens to have the backend running. Run them deliberately:
//
//	DAINTREE_LIVE_TESTS=1 go test ./internal/backend/... -run Live -count=1
//
// These exercise the actual wire contract (named-event SSE stream, task envelope,
// capability descriptor) end-to-end — the unit tests in client_test.go cover the
// same paths against an httptest fake, but only a live run proves the structs match
// what the server actually emits.

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/backend"
)

// liveBaseURL is the hardcoded local backend endpoint (mirrors backend.DefaultBaseURL).
const liveBaseURL = "http://127.0.0.1:8473"

// requireLiveBackend probes /health with a 2s timeout and skips the test when the
// backend is unreachable or unhealthy. Returns a Client bound to the live endpoint.
func requireLiveBackend(t *testing.T) *backend.Client {
	t.Helper()
	if os.Getenv("DAINTREE_LIVE_TESTS") != "1" {
		t.Skip("set DAINTREE_LIVE_TESTS=1 to run live backend integration tests")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, liveBaseURL+"/health", nil)
	if err != nil {
		t.Skipf("backend not running: build healthz request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		t.Skipf("backend not running: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Skipf("backend not healthy: /health status %d", resp.StatusCode)
	}

	return backend.NewClient(backend.ClientConfig{BaseURL: liveBaseURL})
}

func TestLiveRespondStream(t *testing.T) {
	c := requireLiveBackend(t)

	maxTokens := 30
	var streamed strings.Builder

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := c.RespondStream(ctx, backend.RespondRequest{
		Session: backend.RespondSession{ID: "sess_live_integration", TurnID: "turn_live_integration"},
		Input: backend.RespondInput{
			Messages: []backend.Message{
				{Role: "user", Content: []byte(`"Reply with exactly: integration ok"`)},
			},
		},
		Generation: &backend.Generation{
			Stream:         true,
			ResponseFormat: "text",
			MaxTokens:      &maxTokens,
		},
	}, backend.StreamCallbacks{
		OnContent: func(s string) { streamed.WriteString(s) },
	})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}

	if res.Meta.State == "" {
		t.Errorf("meta state is empty; want a refreshed opaque state token")
	}
	if strings.TrimSpace(res.Message.Content) == "" {
		t.Errorf("message content is empty; want a non-empty reply")
	}
	if res.Usage.TotalTokens <= 0 {
		t.Errorf("usage total tokens = %d; want > 0", res.Usage.TotalTokens)
	}
	// The streamed callback content must reassemble into the returned message
	// (the parser accumulates the same fragments it hands to OnContent).
	if streamed.String() != res.Message.Content {
		t.Errorf("streamed content %q != result content %q", streamed.String(), res.Message.Content)
	}
}

func TestLiveRunTask(t *testing.T) {
	c := requireLiveBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	out, err := backend.RunTerminalSummarize(ctx, c, backend.TerminalSummarizeInput{
		Purpose: "summarize an npm install for an integration test",
		Tail:    "npm install\nadded 42 packages in 3s\ndone",
	})
	if err != nil {
		t.Fatalf("RunTerminalSummarize: %v", err)
	}
	if strings.TrimSpace(out.Text) == "" {
		t.Errorf("summarize output text is empty; want a terse factual summary")
	}
}

func TestLiveCapabilities(t *testing.T) {
	c := requireLiveBackend(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	caps, err := c.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	// The CLI speaks protocol version 2, so the advertised range must include it.
	if !(caps.Protocol.Min <= 2 && 2 <= caps.Protocol.Max) {
		t.Errorf("protocol range [%d,%d] does not include 2", caps.Protocol.Min, caps.Protocol.Max)
	}
	if len(caps.Tasks) == 0 {
		t.Errorf("capabilities advertised no tasks; want a non-empty task list")
	}
}
