package mcp

import (
	"math"

	"github.com/daintreehq/assistant/internal/domain"
)

// maxHandbackMessageRunes bounds the handback message locally. Daintree already
// caps it at 500 characters; this is the belt to that braces, so a host that ever
// stopped capping cannot pour an agent-authored wall of text into a verdict
// summary, an inbox event and a tool result at once. Generous on purpose — it
// must never clip a message Daintree considered whole.
const maxHandbackMessageRunes = 1000

// TerminalHandback reads the `lastHandback` object off ONE terminal.getStatus
// entry (the caller has already resolved the entry from structuredContent or the
// JSON text body, so this sees a plain decoded map either way). It is the single
// parser behind every status consumer — the watcher's read model and the shared
// app adapter that feeds terminal.awaitAll and the async coordinator — so the
// three cannot disagree about what counts as a handback.
//
// Defensive by contract: absent, null, a non-object, or an object without a
// usable observedAt all yield nil, which every consumer treats exactly like a
// host that never sent the field. observedAt is the one REQUIRED member because
// freshness is decided on it; a handback that cannot be placed in time cannot be
// matched to a prompt, and an unmatched handback must not complete anything.
// message may legitimately be null (a bare marker) — that is still a handback.
func TerminalHandback(entry map[string]any) *domain.TerminalHandback {
	raw, ok := entry["lastHandback"].(map[string]any)
	if !ok {
		return nil
	}
	at, ok := raw["observedAt"].(float64)
	if !ok || math.IsNaN(at) || math.IsInf(at, 0) || at <= 0 || at != math.Trunc(at) || at > math.MaxInt64/2 {
		return nil
	}
	h := &domain.TerminalHandback{ObservedAt: int64(at)}
	if msg, ok := raw["message"].(string); ok {
		if r := []rune(msg); len(r) > maxHandbackMessageRunes {
			msg = string(r[:maxHandbackMessageRunes])
			h.Truncated = true
		}
		h.Message = &msg
	}
	if tok, ok := raw["submissionToken"].(string); ok {
		h.SubmissionToken = tok
	}
	if t, ok := raw["truncated"].(bool); ok && t {
		h.Truncated = true
	}
	return h
}
