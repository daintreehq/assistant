package mcp

import (
	"context"
	"net/http"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
)

// newInjectedDocs builds a docs-style Client (URL override + anonymous + the docs drift
// baseline) over an injected low-level client, so the parameterization is exercised
// without any network.
func newInjectedDocs(low LowLevelClient) *Client {
	url := DefaultDocsURL
	return New(config.AppConfig{}, Options{
		URL:            &url,
		Anonymous:      true,
		DriftBaseline:  DocsDocumentedToolNames,
		ClientOverride: low,
	})
}

// A docs client checks drift against ITS OWN three tools, never the 60 Daintree
// control-plane names. A live server advertising only `search` drifts on the two missing
// docs tools — and crucially does NOT warn that any Daintree tool is missing.
func TestDocsClientDriftUsesDocsBaseline(t *testing.T) {
	low := &fakeLow{tools: []rawTool{{Name: "search"}}}
	c := newInjectedDocs(low)
	c.Connect(context.Background())
	st := c.Status()
	if len(st.DriftToolNames) != 2 {
		t.Fatalf("want 2 missing docs tools, got %v", st.DriftToolNames)
	}
	for _, n := range st.DriftToolNames {
		if n != "get_page" && n != "get_related_pages" {
			t.Fatalf("unexpected drift name %q — baseline leaked from the Daintree set", n)
		}
	}
}

// All three docs tools present → no drift at all.
func TestDocsClientNoDriftWhenComplete(t *testing.T) {
	all := make([]rawTool, len(DocsDocumentedToolNames))
	for i, n := range DocsDocumentedToolNames {
		all[i] = rawTool{Name: n}
	}
	c := newInjectedDocs(&fakeLow{tools: all})
	c.Connect(context.Background())
	if st := c.Status(); st.DriftWarnings != nil {
		t.Fatalf("complete docs surface must not drift, got %v", st.DriftWarnings)
	}
}

// New resolves the endpoint from Options.URL (a second server) instead of cfg.McpURL,
// and Status reports that ACTUAL endpoint (not the empty cfg.McpURL) so diagnostics on
// the docs client are honest.
func TestDocsClientURLOverride(t *testing.T) {
	c := newInjectedDocs(&fakeLow{})
	if c.endpoint != DefaultDocsURL {
		t.Fatalf("endpoint = %q, want %q", c.endpoint, DefaultDocsURL)
	}
	if got := c.Status().URL; got != DefaultDocsURL {
		t.Fatalf("Status.URL = %q, want the resolved endpoint %q", got, DefaultDocsURL)
	}
}

// A nil DriftBaseline falls back to the full Daintree baseline (the primary client's
// behavior is unchanged by the parameterization).
func TestNilDriftBaselineDefaultsToDaintree(t *testing.T) {
	c := New(config.AppConfig{McpURL: "http://x/mcp", McpToken: "t"}, Options{ClientOverride: &fakeLow{}})
	if len(c.driftBaseline) != len(DocumentedMcpToolNames) {
		t.Fatalf("default baseline len = %d, want %d", len(c.driftBaseline), len(DocumentedMcpToolNames))
	}
}

// httpClientFor returns a plain (no-bearer) client for an anonymous server and a
// bearer-injecting one otherwise — the no-auth docs MCP must not send "Bearer ".
func TestHTTPClientForAnonymous(t *testing.T) {
	plain := httpClientFor("")
	if plain.Transport != nil {
		t.Fatalf("anonymous client must use the default transport (no bearer round-tripper), got %T", plain.Transport)
	}
	authed := httpClientFor("tok")
	if _, ok := authed.Transport.(*bearerRoundTripper); !ok {
		t.Fatalf("tokened client must wrap a bearerRoundTripper, got %T", authed.Transport)
	}
	// Sanity: the wrapped base is the shared default transport.
	if rt, _ := authed.Transport.(*bearerRoundTripper); rt != nil && rt.base != http.DefaultTransport {
		t.Fatal("bearer round-tripper should wrap http.DefaultTransport")
	}
}
