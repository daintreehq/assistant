package fsx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fileText = "hello daintree\nfind-me-needle here\nthird line\n"

// buildFunctionalFixture builds the functional fixture: a readme + a nested subdir.
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

// TestReadReturnsContentAndMaxBytes: fs.read returns the
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

// TestReadBlocksTraversal: a ../ traversal is blocked and
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

// TestListNameAndType: fs.list reports each entry's name and
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

// TestListDescendsWithDepth: depth:2 surfaces a nested file
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

// TestSearchFindsFileLineText: fs.search reports the file,
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

// TestSearchUsesDoublestarGlob replaces the old suffix-filter test. `glob` is a
// real pattern now: "*.txt" floats to any depth, a path-bearing pattern anchors,
// and a bare ".txt" correctly matches nothing (it is not a suffix filter).
func TestSearchUsesDoublestarGlob(t *testing.T) {
	root := buildFunctionalFixture(t)

	hits := func(glob string) []searchMatch {
		t.Helper()
		res := callSearch(t, root, "body", glob)
		if !res.Ok {
			t.Fatalf("search glob %q failed: %+v", glob, res.Error)
		}
		return res.Result.(map[string]any)["matches"].([]searchMatch)
	}
	has := func(ms []searchMatch, file string) bool {
		for _, m := range ms {
			if m.File == file {
				return true
			}
		}
		return false
	}

	if !has(hits("*.txt"), "sub/nested.txt") {
		t.Error(`"*.txt" must match a nested file (a slashless pattern floats to any depth)`)
	}
	if !has(hits("sub/**"), "sub/nested.txt") {
		t.Error(`"sub/**" must match sub/nested.txt`)
	}
	if has(hits("other/**"), "sub/nested.txt") {
		t.Error(`"other/**" must not match under sub/`)
	}
	if ms := hits(".txt"); len(ms) != 0 {
		t.Errorf(`a bare ".txt" is a pattern, not a suffix filter — want no matches, got %+v`, ms)
	}
}

