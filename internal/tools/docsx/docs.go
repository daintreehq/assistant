package docsx

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// Per-tool topK ceilings the docs server enforces; we clamp to them so a too-large value
// is normalized rather than bounced server-side.
const (
	searchMaxTopK  = 100
	relatedMaxTopK = 25
)

// clampTopK normalizes a model-supplied topK: <=0 → 0 (caller omits it so the server
// applies its default), and anything over max is clamped to max (the JSON-schema max is
// only model-facing; StrictDecoder does not enforce numeric refinements, so we enforce
// here too — belt and suspenders).
func clampTopK(v, max int) int {
	if v <= 0 {
		return 0
	}
	if v > max {
		return max
	}
	return v
}

// Tools returns the Daintree documentation family: docs.search (primary), docs.getPage,
// and docs.getRelatedPages. All are risk read — answering a help question never mutates
// Daintree, so they run with no confirmation and at every tier. Each wraps the
// same-purpose tool on the docs MCP server (search / get_page / get_related_pages).
func Tools(deps Deps) []*tools.Tool {
	return []*tools.Tool{
		newSearchTool(deps),
		newGetPageTool(deps),
		newGetRelatedPagesTool(deps),
	}
}

// --- docs.search ---

// searchArgs is the typed shape for docs.search. query is required; topK and pathPrefix
// are optional filters. The other server-side knobs (tags/filters/groupBy/scope) are
// intentionally omitted — they're rarely useful for help answers and keep the surface
// the model has to reason about minimal.
type searchArgs struct {
	Query      string `json:"query"`
	TopK       int    `json:"topK,omitempty"`
	PathPrefix string `json:"pathPrefix,omitempty"`
}

var searchSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["query"],
  "properties": {
    "query": { "type": "string", "minLength": 1, "description": "What to search the Daintree documentation for. Keywords or natural language (e.g. \"create a worktree from a PR\", \"keybindings\")." },
    "topK": { "type": "integer", "minimum": 1, "maximum": 100, "description": "Max number of results to return (default 10)." },
    "pathPrefix": { "type": "string", "description": "Optional: restrict results to URL paths under this prefix (e.g. \"/docs\")." }
  }
}`)

func newSearchTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name: "docs.search",
		Description: "Search Daintree's live documentation to answer a user's how-to or help question " +
			"(\"how do I…\", \"what is…\", \"how do I configure…\"). Returns ranked doc sections, each with a " +
			"title, content, and a page PATH (e.g. /docs/worktrees). This is the FIRST tool to reach for when " +
			"the user asks how to use Daintree — prefer it over guessing. Cite the pages you use as full URLs " +
			"by prepending https://daintree.org to the returned path. " +
			"PARALLEL: docs search/getPage/getRelatedPages calls batched in ONE reply run concurrently — emit several at once when researching.",
		Risk: domain.RiskRead,
		// Independent documentation read over the docs MCP with no ordering dependency on
		// its siblings: a burst of research reads overlaps its network round-trips (up to
		// the docs client's in-flight cap) instead of serializing. See terminal.extract.
		Parallelizable: true,
		Schema:         searchSchema,
		Decode:         tools.StrictDecoder(func() any { return &searchArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a searchArgs
			if res, ok := strictDecode(args, "docs.search", &a); !ok {
				return res
			}
			query := strings.TrimSpace(a.Query)
			if query == "" {
				return tools.Fail(codeInvalidArgs, "docs.search: query is required")
			}
			fwd := map[string]any{"query": query}
			// Clamp to the server's max rather than reject — a too-large topK would be
			// refused server-side, and <=0 means "use the default" (omit it).
			if k := clampTopK(a.TopK, searchMaxTopK); k > 0 {
				fwd["topK"] = k
			}
			if p := strings.TrimSpace(a.PathPrefix); p != "" {
				fwd["pathPrefix"] = p
			}
			return passthrough(ctx, deps.MCP, "search", fmt.Sprintf("Searched Daintree documentation for: %s", query), fwd)
		},
	}
}

// --- docs.getPage ---

type getPageArgs struct {
	Path string `json:"path"`
}

var getPageSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string", "minLength": 1, "description": "URL path of the documentation page (e.g. \"/docs/worktrees\"). Use a URL returned by docs.search." }
  }
}`)

func newGetPageTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name: "docs.getPage",
		Description: "Fetch the full markdown of a specific Daintree documentation page by its URL path. " +
			"Use AFTER docs.search when the search snippets lack the detail to answer accurately. " +
			"Not for discovery — search first to find the page. " +
			"PARALLEL: docs calls batched in ONE reply run concurrently — to read several pages, emit one getPage each in one batch.",
		Risk: domain.RiskRead,
		// Independent per-page documentation read, no ordering dependency on siblings: a
		// batch of getPage calls overlaps its network round-trips. See terminal.extract.
		Parallelizable: true,
		Schema:         getPageSchema,
		Decode:         tools.StrictDecoder(func() any { return &getPageArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a getPageArgs
			if res, ok := strictDecode(args, "docs.getPage", &a); !ok {
				return res
			}
			path := strings.TrimSpace(a.Path)
			if path == "" {
				return tools.Fail(codeInvalidArgs, "docs.getPage: path is required")
			}
			return passthrough(ctx, deps.MCP, "get_page", fmt.Sprintf("Read Daintree documentation page: %s", path), map[string]any{"path": path})
		},
	}
}

// --- docs.getRelatedPages ---

type getRelatedPagesArgs struct {
	Path string `json:"path"`
	TopK int    `json:"topK,omitempty"`
}

var getRelatedPagesSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string", "minLength": 1, "description": "URL path of the source page (e.g. \"/docs/worktrees\"). Use a URL returned by docs.search." },
    "topK": { "type": "integer", "minimum": 1, "maximum": 25, "description": "Max number of related pages to return (default 10)." }
  }
}`)

func newGetRelatedPagesTool(deps Deps) *tools.Tool {
	return &tools.Tool{
		Name: "docs.getRelatedPages",
		Description: "Find Daintree documentation pages related to a known page, for suggesting further reading. " +
			"Use only when you already have a page URL (from docs.search) and want connected content — not for general search.",
		Risk: domain.RiskRead,
		// Independent related-pages lookup, no ordering dependency on siblings: batched
		// docs reads overlap their network round-trips. See terminal.extract.
		Parallelizable: true,
		Schema:         getRelatedPagesSchema,
		Decode:         tools.StrictDecoder(func() any { return &getRelatedPagesArgs{} }),
		Handle: func(ctx context.Context, args json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a getRelatedPagesArgs
			if res, ok := strictDecode(args, "docs.getRelatedPages", &a); !ok {
				return res
			}
			path := strings.TrimSpace(a.Path)
			if path == "" {
				return tools.Fail(codeInvalidArgs, "docs.getRelatedPages: path is required")
			}
			fwd := map[string]any{"path": path}
			if k := clampTopK(a.TopK, relatedMaxTopK); k > 0 {
				fwd["topK"] = k
			}
			return passthrough(ctx, deps.MCP, "get_related_pages", fmt.Sprintf("Found Daintree documentation pages related to: %s", path), fwd)
		},
	}
}
