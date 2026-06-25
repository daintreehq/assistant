package prompts

import (
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func TestParseDistilledFactsArray(t *testing.T) {
	got := ParseDistilledFacts(`["deploy uses Fireworks tokens", "watcher tick floors at 3s"]`)
	if len(got) != 2 {
		t.Fatalf("got %d facts, want 2: %v", len(got), got)
	}
	if got[0] != "deploy uses Fireworks tokens" || got[1] != "watcher tick floors at 3s" {
		t.Fatalf("unexpected facts: %v", got)
	}
}

func TestParseDistilledFactsEmptyArray(t *testing.T) {
	if got := ParseDistilledFacts(`[]`); got != nil && len(got) != 0 {
		t.Fatalf("empty array should yield no facts, got %v", got)
	}
}

func TestParseDistilledFactsFenced(t *testing.T) {
	raw := "```json\n[\"alpha\", \"beta\"]\n```"
	got := ParseDistilledFacts(raw)
	if len(got) != 2 || got[0] != "alpha" || got[1] != "beta" {
		t.Fatalf("fenced JSON not parsed: %v", got)
	}
}

func TestParseDistilledFactsMalformed(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", "{\"a\":1}", "[1,2,3]"} {
		if got := ParseDistilledFacts(raw); len(got) != 0 {
			t.Fatalf("malformed %q should yield nothing, got %v", raw, got)
		}
	}
}

func TestParseDistilledFactsDropsBlanksAndDupes(t *testing.T) {
	got := ParseDistilledFacts(`["  ", "Fact", "fact", "  Fact  ", "other"]`)
	// "Fact"/"fact"/"  Fact  " collapse to one (case-insensitive, trimmed); blanks drop.
	if len(got) != 2 {
		t.Fatalf("got %d, want 2 (deduped): %v", len(got), got)
	}
	if got[0] != "Fact" || got[1] != "other" {
		t.Fatalf("unexpected dedup result: %v", got)
	}
}

func TestParseDistilledFactsCapsCount(t *testing.T) {
	var parts []string
	for i := 0; i < MaxDistilledFacts+5; i++ {
		parts = append(parts, `"fact `+string(rune('a'+i))+`"`)
	}
	got := ParseDistilledFacts("[" + strings.Join(parts, ",") + "]")
	if len(got) != MaxDistilledFacts {
		t.Fatalf("count not capped: got %d want %d", len(got), MaxDistilledFacts)
	}
}

func TestParseDistilledFactsTruncatesLongFact(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := ParseDistilledFacts(`["` + long + `"]`)
	if len(got) != 1 {
		t.Fatalf("want 1 fact, got %d", len(got))
	}
	if r := []rune(got[0]); len(r) > maxDistilledFactRunes {
		t.Fatalf("fact not truncated: %d runes", len(r))
	}
}

// --- ParseDistilledEntries (kind routing) ---

// TestParseDistilledEntriesRoutesKinds proves the per-entry {fact,kind} object shape
// routes each fact to its declared kind.
func TestParseDistilledEntriesRoutesKinds(t *testing.T) {
	got := ParseDistilledEntries(`[{"fact":"deploy uses Fireworks","kind":"semantic"},{"fact":"tried glm-5p2, flash was enough","kind":"episodic"}]`)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Content != "deploy uses Fireworks" || got[0].Kind != domain.MemoryKindSemantic {
		t.Fatalf("entry 0 = %+v, want semantic durable fact", got[0])
	}
	if got[1].Content != "tried glm-5p2, flash was enough" || got[1].Kind != domain.MemoryKindEpisodic {
		t.Fatalf("entry 1 = %+v, want episodic trace", got[1])
	}
}

// TestParseDistilledEntriesLegacyStringsAreSemantic proves the legacy []string reply
// (no kind tag) still parses, with every fact routed to semantic.
func TestParseDistilledEntriesLegacyStringsAreSemantic(t *testing.T) {
	got := ParseDistilledEntries(`["alpha", "beta"]`)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	for i, e := range got {
		if e.Kind != domain.MemoryKindSemantic {
			t.Fatalf("entry %d kind = %q, want semantic (legacy string fallback)", i, e.Kind)
		}
	}
}

// TestParseDistilledEntriesMixedArray proves a reply that mixes bare strings and
// objects is tolerated — each element routed independently, none dropped.
func TestParseDistilledEntriesMixedArray(t *testing.T) {
	got := ParseDistilledEntries(`["bare fact", {"fact":"object fact","kind":"episodic"}]`)
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(got), got)
	}
	if got[0].Content != "bare fact" || got[0].Kind != domain.MemoryKindSemantic {
		t.Fatalf("bare string entry = %+v, want semantic", got[0])
	}
	if got[1].Content != "object fact" || got[1].Kind != domain.MemoryKindEpisodic {
		t.Fatalf("object entry = %+v, want episodic", got[1])
	}
}

