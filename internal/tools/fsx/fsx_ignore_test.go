package fsx

import (
	"os"
	"path/filepath"
	"runtime"
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
		".gitignore":     "*.tmp\n",
		"a/.gitignore":   "!keep.tmp\nlocal.txt\n",
		"a/keep.tmp":     "kept\n",
		"a/drop.tmp":     "dropped\n",
		"a/local.txt":    "local\n",
		"b/local.txt":    "elsewhere\n",
		"b/anything.tmp": "dropped\n",
		"b/c/deep/ok.go": "ok\n",
		"b/c/deep/x.tmp": "dropped\n",
		// A nested .copytreeignore binds below its own directory too, and layers
		// after the nested .gitignore at the same level.
		"b/c/.gitignore":      "*.cfg\n",
		"b/c/.copytreeignore": "!keepme.cfg\ndropme.log\n",
		"b/c/keepme.cfg":      "kept\n",
		"b/c/other.cfg":       "dropped\n",
		"b/c/dropme.log":      "dropped\n",
		"b/dropme.log":        "sibling — untouched by b/c rules\n",
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
	// The nested .copytreeignore layers after the nested .gitignore at b/c.
	if !w["b/c/keepme.cfg"] {
		t.Error("nested .copytreeignore negation must beat the nested .gitignore's *.cfg")
	}
	if w["b/c/other.cfg"] {
		t.Error("b/c/.gitignore's *.cfg must still apply")
	}
	if w["b/c/dropme.log"] {
		t.Error("b/c/.copytreeignore must hide b/c/dropme.log")
	}
	if !w["b/dropme.log"] {
		t.Error("b/c's ignore files must NOT reach up into b/")
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

// TestListNonRootPathIgnoreAnchoring covers the trickiest anchoring case: listing
// a NON-ROOT path. Rules declared above the target still bind inside it (that is
// what ancestorIgnoreRules is for), rules in the target itself bind, and rules
// nested below it bind — all while entry names stay relative to the target.
func TestListNonRootPathIgnoreAnchoring(t *testing.T) {
	root := writeTree(t, map[string]string{
		".gitignore":       "*.log\n",
		"a/.gitignore":     "ancestor.txt\n",
		"a/b/.gitignore":   "nested.txt\n",
		"a/b/keep.go":      "x\n",
		"a/b/nested.txt":   "x\n",
		"a/b/drop.log":     "x\n",
		"a/b/ancestor.txt": "x\n",
		"a/b/c/deep.go":    "x\n",
		"a/b/c/deep.log":   "x\n",
	})
	names := listedNames(t, root, "a/b", 3)
	if !names["keep.go"] || !names["c/deep.go"] {
		t.Errorf("ordinary files must be listed: %v", names)
	}
	if names["nested.txt"] {
		t.Error("the listing target's own .gitignore must apply")
	}
	if names["ancestor.txt"] {
		t.Error("an ANCESTOR .gitignore must still bind inside the listing target")
	}
	if names["drop.log"] || names["c/deep.log"] {
		t.Errorf("the root .gitignore's *.log must bind at any depth below the target: %v", names)
	}
}

// TestIgnoreSymlinkedIgnoreFileIsNotFollowed is a containment test. A symlinked
// .gitignore must be refused outright, in BOTH walkers: following one would let a
// project point the parser outside the project (or at its own .env), and since
// ignore rules change which paths come back, that is an observable oracle over the
// linked file's contents — not a harmless parse.
func TestIgnoreSymlinkedIgnoreFileIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	outside := t.TempDir()
	rules := filepath.Join(outside, "outside-rules")
	if err := os.WriteFile(rules, []byte("hidden.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	root := writeTree(t, map[string]string{
		"hidden.txt":  "x\n",
		"visible.txt": "x\n",
	})
	if err := os.Symlink(rules, filepath.Join(root, ".gitignore")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	if w := walked(root); !w["hidden.txt"] {
		t.Error("walkFiles followed a symlinked .gitignore out of the project root")
	}
	if names := listedNames(t, root, "", 1); !names["hidden.txt"] {
		t.Error("fs.list followed a symlinked .gitignore out of the project root")
	}
	// Both walkers must agree — that consistency is the point of the shared reader.
	found := callFind(t, root, "hidden.txt", 0).Result.(map[string]any)["files"].([]fileMatch)
	if len(found) != 1 {
		t.Errorf("fs.find must agree with fs.list about the symlinked ignore file, got %+v", found)
	}
}

// TestIgnoreRelativeInProjectSymlinkIsNotFollowed is the case the outside-symlink
// test above does NOT cover: a symlink whose target stays inside the project is
// happily followed by os.Root, so only the explicit regular-file check refuses it.
// That matters because the target could be the project's own .env — ignore rules
// change which paths come back, making it an oracle over the linked file.
func TestIgnoreRelativeInProjectSymlinkIsNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	root := writeTree(t, map[string]string{
		"rules.txt":   "hidden.txt\n",
		"hidden.txt":  "x\n",
		"visible.txt": "x\n",
	})
	// Relative, and resolves INSIDE the root — os.Root.Open would follow this.
	if err := os.Symlink("rules.txt", filepath.Join(root, ".gitignore")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	if w := walked(root); !w["hidden.txt"] {
		t.Error("a symlinked .gitignore must be refused even when its target is in-project")
	}
	if names := listedNames(t, root, "", 1); !names["hidden.txt"] {
		t.Error("fs.list must refuse a symlinked .gitignore with an in-project target")
	}
	// Positive control: the SAME rules in a regular .gitignore do take effect, so
	// the assertions above are not passing against an inert ignore layer.
	root2 := writeTree(t, map[string]string{
		".gitignore": "hidden.txt\n", "hidden.txt": "x\n", "visible.txt": "x\n",
	})
	if w := walked(root2); w["hidden.txt"] {
		t.Error("positive control failed — a regular .gitignore should hide hidden.txt")
	}
}

// TestOversizedIgnoreFileIsBounded: the size limit must bound the READ, not just
// reject after the fact — otherwise a pathological ignore file is fully allocated
// before being "skipped".
func TestOversizedIgnoreFileIsBounded(t *testing.T) {
	root := writeTree(t, map[string]string{"keep.txt": "x\n", "drop.txt": "x\n"})
	// Well over the cap, with a real rule at the very end that must NOT take effect.
	huge := strings.Repeat("#"+strings.Repeat("p", 120)+"\n", (maxIgnoreFileBytes/121)+512)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(huge+"drop.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w := walked(root)
	if !w["keep.txt"] || !w["drop.txt"] {
		t.Errorf("an over-cap ignore file must be skipped wholesale, got %v", w)
	}
	// fs.list goes through the same reader and must agree.
	if names := listedNames(t, root, "", 1); !names["drop.txt"] {
		t.Error("fs.list must also skip an over-cap ignore file")
	}
	// Positive control: the same rule in a normal-sized file DOES apply, proving
	// the test above isn't just observing a no-op ignore layer.
	root2 := writeTree(t, map[string]string{
		".gitignore": "drop.txt\n", "keep.txt": "x\n", "drop.txt": "x\n",
	})
	if w2 := walked(root2); w2["drop.txt"] || !w2["keep.txt"] {
		t.Errorf("positive control failed — ignore rules are not being applied at all: %v", w2)
	}
}

// TestIgnoreEscapedMetacharacterStaysLiteral: git writes a literal "*.txt" as
// "\*.txt". Stripping that backslash unconditionally would turn it into a live
// glob that hides every text file.
func TestIgnoreEscapedMetacharacterStaysLiteral(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip(`"*" is not a legal filename character on Windows`)
	}
	root := writeTree(t, map[string]string{
		".gitignore": "\\*.txt\n",
		"*.txt":      "the literally-named file\n",
		"normal.txt": "x\n",
	})
	w := walked(root)
	if !w["normal.txt"] {
		t.Error(`\*.txt is a LITERAL name — it must not hide every .txt file`)
	}
	if w["*.txt"] {
		t.Error(`\*.txt should still hide the file literally named "*.txt"`)
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
