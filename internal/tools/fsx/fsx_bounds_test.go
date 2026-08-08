package fsx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// Finding 4: a negative maxBytes on fs.read previously reached make([]byte, toRead)
// with a negative length and panicked. It must now be rejected as INVALID_ARGS at
// decode (the Zod int().positive().max(200000) bound), and an oversized one too.
func TestReadRejectsOutOfBoundsMaxBytes(t *testing.T) {
	tool := newReadTool()
	for _, bad := range []string{
		`{"path":"x.txt","maxBytes":-1}`,
		`{"path":"x.txt","maxBytes":0}`,
		`{"path":"x.txt","maxBytes":999999}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("maxBytes out of bounds should be rejected at decode: %s", bad)
		}
	}
	// A valid in-range maxBytes still decodes.
	if _, err := tool.Decode(json.RawMessage(`{"path":"x.txt","maxBytes":10}`)); err != nil {
		t.Errorf("valid maxBytes should decode: %v", err)
	}
}

// Finding 4: fs.list depth must be int().positive().max(10).
func TestListRejectsOutOfBoundsDepth(t *testing.T) {
	tool := newListTool()
	for _, bad := range []string{`{"depth":-1}`, `{"depth":0}`, `{"depth":11}`} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("depth out of bounds should be rejected: %s", bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"depth":5}`)); err != nil {
		t.Errorf("valid depth should decode: %v", err)
	}
}

// Finding 4: fs.search maxResults must be int().positive().max(500).
func TestSearchRejectsOutOfBoundsMaxResults(t *testing.T) {
	tool := newSearchTool()
	for _, bad := range []string{`{"query":"q","maxResults":-1}`, `{"query":"q","maxResults":0}`, `{"query":"q","maxResults":501}`} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("maxResults out of bounds should be rejected: %s", bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"query":"q","maxResults":50}`)); err != nil {
		t.Errorf("valid maxResults should decode: %v", err)
	}
}

// Finding 6: a benign-named symlink that resolves OUTSIDE the project root must not
// let fs.read leak the target's contents. The confined os.Root refuses the
// out-of-root symlink at open time, so this can't escape even if a Stat saw a
// different (in-root) inode first (the TOCTOU swap window is gone).
func TestReadRefusesSymlinkEscapingRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "loot.txt")
	if err := os.WriteFile(secretPath, []byte("TOP_SECRET_OUTSIDE_ROOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "innocent.txt")
	if err := os.Symlink(secretPath, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	tool := newReadTool()
	decoded, err := tool.Decode(json.RawMessage(`{"path":"innocent.txt"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	res := tool.Handle(context.Background(), decoded, tctx(root))
	if res.Ok {
		content, _ := res.Result.(map[string]any)["content"].(string)
		t.Fatalf("fs.read followed a symlink out of the project root; leaked %q", content)
	}
}

// Finding 6: fs.search must not read through a symlink that escapes the root either.
func TestSearchRefusesSymlinkEscapingRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	root := t.TempDir()
	outside := t.TempDir()
	secretPath := filepath.Join(outside, "loot.txt")
	if err := os.WriteFile(secretPath, []byte("NEEDLE_OUTSIDE_ROOT"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secretPath, filepath.Join(root, "innocent.txt")); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	res := callSearch(t, root, "NEEDLE_OUTSIDE_ROOT", "")
	if !res.Ok {
		t.Fatalf("search failed: %+v", res.Error)
	}
	if matches := res.Result.(map[string]any)["matches"].([]searchMatch); len(matches) != 0 {
		t.Fatalf("fs.search read through an escaping symlink; leaked %+v", matches)
	}
}

// The window args carry the same decode-time discipline as maxBytes: every bound
// the schema advertises is enforced by Validate(), and the two modes are mutually
// exclusive because they answer the same question in different coordinates.
func TestReadRejectsInvalidWindowArgs(t *testing.T) {
	tool := newReadTool()
	bad := []string{
		`{"path":"x.txt","byteOffset":-1}`,
		`{"path":"x.txt","lineStart":1}`,                // lineEnd missing
		`{"path":"x.txt","lineEnd":2}`,                  // lineStart missing
		`{"path":"x.txt","lineStart":0,"lineEnd":2}`,    // 1-based
		`{"path":"x.txt","lineStart":5,"lineEnd":2}`,    // inverted
		`{"path":"x.txt","lineStart":1,"lineEnd":1001}`, // window too wide
		`{"path":"x.txt","lineStart":1000001,"lineEnd":1000002}`,
		`{"path":"x.txt","byteOffset":5,"lineStart":1,"lineEnd":2}`, // modes are exclusive
		`{"path":"","maxBytes":10}`,                                 // empty path
		`{"path":"x.txt","offset":5}`,                               // unknown field
	}
	for _, b := range bad {
		if _, err := tool.Decode(json.RawMessage(b)); err == nil {
			t.Errorf("should be rejected at decode: %s", b)
		}
	}
	for _, good := range []string{
		`{"path":"x.txt","byteOffset":0}`,
		`{"path":"x.txt","byteOffset":4096,"maxBytes":100}`,
		`{"path":"x.txt","lineStart":10,"lineEnd":20}`,
		`{"path":"x.txt","lineStart":1,"lineEnd":1000}`,
	} {
		if _, err := tool.Decode(json.RawMessage(good)); err != nil {
			t.Errorf("valid window args should decode (%s): %v", good, err)
		}
	}
}

// fs.list maxEntries is bounded 1..1000 — an unbounded result is exactly the gap
// this arg closes, so a bad value must not silently fall through to "no cap".
func TestListRejectsOutOfBoundsMaxEntries(t *testing.T) {
	tool := newListTool()
	for _, bad := range []string{`{"maxEntries":-1}`, `{"maxEntries":0}`, `{"maxEntries":1001}`} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("maxEntries out of bounds should be rejected: %s", bad)
		}
	}
	if _, err := tool.Decode(json.RawMessage(`{"maxEntries":500}`)); err != nil {
		t.Errorf("valid maxEntries should decode: %v", err)
	}
}

