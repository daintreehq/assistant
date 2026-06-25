// Package artifactx is the oversized-result paging tool family (artifact.read,
// risk "read"). When a serialized tool result overflows the inline size limit the
// agent loop stashes the full JSON envelope in the session's artifact store and
// hands the model a compact stub carrying an artifactId; this tool pages back
// through that full output by CHARACTER range. The store is an in-memory hot cache
// backed by a durable SQLite table, so an id survives the cache's eviction and a
// process restart; only a genuinely pruned/unknown id misses, and it fails
// gracefully rather than crashing.
package artifactx

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// ArtifactStore is the oversized-result store this family reads. Get returns
// (value, true) when an id resolves — from the session's in-memory hot cache or, on
// a cache miss, the durable artifacts table — and (_, false) when it was never
// stored or has been pruned. Nil store ⇒ unavailable (handled in the handler via
// Deps.Store == nil). Replaces TS `ctx.artifactStore: Map<string,string>`.
type ArtifactStore interface {
	Get(id string) (string, bool)
}

// Deps wires the family to its session-scoped store. Store may be nil (tests, or
// a non-session context) → the handler returns ARTIFACT_UNAVAILABLE.
type Deps struct {
	Store ArtifactStore
}

const (
	// defaultReadChars / maxReadChars bound one read so its own result can't
	// re-overflow MAX_TOOL_RESULT_CHARS (8000): the stored artifact is already
	// escaped JSON, so re-escaping a slice ≈ 2× — 3500 chars ⇒ ≤ ~7200 worst-case.
	defaultReadChars = 3500
	maxReadChars     = 3500
)

const (
	codeArtifactUnavailable = "ARTIFACT_UNAVAILABLE"
	codeArtifactNotFound    = "ARTIFACT_NOT_FOUND"
)

type readArgs struct {
	ArtifactID string `json:"artifactId"`
	Offset     *int   `json:"offset,omitempty"`
	Limit      *int   `json:"limit,omitempty"`
}

// Validate enforces `offset: int().min(0)` and `limit: int().min(1).max(MAX)`.
// Without the limit floor a negative limit makes end = offset+limit < offset, and
// runes[offset:end] panics with an invalid slice range.
func (a *readArgs) Validate() error {
	if a.Offset != nil && *a.Offset < 0 {
		return fmt.Errorf("offset must be >= 0")
	}
	if a.Limit != nil && (*a.Limit < 1 || *a.Limit > maxReadChars) {
		return fmt.Errorf("limit must be between 1 and %d", maxReadChars)
	}
	return nil
}

var readSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "artifactId": { "type": "string", "description": "The artifactId from a truncated tool result." },
    "offset": { "type": "number", "description": "Character offset to start reading from (default 0)." },
    "limit": { "type": "number", "description": "Max characters to return (default 3500, max 3500)." }
  },
  "required": ["artifactId"]
}`)

// Tools returns the artifact family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{{
		Name: "artifact.read",
		Description: "Read a slice of a large tool result that was stored as an artifact because it overflowed the inline size limit. " +
			"Pass the artifactId from a truncated result, plus an optional character offset and limit, to page through the full output. " +
			"Use the returned nextOffset/eof to continue.",
		Risk:   domain.RiskRead,
		Schema: readSchema,
		Decode: tools.StrictDecoder(func() any { return &readArgs{} }),
		Handle: func(_ context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return handle(deps, raw)
		},
	}}
}

func handle(deps Deps, raw json.RawMessage) tools.ToolResult {
	var a readArgs
	_ = json.Unmarshal(raw, &a)

	if deps.Store == nil {
		return tools.Fail(codeArtifactUnavailable,
			"Artifact storage is not available in this context, so the full output cannot be retrieved.",
			tools.Unrecoverable())
	}
	full, found := deps.Store.Get(a.ArtifactID)
	if !found {
		return tools.Fail(codeArtifactNotFound,
			fmt.Sprintf("No artifact found with id %q. It was never stored or has been pruned by retention.", a.ArtifactID),
			tools.Unrecoverable())
	}

	// CHARACTER (rune) slicing, not byte slicing — the model reasons about JS
	// string indices. The stored artifact is JSON text (ASCII-heavy), so the
	// UTF-16-vs-rune divergence is acceptable; runes are the faithful single-unit.
	runes := []rune(full)
	totalChars := len(runes)

	offset := 0
	if a.Offset != nil {
		offset = *a.Offset
	}
	// Clamp into [0,totalChars] so a past-the-end read returns empty-at-eof.
	if offset < 0 {
		offset = 0
	}
	if offset > totalChars {
		offset = totalChars
	}
	limit := defaultReadChars
	if a.Limit != nil {
		limit = *a.Limit
	}
	if limit > maxReadChars {
		limit = maxReadChars
	}
	end := offset + limit
	if end > totalChars {
		end = totalChars
	}
	content := string(runes[offset:end])
	nextOffset := offset + (end - offset)
	eof := nextOffset >= totalChars

	tail := fmt.Sprintf(", %d remaining", totalChars-nextOffset)
	if eof {
		tail = ", end of artifact"
	}
	return tools.Ok(
		fmt.Sprintf("Read %d of %d chars from %s (offset %d%s).", end-offset, totalChars, a.ArtifactID, offset, tail),
		map[string]any{
			"artifactId": a.ArtifactID,
			"offset":     offset,
			"limit":      limit,
			"totalChars": totalChars,
			"content":    content,
			"nextOffset": nextOffset,
			"eof":        eof,
		})
}
