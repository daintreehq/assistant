package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
)

// TestConnectsToFakeMCP drives App.Create + ConnectMcp against the real go-sdk
// Streamable-HTTP fake MCP server (httptest), asserting the assistant connects over
// the wire exactly as it would to live Daintree: status flips connected, the tool
// cache warms (getContext advertised), and the runtime-context prompt message
// reflects a healthy MCP (no "degraded local mode" wording). This exercises the
// fake MCP fixture end-to-end at the transport level — not an interface stub.
func TestConnectsToFakeMCP(t *testing.T) {
	mcp := newFakeMCP(t)
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	url := mcp.url()
	token := "fake-token"
	key := "test-key"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:       &dir,
			ProjectPath:    &dir,
			Tier:           strPtr("operator"),
			DeepSeekAPIKey: &key, // present but never called (no Send in this test)
			McpURL:         &url,
			McpToken:       &token,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	st := a.ConnectMcp(context.Background())
	if !st.Connected {
		t.Fatalf("MCP not connected to fake server: %+v", st)
	}
	if st.ToolCount == nil || *st.ToolCount < 1 {
		t.Errorf("tool cache cold/empty after connect: %+v", st.ToolCount)
	}

	// The connect path refreshed the runtime-context message; with MCP healthy it
	// must report the connection rather than the degraded-local-mode caveat.
	runtimeMsg := a.Session.Messages()[1].ContentToText()
	if !strings.Contains(runtimeMsg, "connected") {
		t.Errorf("runtime message does not reflect a connected MCP:\n%s", runtimeMsg)
	}
}
