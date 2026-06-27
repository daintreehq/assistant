package terminalid

import (
	"reflect"
	"testing"
)

func TestResolve_ExactMatch(t *testing.T) {
	live := []string{"terminal-aaaa-1", "terminal-bbbb-2"}
	r := Resolve([]string{"terminal-bbbb-2"}, live)
	if !r.OK() {
		t.Fatalf("exact match should resolve, got %+v", r)
	}
	if !reflect.DeepEqual(r.Resolved, []string{"terminal-bbbb-2"}) {
		t.Fatalf("want [terminal-bbbb-2], got %v", r.Resolved)
	}
}

// The model's actual failure mode: an 8-hex prefix of the canonical UUID id.
func TestResolve_UniquePrefixExpands(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c-90cb-136f24046295"
	other := "terminal-2e8f0743-b416-4900-816c-297a947874ab"
	r := Resolve([]string{"terminal-5284bfef"}, []string{full, other})
	if !r.OK() {
		t.Fatalf("a unique prefix should resolve, got %+v", r)
	}
	if !reflect.DeepEqual(r.Resolved, []string{full}) {
		t.Fatalf("prefix should expand to the canonical id %q, got %v", full, r.Resolved)
	}
}

func TestResolve_UnknownIsReported(t *testing.T) {
	r := Resolve([]string{"terminal-deadbeef"}, []string{"terminal-5284bfef-3d11"})
	if r.OK() {
		t.Fatalf("an unmatched id must not resolve, got %+v", r)
	}
	if !reflect.DeepEqual(r.Unknown, []string{"terminal-deadbeef"}) {
		t.Fatalf("want Unknown [terminal-deadbeef], got %v", r.Unknown)
	}
}

// A prefix shared by two live ids must NOT silently pick one — it is ambiguous and the
// caller is told to pass the full id (mirrors the worktree resolver's ambiguous-branch
// fall-through).
func TestResolve_AmbiguousPrefixRejected(t *testing.T) {
	r := Resolve([]string{"terminal-ab"}, []string{"terminal-abc-1", "terminal-abd-2"})
	if r.OK() {
		t.Fatalf("an ambiguous prefix must not resolve, got %+v", r)
	}
	if !reflect.DeepEqual(r.Ambiguous, []string{"terminal-ab"}) {
		t.Fatalf("want Ambiguous [terminal-ab], got %v", r.Ambiguous)
	}
	if len(r.Resolved) != 0 {
		t.Fatalf("an ambiguous request must not appear in Resolved, got %v", r.Resolved)
	}
}

// An exact id that is ALSO a prefix of a longer live id resolves to the exact match,
// never ambiguous — exact wins outright.
func TestResolve_ExactBeatsPrefix(t *testing.T) {
	short := "terminal-abc"
	long := "terminal-abc-extra"
	r := Resolve([]string{short}, []string{short, long})
	if !r.OK() {
		t.Fatalf("exact match should win over the longer prefix, got %+v", r)
	}
	if !reflect.DeepEqual(r.Resolved, []string{short}) {
		t.Fatalf("want exact %q, got %v", short, r.Resolved)
	}
}

// Two different requests that resolve to the same canonical id collapse to one entry, so a
// downstream loop over Resolved never double-counts the terminal.
func TestResolve_DedupesToSameCanonical(t *testing.T) {
	full := "terminal-5284bfef-3d11-424c"
	r := Resolve([]string{"terminal-5284bfef", full}, []string{full})
	if !r.OK() {
		t.Fatalf("both should resolve, got %+v", r)
	}
	if !reflect.DeepEqual(r.Resolved, []string{full}) {
		t.Fatalf("prefix + full → one canonical id, got %v", r.Resolved)
	}
}

func TestResolve_MixedUnknownAndResolved(t *testing.T) {
	full := "terminal-5284bfef-3d11"
	r := Resolve([]string{"terminal-5284bfef", "terminal-nope"}, []string{full})
	if r.OK() {
		t.Fatalf("a mixed batch with one bad id must not be OK, got %+v", r)
	}
	if !reflect.DeepEqual(r.Resolved, []string{full}) {
		t.Fatalf("the good id should still resolve, got %v", r.Resolved)
	}
	if !reflect.DeepEqual(r.Unknown, []string{"terminal-nope"}) {
		t.Fatalf("the bad id should be Unknown, got %v", r.Unknown)
	}
}

func TestParseListIDs_StructuredAndText(t *testing.T) {
	structured := map[string]any{"terminals": []any{
		map[string]any{"id": "terminal-aaa"},
		map[string]any{"terminalId": "terminal-bbb"}, // fallback key
	}}
	text := `{"terminals":[{"id":"terminal-bbb"},{"id":"terminal-ccc"}]}` // bbb dup, ccc new
	got := ParseListIDs(structured, text)
	want := []string{"terminal-aaa", "terminal-bbb", "terminal-ccc"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("union+dedupe across sources failed: want %v, got %v", want, got)
	}
}

func TestLooksCanonical(t *testing.T) {
	canonical := []string{
		"terminal-5284bfef-3d11-424c-90cb-136f24046295",
		"terminal-2e8f0743-b416-4900-816c-297a947874ab",
	}
	for _, id := range canonical {
		if !LooksCanonical(id) {
			t.Errorf("%q should look canonical", id)
		}
	}
	notCanonical := []string{
		"terminal-5284bfef",                    // truncated prefix — must still be resolved
		"terminal-5284bfef-3d11",               // partial
		"5284bfef-3d11-424c-90cb-136f24046295", // missing terminal- prefix
		"term_1",
		"",
		"terminal-",
	}
	for _, id := range notCanonical {
		if LooksCanonical(id) {
			t.Errorf("%q should NOT look canonical (must be resolved)", id)
		}
	}
}

func TestParseListIDs_EmptyAndGarbage(t *testing.T) {
	if got := ParseListIDs(nil, ""); got != nil {
		t.Fatalf("empty inputs should yield nil, got %v", got)
	}
	if got := ParseListIDs(nil, "not json"); got != nil {
		t.Fatalf("unparseable text should yield nil, got %v", got)
	}
	if got := ParseListIDs(map[string]any{"terminals": []any{}}, ""); got != nil {
		t.Fatalf("an empty terminals array should yield nil, got %v", got)
	}
}
