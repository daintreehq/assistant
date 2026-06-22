package prompts

import (
	"strings"
	"testing"
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
