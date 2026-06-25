package agent

import (
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// ArtifactStore is the per-session overflow store: oversized serialized tool
// results are stashed here under an artifact_<uuid8> id and surfaced to the model
// via a truncation stub so it can page them back with artifact.read. The in-memory
// map is a bounded hot cache — it keeps INSERTION ORDER so eviction is oldest-first,
// capped at MaxStoredArtifacts (64). When a persister is wired each set() ALSO
// mirrors the payload to durable storage, so a read survives this id's eviction from
// the cache or a process restart (the DB row is never evicted on cache eviction).
type ArtifactStore struct {
	keys      []string          // insertion-ordered ids (for oldest-first eviction)
	data      map[string]string // id → full serialized result (hot cache)
	sessionID string            // provenance stamped on each durable mirror row
	persister ArtifactPersister // durable mirror; nil ⇒ in-memory only
}

// NewArtifactStore builds an empty store. sessionID + persister enable the durable
// mirror (each set() also writes the payload to storage); pass ("", nil) for an
// in-memory-only store (the default in tests).
func NewArtifactStore(sessionID string, persister ArtifactPersister) *ArtifactStore {
	return &ArtifactStore{data: make(map[string]string), sessionID: sessionID, persister: persister}
}

// Len reports the number of stored artifacts.
func (a *ArtifactStore) Len() int { return len(a.keys) }

// Get returns the full serialized result for an id.
func (a *ArtifactStore) Get(id string) (string, bool) {
	v, ok := a.data[id]
	return v, ok
}

// set stores a value under a fresh id, evicting oldest-first while at/over the
// cap (eviction happens before insert). When a persister is wired it ALSO mirrors
// the payload to durable storage under the SAME id, best-effort: a write failure is
// swallowed because the hot-cache entry just stored already satisfies the in-session
// read — the DB is only the fallback for a later read after this id is evicted or
// the process restarts. Note the DB row is intentionally NOT removed on cache
// eviction; that is the whole point — the durable copy outlives the bounded cache.
func (a *ArtifactStore) set(value string) string {
	for len(a.keys) >= domain.MaxStoredArtifacts {
		oldest := a.keys[0]
		a.keys = a.keys[1:]
		delete(a.data, oldest)
	}
	id := domain.NewID(domain.PrefixArtifact)
	a.keys = append(a.keys, id)
	a.data[id] = value
	if a.persister != nil {
		_, _ = a.persister.InsertArtifact(domain.ArtifactRecord{
			ID:         id,
			SessionID:  a.sessionID,
			Content:    value,
			TotalChars: charLen(value),
			TotalBytes: len(value),
		})
	}
	return id
}

// truncationResult is the inner `result` object of an overflow stub. Field order
// is load-bearing (the artifact-read round-trip test re-serializes a slice and
// checks it stays under the cap — wire-shape contract). Optional fields
// use omitempty / pointers so an absent artifactId/errorCode is dropped, never
// emitted as null.
type truncationResult struct {
	Truncated   bool   `json:"truncated"`
	ArtifactID  string `json:"artifactId,omitempty"`
	ErrorCode   string `json:"errorCode,omitempty"`
	Recoverable *bool  `json:"recoverable,omitempty"`
	TotalChars  int    `json:"totalChars"`
	TotalBytes  int    `json:"totalBytes"`
	Preview     string `json:"preview"`
	Note        string `json:"note"`
}

// truncationStub is the full overflow envelope.
type truncationStub struct {
	Ok      bool             `json:"ok"`
	Summary string           `json:"summary"`
	Result  truncationResult `json:"result"`
}

// fullPayload is the normal (non-truncated) serialized result. result/error are
// emitted with omitempty so the {ok,summary,result,error} shape drops empty
// fields.
type fullPayload struct {
	Ok      bool              `json:"ok"`
	Summary string            `json:"summary"`
	Result  any               `json:"result,omitempty"`
	Error   *domain.ToolError `json:"error,omitempty"`
}

// SerializeToolResult serializes a tool result to JSON, truncating an oversized
// one into a valid-JSON artifact stub (NEVER a sliced-invalid mid-JSON string —
// issue #78). When the full serialization exceeds MaxToolResultChars
// (8000) it stashes the whole thing in artifactStore (if provided) and returns a
// stub the model can page with artifact.read. artifactStore may be nil (e.g.
// rehydration), in which case the note says the full result is unretrievable.
func SerializeToolResult(res domain.ToolResult, artifactStore *ArtifactStore) string {
	payload := fullPayload{Ok: res.Ok, Summary: res.Summary, Result: res.Result, Error: res.Error}
	b, err := json.Marshal(payload)
	if err != nil {
		// Unserializable result/error → fall back to ok+summary only.
		fb, _ := json.Marshal(struct {
			Ok      bool   `json:"ok"`
			Summary string `json:"summary"`
		}{res.Ok, res.Summary})
		b = fb
	}
	s := string(b)
	// The cap is in CHARACTERS, not bytes (it compares the JSON string's
	// character count, not its byte length). A multibyte-heavy result whose
	// byte length exceeds the cap but whose rune count does not must NOT truncate.
	totalChars := charLen(s)
	totalBytes := len(s) // byte length
	if totalChars <= domain.MaxToolResultChars {
		return s
	}

	// Overflow path — build a valid-JSON stub. Slice on rune boundaries so a
	// multibyte rune is never split.
	preview := sliceChars(s, domain.TruncationPreviewChars)
	previewLen := charLen(preview)

	var artifactID string
	if artifactStore != nil {
		artifactID = artifactStore.set(s)
	}

	var note string
	if artifactID != "" {
		note = "Output truncated to a " + itoa(previewLen) + "-char preview of " + itoa(totalChars) +
			" total. Call the artifact.read tool with artifactId \"" + artifactID +
			"\" (and offset/limit) to page through the full result."
	} else {
		note = "Output truncated to a " + itoa(previewLen) + "-char preview of " + itoa(totalChars) +
			" total; the full result is not retrievable in this context."
	}

	inner := truncationResult{
		Truncated:  true,
		ArtifactID: artifactID,
		TotalChars: totalChars,
		TotalBytes: totalBytes,
		Preview:    preview,
		Note:       note,
	}
	if res.Error != nil {
		inner.ErrorCode = res.Error.Code
		rec := res.Error.Recoverable
		inner.Recoverable = &rec
	}
	stub := truncationStub{
		Ok:      res.Ok,
		Summary: sliceChars(res.Summary, domain.TruncationSummaryChars),
		Result:  inner,
	}
	out, _ := json.Marshal(stub)
	return string(out)
}

// sliceChars returns the first n runes of s (rune-safe slicing — never splits a
// multibyte rune, unlike TS .slice on a surrogate pair).
func sliceChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	count := 0
	for i := range s {
		if count == n {
			return s[:i]
		}
		count++
	}
	return s
}

// charLen returns the rune count of s (the faithful char-count for slicing;
// rune count is chosen over a UTF-16 code-unit count).
func charLen(s string) int {
	count := 0
	for range s {
		count++
	}
	return count
}

// itoa is a tiny non-negative int formatter (avoids importing strconv here).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