// TestParseDistilledEntriesUnknownKindFallsBackToSemantic proves a blank/garbled kind
// keeps the fact (it may still be valuable) and routes it to the safe semantic default
// rather than dropping it.
func TestParseDistilledEntriesUnknownKindFallsBackToSemantic(t *testing.T) {
	got := ParseDistilledEntries(`[{"fact":"a","kind":"wat"},{"fact":"b","kind":""},{"fact":"c"}]`)
	if len(got) != 3 {
		t.Fatalf("got %d entries, want 3 (none dropped): %+v", len(got), got)
	}
	for _, e := range got {
		if e.Kind != domain.MemoryKindSemantic {
			t.Fatalf("unknown/blank/missing kind should fall back to semantic, got %+v", e)
		}
	}
}

// TestParseDistilledEntriesContentAlias proves the tolerated "content" key is accepted
// when the model uses it instead of "fact".
func TestParseDistilledEntriesContentAlias(t *testing.T) {
	got := ParseDistilledEntries(`[{"content":"via alias","kind":"episodic"}]`)
	if len(got) != 1 || got[0].Content != "via alias" || got[0].Kind != domain.MemoryKindEpisodic {
		t.Fatalf("content-alias entry not parsed: %+v", got)
	}
}

// TestParseDistilledEntriesWhitespaceFactContentAlias proves a whitespace-only "fact"
// falls back to the "content" alias (not just an exactly-empty fact) — so a padded
// primary key doesn't shadow the real statement.
func TestParseDistilledEntriesWhitespaceFactContentAlias(t *testing.T) {
	got := ParseDistilledEntries(`[{"fact":"   ","content":"real content","kind":"episodic"}]`)
	if len(got) != 1 || got[0].Content != "real content" || got[0].Kind != domain.MemoryKindEpisodic {
		t.Fatalf("whitespace-fact should fall back to content alias: %+v", got)
	}
}

// TestParseDistilledEntriesNonStringKindFallsBackNotDrops proves a non-string kind (a
// stray number/bool) demotes the entry to semantic WITHOUT dropping its valid fact —
// the lenient contract: partial model compliance never zeros usable content.
func TestParseDistilledEntriesNonStringKindFallsBackNotDrops(t *testing.T) {
	for _, raw := range []string{
		`[{"fact":"valid","kind":1}]`,
		`[{"fact":"valid","kind":true}]`,
		`[{"fact":"valid","kind":null}]`,
	} {
		got := ParseDistilledEntries(raw)
		if len(got) != 1 {
			t.Fatalf("%s: got %d entries, want 1 (fact preserved): %+v", raw, len(got), got)
		}
		if got[0].Content != "valid" || got[0].Kind != domain.MemoryKindSemantic {
			t.Fatalf("%s: entry = %+v, want valid/semantic", raw, got[0])
		}
	}
}

// TestParseDistilledEntriesDropsBlankFactObjects proves an object with no usable fact
// is dropped while siblings survive.
func TestParseDistilledEntriesDropsBlankFactObjects(t *testing.T) {
	got := ParseDistilledEntries(`[{"fact":"  ","kind":"semantic"},{"fact":"keep","kind":"episodic"}]`)
	if len(got) != 1 || got[0].Content != "keep" {
		t.Fatalf("blank-fact object should drop, keep survive: %+v", got)
	}
}

// TestParseDistilledEntriesMalformed proves a non-array reply yields nothing.
func TestParseDistilledEntriesMalformed(t *testing.T) {
	for _, raw := range []string{"", "   ", "not json", `{"fact":"x","kind":"semantic"}`} {
		if got := ParseDistilledEntries(raw); len(got) != 0 {
			t.Fatalf("malformed %q should yield nothing, got %+v", raw, got)
		}
	}
}

// TestParseDistilledEntriesFencedObjects proves a ```json fence around the object array
// is stripped.
func TestParseDistilledEntriesFencedObjects(t *testing.T) {
	raw := "```json\n[{\"fact\":\"x\",\"kind\":\"episodic\"}]\n```"
	got := ParseDistilledEntries(raw)
	if len(got) != 1 || got[0].Content != "x" || got[0].Kind != domain.MemoryKindEpisodic {
		t.Fatalf("fenced object array not parsed: %+v", got)
	}
}

func TestPinnedMemoriesBlockRendered(t *testing.T) {
	out := BuildRuntimeContextMessage(MainPromptContext{PinnedMemories: "- deploy uses Fireworks"})
	if !strings.Contains(out, "# Pinned memories") {
		t.Fatalf("pinned header missing:\n%s", out)
	}
	if !strings.Contains(out, "- deploy uses Fireworks") {
		t.Fatalf("pinned content missing:\n%s", out)
	}
}

func TestPinnedMemoriesBlockOmittedWhenEmpty(t *testing.T) {
	out := BuildRuntimeContextMessage(MainPromptContext{})
	if strings.Contains(out, "# Pinned memories") {
		t.Fatalf("pinned block should be absent when empty:\n%s", out)
	}
	// Runtime header (message[1]) must always be present.
	if !strings.Contains(out, "# Runtime context") {
		t.Fatalf("runtime header missing:\n%s", out)
	}
}