// TestReadReturnsByteWindow: byteOffset seeks straight to a mid-file window and
// reports where it sat, so byteEnd can be fed back in to page forward.
func TestReadReturnsByteWindow(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callReadArgs(t, root, map[string]any{"path": "readme.txt", "byteOffset": 15, "maxBytes": 5})
	if !res.Ok {
		t.Fatalf("byte window read failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if got := m["content"].(string); got != "find-" {
		t.Errorf("content = %q, want %q", got, "find-")
	}
	if m["byteStart"].(int64) != 15 || m["byteEnd"].(int64) != 20 {
		t.Errorf("byteStart/byteEnd = %v/%v, want 15/20", m["byteStart"], m["byteEnd"])
	}
	if m["size"].(int64) != int64(len(fileText)) {
		t.Errorf("size = %v, want %d", m["size"], len(fileText))
	}
	if !m["truncated"].(bool) {
		t.Error("truncated must be true while content remains after the window")
	}
	// Paging with the returned byteEnd continues exactly where we left off.
	next := callReadArgs(t, root, map[string]any{"path": "readme.txt", "byteOffset": m["byteEnd"], "maxBytes": 2})
	if !next.Ok {
		t.Fatalf("paged read failed: %+v", next.Error)
	}
	if got := next.Result.(map[string]any)["content"].(string); got != "me" {
		t.Errorf("paged content = %q, want %q", got, "me")
	}
}

// TestReadRejectsByteOffsetPastEnd: an offset at or past EOF is an honest error,
// not a silent empty read the model would misinterpret as an empty file.
func TestReadRejectsByteOffsetPastEnd(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callReadArgs(t, root, map[string]any{"path": "readme.txt", "byteOffset": 9999})
	if res.Ok {
		t.Fatal("a byteOffset past EOF must fail")
	}
	if res.Error.Code != codeFSRead {
		t.Errorf("code = %s, want %s", res.Error.Code, codeFSRead)
	}
}

// TestReadReturnsInclusiveLineWindow: lineStart/lineEnd are 1-based and inclusive,
// and the result echoes the window actually served.
func TestReadReturnsInclusiveLineWindow(t *testing.T) {
	root := buildFunctionalFixture(t)

	res := callReadArgs(t, root, map[string]any{"path": "readme.txt", "lineStart": 2, "lineEnd": 2})
	if !res.Ok {
		t.Fatalf("line window read failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if got := m["content"].(string); got != "find-me-needle here\n" {
		t.Errorf("content = %q, want the second line", got)
	}
	if m["lineStart"].(int) != 2 || m["lineEnd"].(int) != 2 {
		t.Errorf("lineStart/lineEnd = %v/%v, want 2/2", m["lineStart"], m["lineEnd"])
	}
	if m["byteStart"].(int64) != 15 {
		t.Errorf("byteStart = %v, want 15", m["byteStart"])
	}

	// A multi-line window concatenates the lines verbatim.
	res = callReadArgs(t, root, map[string]any{"path": "readme.txt", "lineStart": 1, "lineEnd": 2})
	if !res.Ok {
		t.Fatalf("multi-line window failed: %+v", res.Error)
	}
	if got := res.Result.(map[string]any)["content"].(string); got != "hello daintree\nfind-me-needle here\n" {
		t.Errorf("multi-line content = %q", got)
	}

	// The last line is reachable and reports truncated=false (nothing follows).
	res = callReadArgs(t, root, map[string]any{"path": "readme.txt", "lineStart": 3, "lineEnd": 3})
	if !res.Ok {
		t.Fatalf("last-line window failed: %+v", res.Error)
	}
	m = res.Result.(map[string]any)
	if got := m["content"].(string); got != "third line\n" {
		t.Errorf("last line = %q", got)
	}
	if m["truncated"].(bool) {
		t.Error("nothing follows the last line — truncated must be false")
	}
}

// TestReadLineWindowPastEndOfFile: asking past the final line says so rather than
// returning an empty window (the trailing-newline phantom-line trap).
func TestReadLineWindowPastEndOfFile(t *testing.T) {
	root := buildFunctionalFixture(t)
	res := callReadArgs(t, root, map[string]any{"path": "readme.txt", "lineStart": 9, "lineEnd": 9})
	if res.Ok {
		t.Fatalf("a line window past EOF must fail, got %+v", res.Result)
	}
	if !contains(res.Error.Message, "only 3 lines") {
		t.Errorf("error should report the real line count, got %q", res.Error.Message)
	}
}

// TestReadWindowStillSniffsFilePrefixForBinary: the binary refusal must judge the
// FILE, not just the returned window — otherwise seeking past a NUL-bearing header
// walks straight through a guard that a plain head read enforces.
func TestReadWindowStillSniffsFilePrefixForBinary(t *testing.T) {
	root := t.TempDir()
	// A NUL-prefixed blob with a stretch of clean ASCII further in.
	body := append([]byte{0x00, 0x01, 0x02, 0x00}, []byte("HDR\nplain readable text\n")...)
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range []map[string]any{
		{"path": "blob.bin", "byteOffset": 8},
		{"path": "blob.bin", "lineStart": 2, "lineEnd": 2},
		{"path": "blob.bin"},
	} {
		res := callReadArgs(t, root, args)
		if res.Ok || res.Error.Code != codeFSBinary {
			t.Errorf("a window into a binary file must still be FS_BINARY (%v), got %+v", args, res)
		}
	}
}

// TestReadLineWindowRespectsScanBudget: past the 1MB scan budget the tool must
// steer to byteOffset rather than serve a line it never fully read.
func TestReadLineWindowRespectsScanBudget(t *testing.T) {
	root := t.TempDir()
	// Each line is 10 bytes, so line 100001 starts at byte 1_000_000 — exactly at
	// the budget, hence unreachable by a line window.
	var b strings.Builder
	for i := 0; i < 100_050; i++ {
		b.WriteString("abcdefghi\n")
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callReadArgs(t, root, map[string]any{"path": "big.txt", "lineStart": 100_020, "lineEnd": 100_020})
	if res.Ok {
		t.Fatalf("a line beyond the scan budget must fail, got %+v", res.Result)
	}
	if !contains(res.Error.Message, "byteOffset") {
		t.Errorf("the error must steer to byteOffset, got %q", res.Error.Message)
	}
	// A line inside the budget still works, so the budget isn't just failing always.
	if ok := callReadArgs(t, root, map[string]any{"path": "big.txt", "lineStart": 5, "lineEnd": 5}); !ok.Ok {
		t.Errorf("a line inside the budget must still read: %+v", ok.Error)
	}
}

// TestReadLineWindowMaxBytesAdjustsLineEnd: when maxBytes cuts the window short,
// the reported lineEnd must describe what was actually returned. Claiming lines it
// did not serve would make the model skip them.
func TestReadLineWindowMaxBytesAdjustsLineEnd(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "l.txt"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callReadArgs(t, root, map[string]any{"path": "l.txt", "lineStart": 1, "lineEnd": 3, "maxBytes": 5})
	if !res.Ok {
		t.Fatalf("read failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if got := m["content"].(string); got != "one\nt" {
		t.Fatalf("content = %q, want %q", got, "one\nt")
	}
	if got := m["lineEnd"].(int); got != 2 {
		t.Errorf("lineEnd = %d, want 2 — the window only reaches into line 2", got)
	}
	if !m["truncated"].(bool) {
		t.Error("a maxBytes-cut window must report truncated")
	}
}

// TestReadEmptyFileByteOffsets: offset 0 on an empty file is the one valid read;
// any positive offset is out of bounds and must not report impossible coordinates.
func TestReadEmptyFileByteOffsets(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if res := callReadArgs(t, root, map[string]any{"path": "empty.txt", "byteOffset": 0}); !res.Ok {
		t.Errorf("byteOffset 0 on an empty file should succeed: %+v", res.Error)
	}
	if res := callReadArgs(t, root, map[string]any{"path": "empty.txt", "byteOffset": 1}); res.Ok {
		t.Errorf("a positive byteOffset on an empty file must fail, got %+v", res.Result)
	}
}

// TestSearchRejectsEmptyQueryAtDecode: StrictDecoder does not run the JSON Schema,
// so `required`/`minLength` are advisory. Without a Validate() check an empty query
// reaches strings.Contains(line, "") — true for every line — and dumps maxResults
// worth of unrelated content after a full project walk.
func TestSearchRejectsEmptyQueryAtDecode(t *testing.T) {
	tool := newSearchTool()
	for _, bad := range []string{`{}`, `{"query":""}`, `{"query":"","glob":"*.go"}`} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("an empty/missing query must be rejected at decode: %s", bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"query":"needle"}`)); err != nil {
		t.Errorf("a real query should decode: %v", err)
	}
}

// TestListReportsFileSizesOnly: files carry a byte size (including 0 for an empty
// file), directories carry none.
func TestListReportsFileSizesOnly(t *testing.T) {
	root := buildFunctionalFixture(t)
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	res := callList(t, root, "", 1)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	byName := map[string]listEntry{}
	for _, e := range res.Result.(map[string]any)["entries"].([]listEntry) {
		byName[e.Name] = e
	}
	if e := byName["readme.txt"]; e.Size == nil || *e.Size != int64(len(fileText)) {
		t.Errorf("readme.txt size = %v, want %d", e.Size, len(fileText))
	}
	if e := byName["empty.txt"]; e.Size == nil || *e.Size != 0 {
		t.Errorf("an empty file must report size 0, not omit it (got %v)", e.Size)
	}
	if e := byName["sub"]; e.Size != nil {
		t.Errorf("a directory must not report a size, got %v", *e.Size)
	}
}

// TestListCapsAfterDeterministicSort: the cap is applied to the SORTED list, so
// the truncated result is a stable lexical prefix rather than an arbitrary
// traversal-order sample.
func TestListCapsAfterDeterministicSort(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"e.txt", "b.txt", "d.txt", "a.txt", "c.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := json.Marshal(map[string]any{"depth": 1, "maxEntries": 3})
	res := callTool(t, newListTool(), root, string(b))
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	if !m["capped"].(bool) {
		t.Error("capped must be true when entries were dropped")
	}
	entries := m["entries"].([]listEntry)
	if len(entries) != 3 {
		t.Fatalf("len(entries) = %d, want 3", len(entries))
	}
	for i, want := range []string{"a.txt", "b.txt", "c.txt"} {
		if entries[i].Name != want {
			t.Errorf("entries[%d] = %q, want %q (cap must follow the sort)", i, entries[i].Name, want)
		}
	}
}

// TestFindMatchesByNameAndPath: fs.find is the filename counterpart to fs.search,
// returning sorted paths with sizes.
func TestFindMatchesByNameAndPath(t *testing.T) {
	root := buildFunctionalFixture(t)
	files := callFind(t, root, "*.txt", 0).Result.(map[string]any)["files"].([]fileMatch)
	if len(files) != 2 {
		t.Fatalf("want 2 .txt files, got %+v", files)
	}
	if files[0].Path != "readme.txt" || files[1].Path != "sub/nested.txt" {
		t.Errorf("results must be sorted by path, got %+v", files)
	}
	if files[0].Size != int64(len(fileText)) {
		t.Errorf("size = %d, want %d", files[0].Size, len(fileText))
	}

	if f := callFind(t, root, "sub/**", 0).Result.(map[string]any)["files"].([]fileMatch); len(f) != 1 || f[0].Path != "sub/nested.txt" {
		t.Errorf(`"sub/**" should match only sub/nested.txt, got %+v`, f)
	}
	if f := callFind(t, root, "*.rs", 0).Result.(map[string]any)["files"].([]fileMatch); len(f) != 0 {
		t.Errorf("a non-matching glob must return no files, got %+v", f)
	}
}

// TestFindCapsDeterministically: like fs.list, the cap follows the sort.
func TestFindCapsDeterministically(t *testing.T) {
	root := t.TempDir()
	for _, n := range []string{"e.go", "b.go", "d.go", "a.go", "c.go"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("package x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := callFind(t, root, "*.go", 2)
	m := res.Result.(map[string]any)
	if !m["capped"].(bool) {
		t.Error("capped must be true when results were dropped")
	}
	files := m["files"].([]fileMatch)
	if len(files) != 2 || files[0].Path != "a.go" || files[1].Path != "b.go" {
		t.Errorf("cap must follow the sort, got %+v", files)
	}
}

// TestReadSecretAndBinaryVariants exercises the read guards: a
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
