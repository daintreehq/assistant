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

// DocsToolNames is the docs MCP's three live read tools (search, get_page,
// get_related_pages). It serves two purposes, both scoped to that ONE server:
//
//  1. Options.DriftBaseline — the docs client checks drift against ITS OWN surface
//     and never false-warns about the Daintree control-plane tools.
//  2. Options.ReadOnlyFallback — the docs MCP is a third-party endpoint that is not
//     guaranteed to send MCP `annotations`, and without them its pure documentation
//     reads would silently lose their retry budget. The fallback applies ONLY to a
//     tool that advertised NO annotations at all, so if the server ever does start
//     annotating, its own declaration wins immediately and this list stops mattering.
//
// This stays a hand-written list because these three names are a fixed, public
// product surface with no annotations to derive from — unlike the Daintree host,
// whose surface is now read live from the annotations it already ships.
var DocsToolNames = []string{
	"search",
	"get_page",
	"get_related_pages",
}
