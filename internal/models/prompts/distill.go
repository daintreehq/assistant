package prompts

import (
	"encoding/json"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// DistillSystemPrompt instructs the small model to extract durable, project-scoped
// facts from a conversation that is ABOUT TO BE DISCARDED during compaction, and to
// ROUTE each one to a kind: a stable "semantic" fact vs an instructive "episodic"
// trajectory trace. It is deliberately strict (JSON array of {fact,kind} objects, []
// when nothing is worth keeping) so a best-effort parse of the reply is reliable; the
// parser stays lenient (a bare string still counts as a semantic fact) so a partially
// compliant reply never zeros the output. Both compaction paths (agent auto-compact +
// the /compact command) share this prompt so the routing is consistent regardless of
// which path discards history.
const DistillSystemPrompt = `You are distilling memory from a conversation that is about to be discarded during compaction.
Extract only what is worth remembering, and tag each entry with a kind:
- "semantic": a stable, durable fact — a project decision, constraint, convention, goal, environment quirk, or settled user preference. True across future sessions.
- "episodic": an instructive trajectory trace from this work — what was tried and how it turned out ("tried X, it failed because Y", "an agent run produced Z"). Scoped to this run; valuable as a lesson, not a permanent fact.
Do NOT include transient task status, tool output, greetings, or generic advice that is not specific to this project.
Return ONLY a JSON array of objects, each exactly {"fact": "<one self-contained statement, at most 200 characters>", "kind": "semantic" or "episodic"}. If nothing is worth remembering, return [].
No prose, no markdown, no commentary outside the JSON array.`

// MaxDistilledFacts caps how many facts a single compaction may save (noise guard).
const MaxDistilledFacts = 10

// maxDistilledFactRunes bounds one distilled fact (longer ⇒ truncated to fit).
const maxDistilledFactRunes = 200

// DistilledEntry is one distilled fact plus the memory kind it routes to. Kind is
// always a concrete value (the parser defaults anything non-episodic to semantic).
type DistilledEntry struct {
	Content string
	Kind    domain.MemoryKind
}

// distilledObj is the per-entry object the distill model is asked to emit. content is
// tolerated as an alias for fact (small models occasionally pick the other key). Kind
// is decoded as raw JSON so a non-string value (a stray number/bool) demotes to the
// semantic default WITHOUT failing the whole object and dropping its valid fact.
type distilledObj struct {
	Fact    string          `json:"fact"`
	Content string          `json:"content"`
	Kind    json.RawMessage `json:"kind"`
}

// ParseDistilledEntries parses the small model's distillation reply into a cleaned,
// de-duplicated, capped list of {fact, kind} entries. The expected reply is a JSON
// array of {"fact","kind"} objects, but the parser is deliberately lenient so a
// partially-compliant model never zeros the output:
//   - a leading ```json / ``` fence is stripped;
//   - each element may be an object ({"fact"/"content","kind"}) OR a bare string (the
//     legacy []string shape), which is treated as a semantic fact — a mixed array is fine;
//   - an unknown/blank/garbled kind falls back to semantic rather than dropping the fact
//     (only an explicit "episodic" routes to episodic);
//   - blanks drop, each fact truncates to maxDistilledFactRunes, facts de-dupe
//     case-insensitively, and the count caps at MaxDistilledFacts.
//
// Any top-level parse failure (not a JSON array) yields nil — distillation is
// best-effort, so the caller treats nil as "saved nothing".
func ParseDistilledEntries(raw string) []DistilledEntry {
	raw = stripCodeFence(raw)
	if raw == "" {
		return nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &elems); err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(elems))
	out := make([]DistilledEntry, 0, len(elems))
	for _, el := range elems {
		fact, kind := decodeDistilledElem(el)
		fact = strings.TrimSpace(fact)
		if fact == "" {
			continue
		}
		if r := []rune(fact); len(r) > maxDistilledFactRunes {
			fact = strings.TrimSpace(string(r[:maxDistilledFactRunes]))
		}
		key := strings.ToLower(fact)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, DistilledEntry{Content: fact, Kind: kind})
		if len(out) >= MaxDistilledFacts {
			break
		}
	}
	return out
}

// ParseDistilledFacts is the flat-list view of ParseDistilledEntries: it returns just
// the fact strings, discarding the routed kind. Retained for callers/tests that don't
// route by kind.
func ParseDistilledFacts(raw string) []string {
	entries := ParseDistilledEntries(raw)
	if entries == nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Content)
	}
	return out
}

// stripCodeFence trims surrounding whitespace and removes a wrapping ```json … ``` (or
// plain ``` … ```) fence if the model emitted one.
func stripCodeFence(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
		raw = strings.TrimSpace(raw)
	}
	return raw
}

// decodeDistilledElem reads one array element as either a {"fact"/"content","kind"}
// object or a bare string. The returned kind is always concrete (unknown ⇒ semantic);
// an undecodable element yields an empty fact, which the caller drops.
func decodeDistilledElem(el json.RawMessage) (fact string, kind domain.MemoryKind) {
	if strings.HasPrefix(strings.TrimSpace(string(el)), "{") {
		var obj distilledObj
		if err := json.Unmarshal(el, &obj); err != nil {
			return "", domain.MemoryKindSemantic
		}
		// Fall back to the "content" alias when "fact" is missing OR whitespace-only, so
		// a padded primary key doesn't shadow the real statement in "content".
		f := obj.Fact
		if strings.TrimSpace(f) == "" {
			f = obj.Content
		}
		return f, normalizeMemoryKind(obj.Kind)
	}
	// Bare string (legacy []string distill reply) ⇒ semantic fact.
	var s string
	if err := json.Unmarshal(el, &s); err != nil {
		return "", domain.MemoryKindSemantic
	}
	return s, domain.MemoryKindSemantic
}

// normalizeMemoryKind maps the model's raw kind value onto a concrete MemoryKind. Only
// an explicit string "episodic" (case-insensitive) routes to episodic; anything else —
// blank, "semantic", a typo, null, or a non-string (number/bool, which fails the string
// decode) — is the safe semantic default.
func normalizeMemoryKind(raw json.RawMessage) domain.MemoryKind {
	var k string
	_ = json.Unmarshal(raw, &k) // non-string ⇒ k stays "" ⇒ semantic
	if strings.EqualFold(strings.TrimSpace(k), string(domain.MemoryKindEpisodic)) {
		return domain.MemoryKindEpisodic
	}
	return domain.MemoryKindSemantic
}
