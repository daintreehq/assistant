package contextx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/tools"
)

// The snapshot is the model's "where am I actually pointed?" tool: it must name both
// endpoints — the Daintree MCP it drives and the assistant backend it thinks through.
// Before this, neither was reachable from any tool, so the model could only guess
// (ses_8cb40b4e).
func TestSnapshotReportsBothEndpoints(t *testing.T) {
	deps := Deps{
		MCP:        &fakeMCP{connected: true, url: "http://127.0.0.1:45454/mcp"},
		Router:     fakeRouter{},
		Queue:      fakeQueue{},
		BackendURL: func() string { return "https://assistant.daintree.org" },
	}
	res := newSnapshotTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("snapshot must succeed: %+v", res.Error)
	}
	if !strings.Contains(res.Summary, "http://127.0.0.1:45454/mcp") {
		t.Errorf("summary must name the MCP endpoint, got %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "https://assistant.daintree.org") {
		t.Errorf("summary must name the backend endpoint, got %q", res.Summary)
	}
	m := res.Result.(map[string]any)
	if m["backendUrl"] != "https://assistant.daintree.org" {
		t.Errorf("structured backendUrl = %v", m["backendUrl"])
	}
	if st, ok := m["mcp"].(MCPStatus); !ok || st.URL != "http://127.0.0.1:45454/mcp" {
		t.Errorf("structured mcp status must carry the endpoint, got %v", m["mcp"])
	}
}

// A nil BackendURL (any wiring that doesn't supply one) must degrade silently rather
// than emit an empty "Assistant backend:" line — context.snapshot never throws.
func TestSnapshotOmitsBackendLineWhenUnavailable(t *testing.T) {
	deps := Deps{MCP: &fakeMCP{connected: false}, Router: fakeRouter{}, Queue: fakeQueue{}}
	res := newSnapshotTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("snapshot must succeed: %+v", res.Error)
	}
	if strings.Contains(res.Summary, "Assistant backend") {
		t.Errorf("no backend line without a URL source, got %q", res.Summary)
	}
	if m := res.Result.(map[string]any); m["backendUrl"] != "" {
		t.Errorf("backendUrl = %v, want empty", m["backendUrl"])
	}
}

// context.snapshot is the tool the model reaches for when everything else is broken, so
// its "never throws" contract has to survive a hostile dependency too — a blank return
// or a panicking provider costs the backend line, never the snapshot.
func TestSnapshotSurvivesHostileBackendURLProvider(t *testing.T) {
	for name, provider := range map[string]func() string{
		"panics":     func() string { panic("backend wrapper is half-built") },
		"whitespace": func() string { return "   " },
	} {
		t.Run(name, func(t *testing.T) {
			deps := Deps{MCP: &fakeMCP{connected: true}, Router: fakeRouter{}, Queue: fakeQueue{}, BackendURL: provider}
			res := newSnapshotTool(deps).Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
			if !res.Ok {
				t.Fatalf("snapshot must still succeed: %+v", res.Error)
			}
			if strings.Contains(res.Summary, "Assistant backend") {
				t.Errorf("unusable provider must not render a line, got %q", res.Summary)
			}
		})
	}
}

// The provider is read on EVERY call, not captured once: /login hot-swaps the backend
// client mid-session (backend.Swappable), and a snapshot that kept reporting the old
// endpoint would be confidently wrong about the one thing this line exists to answer.
func TestSnapshotRereadsBackendURLPerCall(t *testing.T) {
	current := "http://127.0.0.1:8473"
	deps := Deps{
		MCP: &fakeMCP{connected: true}, Router: fakeRouter{}, Queue: fakeQueue{},
		BackendURL: func() string { return current },
	}
	tool := newSnapshotTool(deps)
	first := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	current = "https://assistant.daintree.org"
	second := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})

	if !strings.Contains(first.Summary, "http://127.0.0.1:8473") {
		t.Errorf("first snapshot = %q", first.Summary)
	}
	if !strings.Contains(second.Summary, "https://assistant.daintree.org") {
		t.Errorf("swapped endpoint not picked up: %q", second.Summary)
	}
}
