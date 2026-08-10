package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/prompts"
)

// The endpoint has to survive the hop into the backend contract, since request.runtime
// is the only place the model sees it without spending a tool call (ses_8cb40b4e: asked
// which MCP URL it was on, the assistant guessed).
func TestBuildMCPServersCarriesEndpoint(t *testing.T) {
	out := buildMCPServers([]prompts.MCPServerContext{
		{Name: "daintree", URL: "http://127.0.0.1:45454/mcp", Description: "Daintree control plane"},
		{Name: "daintree-docs", URL: "https://daintree.org/api/mcp"},
	})
	if len(out) != 2 {
		t.Fatalf("want 2 servers, got %+v", out)
	}
	if !strings.Contains(out[0].Description, "http://127.0.0.1:45454/mcp") ||
		!strings.Contains(out[0].Description, "Daintree control plane") {
		t.Errorf("description = %q", out[0].Description)
	}
	if !strings.Contains(out[1].Description, "https://daintree.org/api/mcp") {
		t.Errorf("description without a role must still carry the endpoint: %q", out[1].Description)
	}
	// The block is cached ahead of the tool schemas, so nothing that fluctuates
	// mid-session (transport, tool count, connected) may ride it.
	for _, s := range out {
		if s.Transport != "" || s.ToolCount != nil || s.Status != "" {
			t.Errorf("volatile field set on a cached block: %+v", s)
		}
	}
}

// A half-filled entry is dropped rather than rendered as a nameless or endpoint-less
// line, and the backend's strict max_lengths are respected so an odd endpoint can never
// 400 the whole turn. Clamping is by RUNE because pydantic's max_length counts code
// points — a byte-based cut would both truncate short and split a multibyte character.
func TestBuildMCPServersDropsIncompleteAndClamps(t *testing.T) {
	out := buildMCPServers([]prompts.MCPServerContext{
		{Name: "", URL: "http://example.test/mcp"},
		{Name: "nourl", URL: "   "},
		{
			Name:        strings.Repeat("é", 300),
			URL:         "http://example.test/mcp",
			Description: strings.Repeat("é", 5000),
		},
	})
	if len(out) != 1 {
		t.Fatalf("want only the complete entry, got %+v", out)
	}
	// Exactly at the limit, not merely under it: a mutation that truncated everything to
	// one character would satisfy a bounds-only assertion.
	if got := len([]rune(out[0].Name)); got != 128 {
		t.Errorf("name = %d runes, want exactly the 128 limit", got)
	}
	if got := len([]rune(out[0].Description)); got != 4096 {
		t.Errorf("description = %d runes, want exactly the 4096 limit", got)
	}
	if !strings.HasPrefix(out[0].Description, "endpoint http://example.test/mcp") {
		t.Errorf("clamp ate the endpoint: %q", out[0].Description)
	}
}

// Nothing configured ⇒ the field must VANISH from the wire (omitempty), not serialize as
// [] — an empty integrations block is a system message the backend would otherwise
// render for nothing, on the cached side of the prompt.
func TestBuildMCPServersOmittedFromWireWhenEmpty(t *testing.T) {
	rc := backend.RuntimeContext{PermissionTier: "system", MCPServers: buildMCPServers(nil)}
	encoded, err := json.Marshal(rc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "mcp_servers") {
		t.Errorf("empty integration surface still reached the wire: %s", encoded)
	}
}

// THE invariant this design turns on: the backend renders mcp_servers as a session-stable
// SYSTEM block cached ahead of ~18k tokens of tool schemas, so its bytes must not move
// when live MCP state does. Two rounds whose only difference is connectivity must produce
// a byte-identical block — while runtime.mcp, which rides the volatile user-role tail,
// tracks the change. Pinned on the assembled request (not just the mapper) so putting
// "connected"/transport/tool-count into the description later fails here.
func TestRuntimeMCPServersStableAcrossLiveStateChange(t *testing.T) {
	servers := []prompts.MCPServerContext{
		{Name: "daintree", URL: "http://127.0.0.1:45454/mcp", Description: "Daintree control plane"},
		{Name: "daintree-docs", URL: "https://daintree.org/api/mcp", Description: "docs"},
	}
	cold := prompts.MainPromptContext{MCPServers: servers, MCPConnected: false, MCPStatusLine: "not connected — no url/token"}
	warm := prompts.MainPromptContext{
		MCPServers: servers, MCPConnected: true, MCPTransport: "streamable-http",
		MCPToolCount: intPtr(148), MCPStatusLine: "connected (streamable-http, 148 tools)",
	}

	s := &Session{}
	before, err := json.Marshal(s.buildRuntimeContext(cold, nil).MCPServers)
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(s.buildRuntimeContext(warm, nil).MCPServers)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("cached integration block moved with live state:\n before: %s\n after:  %s", before, after)
	}
	// Sanity: the volatile half DID change, so the test above is not passing because
	// nothing was wired up at all.
	coldMCP, warmMCP := s.buildRuntimeContext(cold, nil).MCP, s.buildRuntimeContext(warm, nil).MCP
	if coldMCP == nil || warmMCP == nil || coldMCP.Connected == warmMCP.Connected {
		t.Fatalf("runtime.mcp did not track the live change: %+v vs %+v", coldMCP, warmMCP)
	}
}

func intPtr(i int) *int { return &i }

// "Was it even pointed at the Daintree the user meant?" has to be answerable from a
// session log alone — that is the repo's core debugging loop — so the endpoints the
// model was shown ride the backend.respond.request trace.
func TestTraceRecordsMCPEndpoints(t *testing.T) {
	cap := &traceCapture{}
	deps := baseDeps(&fakeRouter{results: []models.ChatResult{{Content: "done"}}}, &fakeTools{})
	deps.Trace = cap.record
	deps.PromptContext = prompts.MainPromptContext{
		MCPServers: []prompts.MCPServerContext{
			{Name: "daintree", URL: "http://127.0.0.1:45454/mcp", Description: "Daintree control plane"},
		},
	}
	if _, err := NewSession(deps).Send(context.Background(), "hello", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	ev, ok := cap.first("backend.respond.request")
	if !ok {
		t.Fatal("missing backend.respond.request")
	}
	runtime, _ := ev.fields["runtime"].(map[string]any)
	servers, _ := runtime["mcpServers"].([]string)
	if len(servers) != 1 || !strings.Contains(servers[0], "http://127.0.0.1:45454/mcp") {
		t.Errorf("mcpServers trace = %+v", runtime["mcpServers"])
	}
}
