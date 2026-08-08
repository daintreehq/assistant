package mcpx

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// WrappedMCPToolNames is production-load-bearing: internal/app injects it as the
// MCP client's drift baseline, so every name here is checked against the live
// server at connect. These guards are the successor to the deleted
// TestDocumentedBaselineHealth, which policed the hand-maintained list this
// replaced.

// TestWrappedMCPToolNamesHealth: the baseline must be non-empty, free of blank or
// duplicate entries (a dup would emit duplicate drift warnings), and sorted (the
// documented contract, so the warning order is stable run to run).
func TestWrappedMCPToolNamesHealth(t *testing.T) {
	names := WrappedMCPToolNames()
	if len(names) == 0 {
		t.Fatal("drift baseline must be non-empty")
	}
	// The baseline is a UNION (denylist ∪ wrapperMCPTargets ∪ directMCPDependencies),
	// so it is expected to be larger than the redirect denylist alone — that gap is
	// precisely the coverage the denylist-only baseline was missing. It must never be
	// SMALLER, which would mean a source was dropped from the union.
	if len(names) < len(wrappedMCPTools) {
		t.Errorf("baseline has %d names, fewer than the %d denylist entries it must cover",
			len(names), len(wrappedMCPTools))
	}
	seen := map[string]bool{}
	for _, n := range names {
		if strings.TrimSpace(n) == "" {
			t.Error("baseline contains a blank name")
		}
		if seen[n] {
			t.Errorf("duplicate baseline entry %q would emit duplicate drift warnings", n)
		}
		seen[n] = true
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("baseline must be sorted for a stable warning order, got %v", names)
	}
	// Every entry must look like a raw Daintree action id (dotted namespace, no
	// whitespace). A local wrapper name or a padded variant leaking in here would
	// drift-warn forever, since the host never advertises such a name.
	for _, n := range names {
		if !strings.Contains(n, ".") {
			t.Errorf("baseline entry %q lacks a dotted namespace — Daintree action ids are dotted", n)
		}
		if strings.ContainsAny(n, " \t\n") {
			t.Errorf("baseline entry %q contains whitespace and could never match a live tool", n)
		}
	}
}

// TestWrappedMCPToolNamesIsACopy: the accessor must not hand out a view a caller
// could mutate into the denylist (which gates daintree.call's typed-wrapper
// redirect — corrupting it would let a raw mutating call bypass validation).
func TestWrappedMCPToolNamesIsACopy(t *testing.T) {
	first := WrappedMCPToolNames()
	if len(first) == 0 {
		t.Fatal("baseline unexpectedly empty")
	}
	original := first[0]
	first[0] = "mutated.by.caller"

	second := WrappedMCPToolNames()
	if second[0] != original {
		t.Errorf("mutating the returned slice changed the baseline: got %q, want %q", second[0], original)
	}
	if _, ok := wrappedMCPTools["mutated.by.caller"]; ok {
		t.Error("caller mutation leaked into the denylist map")
	}
}

// TestWrappedMCPToolNamesCoversDenylist: every daintree.call redirect target is in
// the baseline. A redirected name is by definition one we have a wrapper for.
func TestWrappedMCPToolNamesCoversDenylist(t *testing.T) {
	got := map[string]bool{}
	for _, n := range WrappedMCPToolNames() {
		got[n] = true
	}
	for name := range wrappedMCPTools {
		if !got[name] {
			t.Errorf("denylist entry %q missing from the drift baseline", name)
		}
	}
}

// wrapperSourceGlobs are the files whose raw MCP name literals must all appear in
// the drift baseline: the typed wrappers in this package and in internal/tools/mcpwrap.
var wrapperSourceGlobs = []string{
	"*.go",
	"../mcpwrap/*.go",
}

// mcpNameLiteral matches a dotted Daintree action id in a Go string literal
// ("terminal.getStatus"). Deliberately narrow — lowerCamel segments separated by a
// dot — so ordinary strings ("application/json", file paths, prose) do not match.
var mcpNameLiteral = regexp.MustCompile(`"([a-z][a-zA-Z0-9]*(?:\.[a-z][a-zA-Z0-9]*)+)"`)

// nonMCPLiterals are dotted lowerCamel literals in those files that are NOT MCP
// action ids (local tool names, arg paths, media types). Listed explicitly so the
// scan can be strict about everything else.
var nonMCPLiterals = map[string]bool{
	// Tool names this build REGISTERS locally (the model calls these; they are not
	// host MCP actions, so drift must not expect the server to advertise them).
	//
	// Keep this list MINIMAL and local-only: every entry is a name the scan will
	// never check again, so a speculative exemption could silently excuse a real
	// host dependency later. Never add a name just to quiet the test — if the
	// literal names a tool the HOST provides, it belongs in the baseline instead.
	"daintree.status":    true,
	"daintree.listTools": true,
	"daintree.call":      true,
	"tool.search":        true,
	"terminal.focus":     true,
}

// TestMCPDependenciesCoverWrapperCallSites is what makes the drift baseline
// self-maintaining. It scans the wrapper sources for raw MCP name literals and
// fails if any is missing from WrappedMCPToolNames().
//
// This is the guard that was missing when the baseline was just the redirect
// denylist: six real wrapper targets (copyTree.generate, worktree.list/getCurrent,
// the forge reads) had no denylist entry, so the baseline silently under-covered
// the dependency set and drift would not have warned if the host dropped them.
// Add a wrapper for a new host tool and this test fails until it is declared.
func TestMCPDependenciesCoverWrapperCallSites(t *testing.T) {
	baseline := map[string]bool{}
	for _, n := range WrappedMCPToolNames() {
		baseline[n] = true
	}

	var files []string
	for _, glob := range wrapperSourceGlobs {
		matched, err := filepath.Glob(glob)
		if err != nil {
			t.Fatalf("glob %q: %v", glob, err)
		}
		files = append(files, matched...)
	}
	if len(files) == 0 {
		t.Fatal("found no wrapper sources to scan — the globs are wrong, so this guard is inert")
	}

	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		scanned++
		for _, m := range mcpNameLiteral.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if nonMCPLiterals[name] || baseline[name] {
				continue
			}
			t.Errorf("%s calls raw MCP tool %q, which is missing from the drift baseline "+
				"— add it to wrapperMCPTargets or wrappedMCPTools so drift warns if the host drops it",
				path, name)
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no non-test wrapper sources — the guard is inert")
	}
}
