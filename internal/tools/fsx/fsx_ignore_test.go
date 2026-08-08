package fsx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTree lays out a fixture from a rel-path → content map.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// walked returns the set of project-relative slash paths walkFiles yields.
func walked(root string) map[string]bool {
	out := map[string]bool{}
	for _, e := range walkFiles(root) {
		out[filepath.ToSlash(e.rel)] = true
	}
	return out
}

// listedNames returns the entry names an fs.list call produced.
func listedNames(t *testing.T, root, path string, depth int) map[string]bool {
	t.Helper()
	res := callList(t, root, path, depth)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	out := map[string]bool{}
	for _, e := range res.Result.(map[string]any)["entries"].([]listEntry) {
		out[e.Name] = true
	}
	return out
}

// TestIgnoreAppliesToEveryDiscoveryTool is the headline behaviour: an ignored file
// is invisible to the walk, to fs.list, to fs.find and to fs.search alike, so the
// assistant's view of the tree stops diverging from what CopyTree would bundle.
func TestIgnoreAppliesToEveryDiscoveryTool(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":     "secret.log\ngenerated/\n",
		"secret.log":     "NEEDLE_IGNORED\n",
		"keep.txt":       "NEEDLE_KEPT\n",
		"generated/a.go": "NEEDLE_GENERATED\n",
	})

	if w := walked(root); w["secret.log"] || w["generated/a.go"] {
		t.Errorf("walk surfaced ignored paths: %v", w)
	} else if !w["keep.txt"] {
		t.Error("walk dropped a non-ignored file")
	}

	names := listedNames(t, root, "", 5)
	if names["secret.log"] || names["generated"] || names["generated/a.go"] {
		t.Errorf("fs.list surfaced ignored entries: %v", names)
	}
	if !names["keep.txt"] {
		t.Error("fs.list dropped a non-ignored file")
	}

	res := callFind(t, root, "*", 0)
	for _, f := range res.Result.(map[string]any)["files"].([]fileMatch) {
		if f.Path == "secret.log" || f.Path == "generated/a.go" {
			t.Errorf("fs.find surfaced an ignored path: %s", f.Path)
		}
	}

	for _, needle := range []string{"NEEDLE_IGNORED", "NEEDLE_GENERATED"} {
		sr := callSearch(t, root, needle, "")
		if !sr.Ok {
			t.Fatalf("search failed: %+v", sr.Error)
		}
		if m := sr.Result.(map[string]any)["matches"].([]searchMatch); len(m) != 0 {
			t.Errorf("fs.search matched inside an ignored file: %+v", m)
		}
	}
}

// TestIgnoreLayersGitThenCopyTreeAtEveryDirectory pins the CopyTree layering
// contract: .copytreeignore is loaded AFTER .gitignore in the same directory, so
// under last-match-wins its rule beats the git one on a conflict.
func TestIgnoreLayersGitThenCopyTreeAtEveryDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":      "*.md\n",
		".copytreeignore": "!README.md\nnotes.txt\n",
		"README.md":       "readme\n",
		"CHANGES.md":      "changes\n",
		"notes.txt":       "notes\n",
		"kept.txt":        "kept\n",
	})
	w := walked(root)
	if !w["README.md"] {
		t.Error("copytreeignore negation must re-include README.md over .gitignore's *.md")
	}
	if w["CHANGES.md"] {
		t.Error("CHANGES.md is still ignored by *.md")
	}
	if w["notes.txt"] {
		t.Error("copytreeignore's own exclusion must apply")
	}
	if !w["kept.txt"] {
		t.Error("unrelated file must survive")
	}
}

// TestIgnoreNestedFilesApplyBelowTheirOwnDirectory: an ignore file at depth binds
// only inside its own subtree, and its rules come after (so can override) the
// ancestors'.
func TestIgnoreNestedFilesApplyBelowTheirOwnDirectory(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":       "*.tmp\n",
		"a/.gitignore":     "!keep.tmp\nlocal.txt\n",
		"a/keep.tmp":       "kept\n",
		"a/drop.tmp":       "dropped\n",
		"a/local.txt":      "local\n",
		"b/local.txt":      "elsewhere\n",
		"b/anything.tmp":   "dropped\n",
		"b/c/deep/ok.go":   "ok\n",
		"b/c/deep/x.tmp":   "dropped\n",
		"b/c/.copytreeign": "unused\n",
	})
	w := walked(root)
	if !w["a/keep.tmp"] {
		t.Error("nested negation must re-include a/keep.tmp")
	}
	if w["a/drop.tmp"] || w["b/anything.tmp"] || w["b/c/deep/x.tmp"] {
		t.Errorf("root *.tmp must still hide unnegated .tmp files: %v", w)
	}
	if w["a/local.txt"] {
		t.Error("a/.gitignore must hide a/local.txt")
	}
	if !w["b/local.txt"] {
		t.Error("a/.gitignore must NOT reach into b/")
	}
	if !w["b/c/deep/ok.go"] {
		t.Error("ordinary deep file must survive")
	}
}

// TestIgnoreAnchoringAndDirectoryOnlyRules covers the three syntax rules that are
// easy to get wrong: a leading "/" pins to the declaring directory, a trailing "/"
// makes a rule directory-only, and a slashless pattern floats to any depth.
func TestIgnoreAnchoringAndDirectoryOnlyRules(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":       "/rooted.txt\nfloating.txt\nonlydir/\n",
		"rooted.txt":       "x\n",
		"sub/rooted.txt":   "x\n",
		"floating.txt":     "x\n",
		"sub/floating.txt": "x\n",
		"onlydir/a.txt":    "x\n",
		"sub/onlydir.txt":  "x\n",
	})
	w := walked(root)
	if w["rooted.txt"] {
		t.Error("/rooted.txt must be ignored at the root")
	}
	if !w["sub/rooted.txt"] {
		t.Error("/rooted.txt is anchored — it must NOT match sub/rooted.txt")
	}
	if w["floating.txt"] || w["sub/floating.txt"] {
		t.Errorf("a slashless pattern floats to any depth: %v", w)
	}
	if w["onlydir/a.txt"] {
		t.Error("onlydir/ must prune the directory")
	}
	if !w["sub/onlydir.txt"] {
		t.Error("a trailing-slash rule is directory-only — the .txt FILE must survive")
	}
}

