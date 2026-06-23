package models

import (
	"strings"
	"testing"
)

// big builds a content string comfortably over elideMinBytes.
func big(seed string) string { return seed + strings.Repeat("x", elideMinBytes) }

// A repeated (role,content) is logged full the first time and referenced after,
// while short content and distinct content always pass through verbatim.
func TestLogEliderCollapsesRepeats(t *testing.T) {
	e := newLogElider()
	sys := big("system-prompt-")

	first := e.elide([]map[string]any{
		{"role": "system", "content": sys},
		{"role": "user", "content": "hi"}, // short → always inline
	})
	if first[0]["content"] != sys {
		t.Fatalf("first occurrence must pass through full, got %v", first[0])
	}
	if _, elided := first[0]["repeated"]; elided {
		t.Fatal("first occurrence must not be marked repeated")
	}
	sha, _ := first[0]["contentSha"].(string)
	if !strings.HasPrefix(sha, "sha256:") {
		t.Fatalf("first occurrence must be tagged with its hash, got %v", first[0]["contentSha"])
	}

	second := e.elide([]map[string]any{
		{"role": "system", "content": sys},                // repeat → elided
		{"role": "user", "content": "hi"},                 // short → inline
		{"role": "tool", "content": big("fresh-result-")}, // new → full
	})
	if _, ok := second[0]["content"]; ok {
		t.Fatalf("repeated system content must be dropped, got %v", second[0])
	}
	ref, _ := second[0]["repeated"].(string)
	// The reference must be greppable straight back to the full first occurrence.
	if !strings.HasPrefix(ref, sha) || !strings.Contains(ref, "logged once") {
		t.Fatalf("repeated marker = %q, want it to lead with %q", ref, sha)
	}
	if second[1]["content"] != "hi" {
		t.Fatalf("short content must always stay inline, got %v", second[1])
	}
	if second[2]["content"] == nil {
		t.Fatal("a brand-new large content must be logged full")
	}
}

// elide must not mutate the caller's entries (it copies before annotating).
func TestLogEliderDoesNotMutateInput(t *testing.T) {
	e := newLogElider()
	entry := map[string]any{"role": "system", "content": big("p-")}
	_ = e.elide([]map[string]any{entry})
	if _, leaked := entry["contentSha"]; leaked {
		t.Fatal("elide must not add fields to the caller's entry")
	}
}

// Identical content under different roles hashes distinctly (role is part of the key).
func TestLogEliderRoleScoped(t *testing.T) {
	e := newLogElider()
	body := big("shared-")
	e.elide([]map[string]any{{"role": "user", "content": body}})
	out := e.elide([]map[string]any{{"role": "assistant", "content": body}})
	if _, elided := out[0]["repeated"]; elided {
		t.Fatal("same body under a different role must not be treated as a repeat")
	}
}

// A nil elider is a safe pass-through (a Router built without one never panics).
func TestLogEliderNilPassthrough(t *testing.T) {
	var e *logElider
	in := []map[string]any{{"role": "system", "content": big("p-")}}
	if out := e.elide(in); len(out) != 1 || out[0]["content"] == nil {
		t.Fatalf("nil elider must return input unchanged, got %v", out)
	}
}
