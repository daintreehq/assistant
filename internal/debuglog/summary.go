package debuglog

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// Summary is a bounded, hashable description of a possibly-large payload.
//
// The high-frequency trace events added for the backend migration (the per-round
// backend respond request and every MCP read) would, if dumped in full, re-print
// the whole conversation / prompt / terminal scrollback every single round — the
// exact O(turns²) growth the legacy router's logElider was built to avoid. A
// Summary keeps each such event diagnostic without the bloat: the full byte length,
// a short content hash so two identical payloads are recognisable across events
// without re-printing either, and a bounded preview of the head.
//
// This is deliberately NOT used for the full-fidelity tool.call event, which stays
// untruncated (it is logged once per call and is the ground truth the dev loop
// greps) — Summary is for the events that repeat every round.
type Summary struct {
	Bytes     int    `json:"bytes"`
	SHA       string `json:"sha,omitempty"`
	Preview   string `json:"preview,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// defaultPreviewMax is the standard preview budget (in runes) for model/tool/MCP
// text when a caller passes max <= 0.
const defaultPreviewMax = 2000

// Summarize bounds s into a Summary: the full byte length, a sha256 prefix over the
// whole string (so identical payloads correlate without re-dumping), and a preview
// of at most max runes (rune-safe — a multibyte boundary is never split). max <= 0
// selects defaultPreviewMax. An empty string yields the zero Summary.
func Summarize(s string, max int) Summary {
	if max <= 0 {
		max = defaultPreviewMax
	}
	sum := Summary{Bytes: len(s)}
	if s == "" {
		return sum
	}
	h := sha256.Sum256([]byte(s))
	// First 8 bytes (16 hex chars) is plenty to correlate two payloads within one
	// session while keeping the trace line short.
	sum.SHA = "sha256:" + fmt.Sprintf("%x", h[:8])
	sum.Preview, sum.Truncated = headRunes(s, max)
	return sum
}

// SummarizeJSON marshals v and summarizes the result. On a marshal error it summarizes
// a type-tagged marker instead of the value — a logging helper must never surface an
// error, and a %v rendering of an unmarshalable value can carry pointer addresses
// (non-deterministic, defeating the correlation hash). The type tag is stable.
func SummarizeJSON(v any, max int) Summary {
	blob, err := json.Marshal(v)
	if err != nil {
		return Summarize(fmt.Sprintf("<unmarshalable %T>", v), max)
	}
	return Summarize(string(blob), max)
}

// Preview returns a bounded, rune-safe excerpt of s (no hash/metadata) for inline
// fields where a full Summary is overkill — e.g. a one-line prompt or reply preview.
// max <= 0 selects a short 240-rune budget. An over-budget string gets a trailing
// ellipsis so a truncated preview is visibly truncated.
func Preview(s string, max int) string {
	if max <= 0 {
		max = 240
	}
	head, truncated := headRunes(s, max)
	if truncated {
		return head + "…"
	}
	return head
}

// headRunes returns the first max runes of s and whether s exceeded max — WITHOUT
// allocating a full []rune for the whole (possibly huge) string. Ranging a string
// yields successive rune-start byte indices, so we stop at the (max+1)-th rune and
// slice on a known-valid boundary.
func headRunes(s string, max int) (string, bool) {
	if max <= 0 {
		return "", s != ""
	}
	count := 0
	for i := range s {
		if count == max {
			return s[:i], true
		}
		count++
	}
	return s, false
}
