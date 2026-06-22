package prompts

import (
	"encoding/json"
	"strings"
)

// DistillSystemPrompt instructs the small model to extract durable, project-scoped
// facts from a conversation that is ABOUT TO BE DISCARDED during compaction. It is
// deliberately strict (JSON array only, [] when nothing durable) so a best-effort
// parse of the reply is reliable; the caller silently drops anything it can't parse.
// Both compaction paths (agent auto-compact + the /compact command) share this prompt
// so the extracted memories are consistent regardless of which path discards history.
const DistillSystemPrompt = `You are distilling durable memory from a conversation that is about to be discarded during compaction.
Extract only facts worth remembering across future sessions: project decisions, constraints, conventions, goals, environment quirks, and stable user preferences.
Do NOT include transient task status, tool output, greetings, or generic advice that is not specific to this project.
Return ONLY a JSON array of strings. Each string is one self-contained fact, at most 200 characters. If nothing durable was discussed, return [].
No prose, no markdown, no commentary outside the JSON array.`

// MaxDistilledFacts caps how many facts a single compaction may save (noise guard).
const MaxDistilledFacts = 10

// maxDistilledFactRunes bounds one distilled fact (longer ⇒ truncated to fit).
const maxDistilledFactRunes = 200

// ParseDistilledFacts parses the small model's distillation reply (expected: a JSON
// array of strings) into a cleaned, de-duplicated, capped fact list. It is lenient:
// it tolerates a leading ```json / ``` fence, trims whitespace, drops blanks,
// truncates each fact to maxDistilledFactRunes, de-dupes case-insensitively, and caps
// the count at MaxDistilledFacts. Any parse failure yields nil — distillation is
// best-effort, so the caller treats nil as "saved nothing".
func ParseDistilledFacts(raw string) []string {
	raw = strings.TrimSpace(raw)
	// Strip a fenced code block (```json … ``` or ``` … ```) if the model wrapped one.
	if strings.HasPrefix(raw, "```") {
		if i := strings.IndexByte(raw, '\n'); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimSuffix(strings.TrimSpace(raw), "```")
		raw = strings.TrimSpace(raw)
	}
	if raw == "" {
		return nil
	}
	var facts []string
	if err := json.Unmarshal([]byte(raw), &facts); err != nil {
		return nil
	}
	seen := make(map[string]struct{}, len(facts))
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if r := []rune(f); len(r) > maxDistilledFactRunes {
			f = strings.TrimSpace(string(r[:maxDistilledFactRunes]))
		}
		key := strings.ToLower(f)
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, f)
		if len(out) >= MaxDistilledFacts {
			break
		}
	}
	return out
}
