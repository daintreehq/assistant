package fsx

import (
	"os"
	"path/filepath"
	"testing"
)

const fileText = "hello daintree\nfind-me-needle here\nthird line\n"

// buildFunctionalFixture mirrors fsTools.test.ts: a readme + a nested subdir.
func buildFunctionalFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "readme.txt"), []byte(fileText), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "sub", "nested.txt"), []byte("nested body\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestReadReturnsContentAndMaxBytes ports fsTools.test.ts: fs.read returns the
// full content, and honours maxBytes by slicing.
func TestReadReturnsContentAndMaxBytes(t *testing.T) {
	root := buildFunctionalFixture(t)

	res := callRead(t, root, "readme.txt", 0)
	if !res.Ok {
		t.Fatalf("read failed: %+v", res.Error)
	}
	if got := res.Result.(map[string]any)["content"].(string); got != fileText {
		t.Fatalf("content mismatch: %q", got)
	}

	res = callRead(t, root, "readme.txt", 5)
	if !res.Ok {
		t.Fatalf("read maxBytes failed: %+v", res.Error)
	}
	if got := res.Result.(map[string]any)["content"].(string); got != "hello" {
		t.Fatalf("maxBytes slice = %q want %q", got, "hello")
	}
}

// TestReadBlocksTraversal ports fsTools.test.ts: a ../ traversal is blocked and
// returns FS_READ.
func TestReadBlocksTraversal(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callRead(t, root, "../outside", 0)
	if res.Ok {
		t.Fatal("traversal must be blocked")
	}
	if res.Error.Code != codeFSRead {
		t.Fatalf("traversal block should be FS_READ, got %s", res.Error.Code)
	}
}

// TestListNameAndType ports fsTools.test.ts: fs.list reports each entry's name and
// type (file vs dir) under the project root.
func TestListNameAndType(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callList(t, root, "", 1)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	entries := res.Result.(map[string]any)["entries"].([]listEntry)
	byName := map[string]string{}
	for _, e := range entries {
		byName[e.Name] = e.Type
	}
	if byName["readme.txt"] != "file" {
		t.Errorf("readme.txt type = %q want file", byName["readme.txt"])
	}
	if byName["sub"] != "dir" {
		t.Errorf("sub type = %q want dir", byName["sub"])
	}
}

// TestListDescendsWithDepth ports fsTools.test.ts: depth:2 surfaces a nested file
// as a path-joined entry.
func TestListDescendsWithDepth(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callList(t, root, "", 2)
	if !res.Ok {
		t.Fatalf("list depth failed: %+v", res.Error)
	}
	entries := res.Result.(map[string]any)["entries"].([]listEntry)
	found := false
	for _, e := range entries {
		if e.Name == "sub/nested.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("depth:2 should surface sub/nested.txt")
	}
}

// TestSearchFindsFileLineText ports fsTools.test.ts: fs.search reports the file,
// 1-based line number, and matching text.
func TestSearchFindsFileLineText(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callSearch(t, root, "find-me-needle", "")
	if !res.Ok {
		t.Fatalf("search failed: %+v", res.Error)
	}
	matches := res.Result.(map[string]any)["matches"].([]searchMatch)
	var hit *searchMatch
	for i := range matches {
		if matches[i].File == "readme.txt" {
			hit = &matches[i]
		}
	}
	if hit == nil {
		t.Fatal("expected a match in readme.txt")
	}
	if hit.Line != 2 {
		t.Errorf("line = %d want 2 (1-based)", hit.Line)
	}
	if want := "find-me-needle"; !contains(hit.Text, want) {
		t.Errorf("text %q should contain %q", hit.Text, want)
	}
}

// TestSearchRespectsGlobSuffix ports fsTools.test.ts: the glob acts as a filename
// suffix filter.
func TestSearchRespectsGlobSuffix(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callSearch(t, root, "body", ".txt")
	if !res.Ok {
		t.Fatalf("search glob failed: %+v", res.Error)
	}
	matches := res.Result.(map[string]any)["matches"].([]searchMatch)
	found := false
	for _, m := range matches {
		if m.File == "sub/nested.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("glob '.txt' should still match sub/nested.txt")
	}
}

// TestReadSecretAndBinaryVariants ports fsToolsSecurity.test.ts read guards: a
// .env is FS_SENSITIVE, a private key is refused, a NUL-byte file is FS_BINARY,
// and an ordinary source file still reads.
func TestReadSecretAndBinaryVariants(t *testing.T) {
	root := buildSecurityFixture(t)

	if res := callRead(t, root, ".env", 0); res.Ok || res.Error.Code != codeFSSensitive {
		t.Errorf(".env should be FS_SENSITIVE, got %+v", res)
	}
	if res := callRead(t, root, "server.key", 0); res.Ok {
		t.Error("private key should be refused")
	}
	if res := callRead(t, root, "blob.bin", 0); res.Ok || res.Error.Code != codeFSBinary {
		t.Errorf("binary should be FS_BINARY, got %+v", res)
	}
	if res := callRead(t, root, "app.ts", 0); !res.Ok {
		t.Errorf("ordinary source should read, got %+v", res.Error)
	} else if !contains(res.Result.(map[string]any)["content"].(string), "readEnv") {
		t.Error("content should include readEnv")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return len(sub) == 0
}