// A malformed glob is rejected at DECODE on both pattern-taking tools, so the
// model gets an INVALID_ARGS it can fix rather than a successful call that
// silently matched nothing.
func TestGlobsRejectedAtDecode(t *testing.T) {
	find := newFindTool()
	for _, bad := range []string{
		`{"glob":"["}`,  // unterminated character class
		`{"glob":""}`,   // empty is not "match everything"
		`{"glob":"  "}`, // whitespace-only
		`{"glob":"*","maxResults":0}`,
		`{"glob":"*","maxResults":1001}`,
	} {
		if _, err := find.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("fs.find should reject at decode: %s", bad)
		}
	}
	for _, good := range []string{`{"glob":"*.go"}`, `{"glob":"internal/**/*.go","maxResults":50}`} {
		if _, err := find.Decode(json.RawMessage(good)); err != nil {
			t.Errorf("valid fs.find args should decode (%s): %v", good, err)
		}
	}

	search := newSearchTool()
	if _, err := search.Decode(json.RawMessage(`{"query":"q","glob":"["}`)); err == nil {
		t.Error("fs.search should reject a malformed glob at decode")
	}
	if _, err := search.Decode(json.RawMessage(`{"query":"q","glob":"**/*.ts"}`)); err != nil {
		t.Errorf("valid fs.search glob should decode: %v", err)
	}
}

// The backend forwards each tool's Schema to the model verbatim, so a bound that
// lives only in the description prose does not actually steer the call. Every
// numeric arg in this family must carry real minimum/maximum keywords.
func TestSchemasEncodeBoundsAsRealKeywords(t *testing.T) {
	for _, tool := range Tools(Deps{}) {
		var schema struct {
			Properties map[string]struct {
				Type    string   `json:"type"`
				Minimum *float64 `json:"minimum"`
				Maximum *float64 `json:"maximum"`
			} `json:"properties"`
			AdditionalProperties *bool `json:"additionalProperties"`
		}
		if err := json.Unmarshal(tool.Schema, &schema); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", tool.Name, err)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("%s: schema must set additionalProperties:false", tool.Name)
		}
		for name, prop := range schema.Properties {
			if prop.Type != "integer" && prop.Type != "number" {
				continue
			}
			if prop.Minimum == nil || prop.Maximum == nil {
				t.Errorf("%s.%s is numeric but has no minimum/maximum keyword — prose bounds don't reach the model", tool.Name, name)
			}
		}
	}
}

// fs.read still reads ordinary in-root files (the os.Root change is non-regressive).
func TestReadStillReadsInRootFile(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callRead(t, root, "ok.txt", 0)
	if !res.Ok {
		t.Fatalf("in-root read should succeed: %+v", res.Error)
	}
	if got := res.Result.(map[string]any)["content"].(string); got != "plain text" {
		t.Fatalf("content = %q, want plain text", got)
	}
}
