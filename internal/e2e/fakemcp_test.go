package e2e

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeMCP is a real Daintree-shaped MCP server over the go-sdk's Streamable HTTP
// handler, served on httptest. The assistant's mcp.Client connects to it exactly as
// it would the live Daintree server (Streamable HTTP + Bearer token), so this is a
// genuine transport-level fake — not an interface stub. It advertises a single
// read-only `getContext` tool returning a fixed project name, enough for
// daintree.status / doctor to round-trip.
type fakeMCP struct {
	srv *httptest.Server
}

// newFakeMCP starts the fake MCP server. The returned URL is the value to feed
// DAINTREE_MCP_URL (a /mcp endpoint).
func newFakeMCP(t *testing.T) *fakeMCP {
	t.Helper()
	server := sdkmcp.NewServer(&sdkmcp.Implementation{Name: "fake-daintree", Version: "v0.0.1"}, nil)

	// getContext: the project-identity read the assistant probes at boot / in doctor.
	type getContextIn struct{}
	type getContextOut struct {
		ProjectName string `json:"projectName"`
	}
	sdkmcp.AddTool(server, &sdkmcp.Tool{
		Name:        "getContext",
		Description: "Return the bound project context.",
	}, func(ctx context.Context, req *sdkmcp.CallToolRequest, in getContextIn) (*sdkmcp.CallToolResult, getContextOut, error) {
		out := getContextOut{ProjectName: "fake-project"}
		return &sdkmcp.CallToolResult{
			Content: []sdkmcp.Content{&sdkmcp.TextContent{Text: `{"projectName":"fake-project"}`}},
		}, out, nil
	})

	handler := sdkmcp.NewStreamableHTTPHandler(func(r *http.Request) *sdkmcp.Server {
		return server
	}, &sdkmcp.StreamableHTTPOptions{})
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &fakeMCP{srv: srv}
}

// url is the /mcp endpoint to feed DAINTREE_MCP_URL.
func (m *fakeMCP) url() string { return m.srv.URL + "/mcp" }
