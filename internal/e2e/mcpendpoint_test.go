package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/config"
)

func boolPtr(b bool) *bool { return &b }

// A real turn must put the MCP endpoints on the wire, in runtime.mcp_servers — the
// block the backend renders as inert data ahead of the tool schemas. That is what makes
// "which URL are you connected to?" answerable with no tool call at all; before it, the
// assistant could only guess at a plausible localhost URL (ses_8cb40b4e).
//
// The token-bearing query must NOT survive anywhere in the body: Daintree's per-session
// MCP URL can carry its bearer as ?session=<token>, and this block is rendered into the
// prompt the model can quote back.
func TestRespondRequestCarriesMCPEndpoints(t *testing.T) {
	fake := newFakeBackend(t, sseRound{contentTokens: []string{"ok"}})
	t.Setenv("DAINTREE_BACKEND_URL", fake.baseURL())
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	mcpURL := "http://127.0.0.1:45454/mcp?session=super-secret-token"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
			McpURL:      &mcpURL,
			// Offline, because the first Send joins ensureStartupForTurn → ConnectMcp,
			// which fires a real handshake at the PUBLIC docs MCP (and, with an inherited
			// DAINTREE_MCP_TOKEN, at whatever is listening on 45454). Offline short-circuits
			// mcp.Client.Connect and leaves the backend client untouched, so the turn still
			// runs against the fake — and a configured-but-never-connected endpoint is
			// exactly the case worth pinning: the endpoint is reported either way.
			Offline: boolPtr(true),
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	if _, serr := a.Session.Send(context.Background(), "which MCP are you on?", agent.SendOptions{}); serr != nil {
		t.Fatalf("Send returned error: %v", serr)
	}

	body := fake.request(0)
	if body == nil {
		t.Fatal("no respond request captured")
	}
	runtime, _ := body["runtime"].(map[string]any)
	servers, _ := runtime["mcp_servers"].([]any)
	if len(servers) == 0 {
		t.Fatalf("runtime.mcp_servers missing from the request: %+v", runtime)
	}
	var primary, docs bool
	for _, s := range servers {
		entry, _ := s.(map[string]any)
		desc, _ := entry["description"].(string)
		switch entry["name"] {
		case "daintree":
			primary = strings.Contains(desc, "http://127.0.0.1:45454/mcp")
		case "daintree-docs":
			docs = strings.Contains(desc, "://")
		}
		// The wire entry must stay endpoint-only. Anything else here (transport, status,
		// tool_count) changes mid-session and would re-encode the ~18k tool schemas
		// cached behind this block on every flap.
		for k := range entry {
			if k != "name" && k != "description" {
				t.Errorf("volatile field %q on the cached integration block: %+v", k, entry)
			}
		}
	}
	if !primary {
		t.Errorf("no daintree entry naming the endpoint: %+v", servers)
	}
	if !docs {
		t.Errorf("docs MCP missing from the integration surface: %+v", servers)
	}
	if strings.Contains(mustJSON(body), "super-secret-token") {
		t.Error("the MCP session token reached the backend request body")
	}
}
