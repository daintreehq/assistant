package backend

import (
	"strings"
	"testing"
)

// feedAll runs text through the filter in fragments of the given size and returns what
// a downstream consumer would actually see — the concatenation of every Feed result
// plus the Finish flush. Fragmenting is the whole point: a leaked marker arrives as a
// token stream, so a guard that only works on a whole string is no guard at all.
func feedAll(text string, chunk int) string {
	var f declarationFilter
	var out strings.Builder
	for i := 0; i < len(text); i += chunk {
		end := i + chunk
		if end > len(text) {
			end = len(text)
		}
		out.WriteString(f.Feed(text[i:end]))
	}
	out.WriteString(f.Finish())
	return out.String()
}

// The regression issue #365 explicitly asks for: a declaration marker the backend
// failed to strip must never reach a human, at ANY fragmentation. One character at a
// time is the adversarial case — every prefix of the marker is fed as its own
// ambiguous fragment.
func TestDeclarationFilterStripsLeakedMarkerAtEveryFragmentation(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"final marker", "[[DAINTREE:FINAL]]\nThe worktree is ready.", "The worktree is ready."},
		{"tools marker", "[[DAINTREE:TOOLS]]\nChecking the roster.", "Checking the roster."},
		{"no separator", "[[DAINTREE:FINAL]]Done.", "Done."},
		{"crlf separator", "[[DAINTREE:FINAL]]\r\nDone.", "Done."},
		{"spaces then newline", "[[DAINTREE:FINAL]]   \nDone.", "Done."},
		{"leading whitespace", "  \n[[DAINTREE:FINAL]]\nDone.", "Done."},
		{"deliberate blank line survives", "[[DAINTREE:FINAL]]\n\nDone.", "\nDone."},
		{"marker only", "[[DAINTREE:FINAL]]", ""},
	}
	for _, tc := range cases {
		for _, chunk := range []int{1, 2, 3, 7, 17, len(tc.in)} {
			if chunk == 0 {
				continue
			}
			if got := feedAll(tc.in, chunk); got != tc.want {
				t.Errorf("%s @chunk=%d: got %q, want %q", tc.name, chunk, got, tc.want)
			}
		}
	}
}

// The far more important half: the filter must be invisible on every ordinary reply.
// A guard that eats real prose to catch a marker that never arrives is worse than the
// leak it guards against.
//
// Two cases below look like leaks and are not. A marker past the whitespace bound, and a
// marker that is not at the very start, are BOTH ordinary content by the server's own
// rule — its scanner would not have stripped them either, so what arrives is a reply the
// model wrote that merely contains those characters, not a declaration. Diverging from
// the oracle to "catch" them would mean deleting text the server deliberately kept.
func TestDeclarationFilterPassesOrdinaryProseThrough(t *testing.T) {
	cases := []string{
		"The worktree is ready.",
		"[this is a bracketed opening]",
		"[[not a marker]] still prose",
		"[[DAINTREE:OTHER]] unknown marker stays",
		"mid-reply [[DAINTREE:FINAL]] is not a declaration",
		"\n\n\n\n\n\n\n\n\n\n[[DAINTREE:FINAL]] past the whitespace bound", // prose by the oracle's rule too
		"  leading spaces then prose",
		"",
		"[",
		"[[DAINT",
	}
	for _, in := range cases {
		for _, chunk := range []int{1, 3, 11, 64} {
			if got := feedAll(in, chunk); got != in {
				t.Errorf("prose %q @chunk=%d: got %q, want it unchanged", in, chunk, got)
			}
		}
	}
}

// A reply that stops mid-marker held real characters, and they are still owed to the
// caller — the marker never completed, so it was never a marker.
func TestDeclarationFilterFinishReleasesHeldPrefix(t *testing.T) {
	var f declarationFilter
	if got := f.Feed("[[DAINT"); got != "" {
		t.Fatalf("an ambiguous prefix must be held, got %q", got)
	}
	if got := f.Finish(); got != "[[DAINT" {
		t.Fatalf("Finish must release the held prefix, got %q", got)
	}
	// Finish is idempotent for a settled filter.
	if got := f.Finish(); got != "" {
		t.Fatalf("second Finish leaked %q", got)
	}
}
