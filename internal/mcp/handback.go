package mcp

import (
	"math"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/redact"
)

// maxHandbackMessageRunes bounds the handback message locally. Daintree already
// caps it at 500 characters; this is the belt to that braces, so a host that ever
// stopped capping cannot pour an agent-authored wall of text into a verdict
// summary, an inbox event and a tool result at once. Generous on purpose — it
// must never clip a message Daintree considered whole.
const maxHandbackMessageRunes = 1000

// maxHandbackTokenBytes is the same bound SubmissionReceipt holds a submission
// token to. A token is a correlation key, so an oversized one is REJECTED, never
// truncated — a clipped token could match a prompt it was not issued for.
const maxHandbackTokenBytes = 128

// TerminalHandback reads the `lastHandback` object off ONE terminal.getStatus
// entry (the caller has already resolved the entry from structuredContent or the
// JSON text body, so this sees a plain decoded map either way). It is the single
// parser behind every status consumer — the watcher's read model and the shared
// app adapter that feeds terminal.awaitAll and the async coordinator — so the
// three cannot disagree about what counts as a handback.
//
// Strict by contract, because a handback can complete a watcher: absent, null, a
// non-object, or an object with ANY malformed member yields nil, which every
// consumer treats exactly like a host that never sent the field. A present
// member of the wrong type is schema drift, not a bare marker — guessing at it
// would turn a garbled object into completion evidence. observedAt is REQUIRED
// (freshness is decided on it; a handback that cannot be placed in time cannot be
// matched to a prompt). message may legitimately be null or absent (a bare
// marker) — that is still a handback.
//
// The message is redacted HERE, at ingestion, not at the log boundary. Every
// surface renders it through domain.HandbackReport, whose strconv.Quote escaping
// turns `"password":"x"` into `\"password\":\"x\"` — a shape the write-boundary
// redactor's patterns no longer match. Masking the raw text first means the
// audit rows, the debug log, the inbox event AND the model all see the same
// already-clean summary, and an agent that pasted a credential into its one-line
// summary has not just published it to all four.
func TerminalHandback(entry map[string]any) *domain.TerminalHandback {
	raw, ok := entry["lastHandback"].(map[string]any)
	if !ok {
		return nil
	}
	at, ok := raw["observedAt"].(float64)
	if !ok || math.IsNaN(at) || math.IsInf(at, 0) || at <= 0 || at != math.Trunc(at) || at > 1<<53 {
		return nil
	}
	h := &domain.TerminalHandback{ObservedAt: int64(at)}

	switch msg := raw["message"].(type) {
	case nil:
		// absent or null: a bare marker
	case string:
		if r := []rune(msg); len(r) > maxHandbackMessageRunes {
			msg = string(r[:maxHandbackMessageRunes])
			h.Truncated = true
		}
		msg = redact.String(msg)
		h.Message = &msg
	default:
		return nil
	}

	switch tok := raw["submissionToken"].(type) {
	case nil:
	case string:
		if len(tok) > maxHandbackTokenBytes {
			return nil
		}
		h.SubmissionToken = tok
	default:
		return nil
	}

	switch t := raw["truncated"].(type) {
	case nil:
	case bool:
		h.Truncated = h.Truncated || t
	default:
		return nil
	}
	return h
}
