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
// cache warms (getContext advertised), and the runtime context the assistant ships to
// the backend reflects a healthy MCP (no "degraded local mode" wording). This exercises
// the fake MCP fixture end-to-end at the transport level — not an interface stub.
//
// The migration replaced the cached message[1] runtime-context prompt with structured
// data assembled per round from a.PromptContext() (rendered into backend.RuntimeContext
// by the session). So the post-connect runtime status is asserted on a.PromptContext()
// — the live builder that feeds every backend round — rather than a baked message.
func TestConnectsToFakeMCP(t *testing.T) {
	mcp := newFakeMCP(t)
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	url := mcp.url()
	// Point the SECOND (docs) MCP at the same fake so ConnectMcp's parallel docs
	// handshake stays on the loopback httptest server and never reaches the real
	// daintree.org. This test only asserts the PRIMARY link; the docs connect is inert.
	t.Setenv("DAINTREE_DOCS_MCP_URL", url)
	token := "fake-token"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
			McpURL:      &url,
			McpToken:    &token,
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

	// The runtime-context builder reflects the live connect: with MCP healthy it reports
	// connected (and a "connected (...)" status line) rather than the degraded-local-mode
	// caveat. a.PromptContext() reads the live MCP status, so this is the same data the
	// session renders into the backend request's RuntimeContext on the next round.
	pc := a.PromptContext()
	if !pc.MCPConnected {
		t.Errorf("runtime context does not reflect a connected MCP: %+v", pc)
	}
	if !strings.Contains(pc.MCPStatusLine, "connected") {
		t.Errorf("runtime MCP status line does not report a healthy connection: %q", pc.MCPStatusLine)
	}
}