// TestIgnoreSupportsDoubleStar: "**" spans directories, which stdlib
// filepath.Match cannot express at all.
func TestIgnoreSupportsDoubleStar(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":          "docs/**/*.md\n",
		"docs/a.md":           "x\n",
		"docs/deep/b.md":      "x\n",
		"docs/deep/more/c.md": "x\n",
		"docs/deep/keep.go":   "x\n",
		"other/d.md":          "x\n",
	})
	w := walked(root)
	for _, p := range []string{"docs/a.md", "docs/deep/b.md", "docs/deep/more/c.md"} {
		if w[p] {
			t.Errorf("docs/**/*.md should hide %s", p)
		}
	}
	if !w["docs/deep/keep.go"] {
		t.Error("non-.md under docs must survive")
	}
	if !w["other/d.md"] {
		t.Error("the pattern is anchored under docs/ — other/d.md must survive")
	}
}

// TestIgnoreCommentsBlanksAndEscapes: comments and blank lines are skipped, and a
// leading backslash escapes a literal "#" or "!".
func TestIgnoreCommentsBlanksAndEscapes(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore": "# a comment\n\n\\#hash.txt\ndrop.txt\n",
		"#hash.txt":  "x\n",
		"drop.txt":   "x\n",
		"comment":    "x\n",
	})
	w := walked(root)
	if !w["comment"] {
		t.Error("a '# a comment' line must be skipped, not turned into a pattern")
	}
	if w["#hash.txt"] {
		t.Error(`\#hash.txt escapes the comment marker — it must ignore the literal "#hash.txt"`)
	}
	if w["drop.txt"] {
		t.Error("drop.txt must be ignored")
	}
}

// TestSkipDirsWinOverIgnoreNegation: the hardcoded resource guard is
// unconditional — a .gitignore cannot negate node_modules back into the walk.
func TestSkipDirsWinOverIgnoreNegation(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":          "!node_modules/\n!node_modules/**\n!dist/\n",
		"node_modules/dep.js": "x\n",
		"dist/bundle.js":      "x\n",
		"src/app.js":          "x\n",
	})
	w := walked(root)
	if w["node_modules/dep.js"] || w["dist/bundle.js"] {
		t.Errorf("skipDirs must beat an ignore-file negation: %v", w)
	}
	if !w["src/app.js"] {
		t.Error("ordinary source must survive")
	}
}

// TestOversizedIgnoreFileIsSkipped: a pathological ignore file is skipped rather
// than failing the whole call — a partial ruleset still prunes better than none.
func TestOversizedIgnoreFileIsSkipped(t *testing.T) {
	root := writeTree(t, map[string]string{
		"keep.txt": "x\n",
	})
	huge := strings.Repeat("a-very-long-pattern-line\n", (maxIgnoreFileBytes/25)+64)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(huge+"keep.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := walked(root); !w["keep.txt"] {
		t.Error("an oversized ignore file must be skipped, not applied")
	}
}

// TestIgnoreDoesNotBlockAnExplicitlyNamedTarget pins the deliberate scope limit:
// ignore rules prune DISCOVERY, they never refuse a path the caller names. Reading
// a gitignored build output or listing a gitignored directory by exact path stays
// legal — only the secret guard refuses.
func TestIgnoreDoesNotBlockAnExplicitlyNamedTarget(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":     "generated/\n",
		"generated/a.go": "package generated\n",
	})
	if res := callRead(t, root, "generated/a.go", 0); !res.Ok {
		t.Errorf("fs.read of an explicitly named ignored file must still work: %+v", res.Error)
	}
	res := callList(t, root, "generated", 1)
	if !res.Ok {
		t.Fatalf("fs.list of an explicitly named ignored dir must still work: %+v", res.Error)
	}
	if len(res.Result.(map[string]any)["entries"].([]listEntry)) != 1 {
		t.Error("listing the named ignored dir should show its contents")
	}
}

// TestMatchGlobBasenameVsPathSemantics locks the rule that keeps "*.go" from being
// a silent footgun: with no "/" the pattern matches the NAME at any depth; with a
// "/" it matches the whole relative path.
func TestMatchGlobBasenameVsPathSemantics(t *testing.T) {
	cases := []struct {
		pattern, path string
		want          bool
	}{
		{"*.go", "main.go", true},
		{"*.go", "internal/tools/fsx/fsx.go", true}, // floats to any depth
		{"*.go", "main.rs", false},
		{".go", "main.go", false}, // a bare extension is not a suffix filter
		{"**/*.go", "internal/a.go", true},
		{"**/*.go", "a.go", true}, // ** matches zero directories
		{"internal/**/*.go", "internal/tools/x.go", true},
		{"internal/**/*.go", "cmd/x.go", false},
		{"Dockerfile", "deploy/Dockerfile", true},
		{"deploy/Dockerfile", "deploy/Dockerfile", true},
		{"deploy/Dockerfile", "other/deploy/Dockerfile", false},
	}
	for _, c := range cases {
		got, err := matchGlob(c.pattern, c.path)
		if err != nil {
			t.Fatalf("matchGlob(%q,%q): %v", c.pattern, c.path, err)
		}
		if got != c.want {
			t.Errorf("matchGlob(%q, %q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}
