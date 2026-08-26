package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/tools"
)

// The prompt context must name the endpoints this process is wired to, so the model can
// answer "which Daintree are you connected to?" instead of guessing at a plausible
// localhost URL (ses_8cb40b4e). Credentials must not ride along: Daintree's per-session
// MCP URL can carry its bearer as a query param, so the endpoint is sanitized before it
// can reach the backend, the model, or a debug log.
func TestPromptContextCarriesSanitizedMCPEndpoints(t *testing.T) {
	dir := t.TempDir()
	mcpURL := "http://127.0.0.1:45454/mcp?session=super-secret-token"
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    &dir,
			ProjectPath: &dir,
			Tier:        strPtr("operator"),
			McpURL:      &mcpURL,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	servers := a.PromptContext().MCPServers
	if len(servers) == 0 {
		t.Fatal("no MCP servers reported — the model cannot name its endpoint")
	}
	var primary bool
	for _, s := range servers {
		if strings.Contains(s.URL, "super-secret-token") {
			t.Fatalf("credential survived into the prompt context: %q", s.URL)
		}
		if s.Name == "daintree" {
			primary = true
			if s.URL != "http://127.0.0.1:45454/mcp" {
				t.Errorf("primary endpoint = %q", s.URL)
			}
		}
		if s.Description == "" {
			t.Errorf("%s has no role description", s.Name)
		}
	}
	if !primary {
		t.Errorf("want the daintree MCP server listed, got %+v", servers)
	}
}

// The tool surfaces are the on-demand answer to "which URL are you on?", and they read
// the endpoint through the app's adapters — the layer that sanitizes. Exercised through
// the REAL wired registry (not a fake status), so deleting either adapter's
// mcp.SanitizeURL call fails here, and asserted on the SERIALIZED result, since that is
// what actually reaches the model.
func TestEndpointToolsNeverLeakTheMCPToken(t *testing.T) {
	dir := t.TempDir()
	mcpURL := "http://127.0.0.1:45454/mcp?session=super-secret-token"
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir,
			Tier: strPtr("operator"), McpURL: &mcpURL,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	for _, name := range []string{"daintree.status", "context.snapshot"} {
		tool := a.Registry.Get(name)
		if tool == nil {
			t.Fatalf("%s is not registered", name)
		}
		res := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
		if !res.Ok {
			t.Fatalf("%s must not fail on a down link: %+v", name, res.Error)
		}
		encoded, merr := json.Marshal(map[string]any{"summary": res.Summary, "result": res.Result})
		if merr != nil {
			t.Fatal(merr)
		}
		if strings.Contains(string(encoded), "super-secret-token") {
			t.Errorf("%s leaked the session token: %s", name, encoded)
		}
		if !strings.Contains(string(encoded), "http://127.0.0.1:45454/mcp") {
			t.Errorf("%s dropped the endpoint entirely: %s", name, encoded)
		}
	}
}

// A credential-bearing endpoint must not survive ANY of the shapes it can arrive in.
// url.Parse's error path leaves userinfo intact, so an endpoint that fails to parse is
// dropped whole rather than published half-stripped.
func TestPromptContextRejectsUnstrippableEndpoint(t *testing.T) {
	dir := t.TempDir()
	// A space in the host makes this unparseable; the userinfo would otherwise ride along.
	mcpURL := "http://user:tok@127.0.0.1 45454/mcp"
	a, err := Create(CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir,
			Tier: strPtr("operator"), McpURL: &mcpURL,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	for _, s := range a.PromptContext().MCPServers {
		if strings.Contains(s.URL, "tok") || s.Name == "daintree" {
			t.Errorf("unstrippable endpoint published: %+v", s)
		}
	}
}

// The assistant backend URL is model-visible through context.snapshot, so it must be
// sanitized on the way to the model exactly like an MCP endpoint.
//
// This is DEFENCE IN DEPTH now, and the comment that used to sit here said the opposite:
// it claimed DAINTREE_BACKEND_URL was taken as given with no normalisation step, which
// was true and is the hole this test was written around. Startup runs every endpoint
// source through backend.NormalizeBaseURL, so a userinfo endpoint is refused at load and
// can no longer reach the config at all — asserted first, below. The sanitizing still has
// to hold, because the value reaching the tool is whatever the LIVE client reports, and a
// client can be replaced at runtime; the swap below stands in for that.
func TestSnapshotToolSanitizesBackendURL(t *testing.T) {
	const secret = "supersecret"
	const withUserinfo = "https://user:" + secret + "@backend.example"

	// Half one: it cannot get in through the front door any more.
	t.Setenv("DAINTREE_BACKEND_URL", withUserinfo)
	dir := t.TempDir()
	if _, err := Create(CreateOptions{Overrides: config.ConfigOverrides{
		Offline: boolPtr(true), StateDir: &dir, ProjectPath: &dir, Tier: strPtr("operator"),
	}}); err == nil {
		t.Fatal("a backend endpoint carrying userinfo must be refused at startup — Go would send it as an Authorization header on every request")
	} else if strings.Contains(err.Error(), secret) {
		t.Fatalf("the startup refusal echoed the password back: %v", err)
	}

	// Half two: and if one ever reaches the live client regardless, the model never sees it.
	t.Setenv("DAINTREE_BACKEND_URL", "")
	t.Setenv("DAINTREE_API_KEY", "sk-test-key")
	a := newOfflineApp(t)
	defer a.Shutdown()
	sw, ok := a.Backend.(*backend.Swappable)
	if !ok {
		t.Fatal("App.Backend is not a Swappable — the wiring invariant App.Create establishes")
	}
	sw.Swap(backend.NewClient(backend.ClientConfig{BaseURL: withUserinfo}))

	tool := a.Registry.Get("context.snapshot")
	if tool == nil {
		t.Fatal("context.snapshot is not registered")
	}
	res := tool.Handle(context.Background(), json.RawMessage(`{}`), &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("snapshot must never fail: %+v", res.Error)
	}
	if strings.Contains(res.Summary, secret) {
		t.Errorf("backend credential reached the model: %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "https://backend.example") {
		t.Errorf("sanitized backend endpoint missing from summary: %q", res.Summary)
	}
}

// With no Daintree in the environment there is no endpoint to report; an entry with a
// blank URL would be worse than none (the model would read it as "connected to nothing
// in particular").
func TestPromptContextOmitsUnconfiguredMCPEndpoint(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	servers := a.PromptContext().MCPServers
	for _, s := range servers {
		if s.URL == "" {
			t.Errorf("blank endpoint reported for %q", s.Name)
		}
		if s.Name == "daintree" {
			t.Errorf("unconfigured primary MCP must not be listed, got %+v", s)
		}
	}
	// The primary control plane is the ONLY MCP this process is wired to now (the docs
	// client left with issue #332), so an unconfigured Daintree means an empty block —
	// not a fabricated entry with a blank endpoint, which is what the loop above guards.
	if len(servers) != 0 {
		t.Errorf("no MCP is configured, so the block must be empty, got %+v", servers)
	}
}
