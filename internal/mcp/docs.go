package mcp

// docs.go holds the constants for the SECOND MCP server this client can target: the
// public Daintree documentation MCP. Unlike the primary Daintree control-plane MCP
// (per-session URL + bearer token injected by Daintree), the docs server is a fixed,
// no-auth, stateless HTTP endpoint that answers "how do I use Daintree" questions via
// live documentation search. A docs Client is constructed with
// Options{URL: &DefaultDocsURL, Anonymous: true, DriftBaseline: DocsDocumentedToolNames}.

// DefaultDocsURL is the public Daintree documentation MCP endpoint. It is a fixed
// product URL, NOT read from the Daintree env (DAINTREE_MCP_URL is the LOCAL
// control-plane MCP). The dev/test override is DAINTREE_DOCS_MCP_URL, resolved by the
// app — mirroring the DAINTREE_BACKEND_URL hardcoded-with-escape-hatch pattern.
const DefaultDocsURL = "https://daintree.org/api/mcp"

// DocsDocumentedToolNames is the drift baseline for the docs MCP — its three live
// read tools (search, get_page, get_related_pages). It is passed as
// Options.DriftBaseline so the docs client checks drift against ITS OWN surface and
// never false-warns that the 60 Daintree control-plane tools (DocumentedMcpToolNames)
// are "missing" — that baseline belongs to the primary client alone. Keep in sync with
// the live server.
var DocsDocumentedToolNames = []string{
	"search",
	"get_page",
	"get_related_pages",
}
