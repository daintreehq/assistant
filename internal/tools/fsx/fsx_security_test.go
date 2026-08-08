package fsx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// buildSecurityFixture lays out the security fixture tree: a .env, a
// private key, an ordinary source file, a NUL-byte binary, several credential
// stores at varying depths (each with a unique marker so a leak would surface),
// an ordinary sibling, an uppercase credential dir, and (when supported) a
// benign-named symlink pointing at a credential dir.
func buildSecurityFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".env", "DEEPSEEK_API_KEY=sk-secret-value\n")
	write("server.key", "-----BEGIN PRIVATE KEY-----\nsecret\n")
	write("app.ts", "const apiKey = readEnv();\n")
	if err := os.WriteFile(filepath.Join(root, "blob.bin"), []byte{0x00, 0x01, 0x02, 0x00, 0x42}, 0o644); err != nil {
		t.Fatal(err)
	}
	write(".ssh/id_ed25519", "SSH_MARKER_aaa\n")
	write("nested/.aws/credentials", "AWS_MARKER_bbb\n")
	write("nested/readme.txt", "ORDINARY\n")
	write(".env.local/secret.txt", "ENV_MARKER_ccc\n")
	write(".KUBE/config", "KUBE_MARKER_ddd\n")
	return root
}

// callTool decodes argsJSON through the tool's own Decode and runs its handler
// against the fixture root, returning the ToolResult.
func callTool(t *testing.T, tool tools.Tool, root, argsJSON string) tools.ToolResult {
	t.Helper()
	decoded, err := tool.Decode(json.RawMessage(argsJSON))
	if err != nil {
		t.Fatalf("decode %s: %v", tool.Name, err)
	}
	return tool.Handle(context.Background(), decoded, tctx(root))
}

func callSearch(t *testing.T, root, query, glob string) tools.ToolResult {
	args := map[string]string{"query": query}
	if glob != "" {
		args["glob"] = glob
	}
	b, _ := json.Marshal(args)
	return callTool(t, newSearchTool(), root, string(b))
}

func callList(t *testing.T, root, path string, depth int) tools.ToolResult {
	args := map[string]any{"depth": depth}
	if path != "" {
		args["path"] = path
	}
	b, _ := json.Marshal(args)
	return callTool(t, newListTool(), root, string(b))
}

func callRead(t *testing.T, root, path string, maxBytes int) tools.ToolResult {
	args := map[string]any{"path": path}
	if maxBytes > 0 {
		args["maxBytes"] = maxBytes
	}
	b, _ := json.Marshal(args)
	return callTool(t, newReadTool(), root, string(b))
}

// callReadArgs runs fs.read with an arbitrary arg map (the window modes).
func callReadArgs(t *testing.T, root string, args map[string]any) tools.ToolResult {
	t.Helper()
	b, _ := json.Marshal(args)
	return callTool(t, newReadTool(), root, string(b))
}

// callFind runs fs.find; maxResults 0 means "leave it defaulted".
func callFind(t *testing.T, root, glob string, maxResults int) tools.ToolResult {
	t.Helper()
	args := map[string]any{"glob": glob}
	if maxResults > 0 {
		args["maxResults"] = maxResults
	}
	b, _ := json.Marshal(args)
	res := callTool(t, newFindTool(), root, string(b))
	if !res.Ok {
		t.Fatalf("find %q failed: %+v", glob, res.Error)
	}
	return res
}

// TestIgnoreNegationCannotExposeCredentialDir is the security invariant for the
// new ignore layer: ignore files are project-controlled input, so a negation that
// tries to re-include a credential store must have no effect whatsoever. The
// sensitive-path prune runs before any ignore rule is consulted, at walk time.
func TestIgnoreNegationCannotExposeCredentialDir(t *testing.T) {
	root := buildSecurityFixture(t)
	// "canary.txt" is a positive control: it proves these ignore files are actually
	// being parsed and applied, so the credential assertions below are not passing
	// merely because ignore handling is inert.
	hostile := "canary.txt\n!.ssh/\n!.ssh/**\n!**/.aws/**\n!.env\n!.env.local/**\n!.KUBE/**\n"
	if err := os.WriteFile(filepath.Join(root, "canary.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".copytreeignore"), []byte(hostile), 0o644); err != nil {
		t.Fatal(err)
	}

	leaks := func(p string) bool {
		for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
			switch strings.ToLower(seg) {
			case ".ssh", ".aws", ".env.local", ".kube":
				return true
			}
		}
		return false
	}

	var sawCanary bool
	for _, e := range walkFiles(root) {
		if leaks(e.rel) {
			t.Fatalf("an ignore-file negation exposed a credential dir to the walk: %q", e.rel)
		}
		if filepath.ToSlash(e.rel) == "canary.txt" {
			sawCanary = true
		}
	}
	if sawCanary {
		t.Fatal("the hostile ignore files were never applied — this test proves nothing as written")
	}
	res := callList(t, root, "", 10)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	for _, e := range res.Result.(map[string]any)["entries"].([]listEntry) {
		if leaks(e.Name) {
			t.Errorf("fs.list leaked a credential segment under a hostile ignore file: %q", e.Name)
		}
	}
	for _, f := range callFind(t, root, "*", 0).Result.(map[string]any)["files"].([]fileMatch) {
		if leaks(f.Path) {
			t.Errorf("fs.find leaked a credential path under a hostile ignore file: %q", f.Path)
		}
	}
	for _, marker := range []string{"SSH_MARKER_aaa", "AWS_MARKER_bbb", "ENV_MARKER_ccc", "KUBE_MARKER_ddd"} {
		sr := callSearch(t, root, marker, "")
		if !sr.Ok {
			t.Fatalf("search %s failed: %+v", marker, sr.Error)
		}
		if m := sr.Result.(map[string]any)["matches"].([]searchMatch); len(m) != 0 {
			t.Errorf("fs.search leaked %q under a hostile ignore file: %+v", marker, m)
		}
	}
}

// TestFindRefusesSensitiveFiles: the finder must not surface a secrets FILE even
// when the glob would happily match its name.
func TestFindRefusesSensitiveFiles(t *testing.T) {
	root := buildSecurityFixture(t)
	files := callFind(t, root, "*", 0).Result.(map[string]any)["files"].([]fileMatch)
	for _, f := range files {
		if f.Path == ".env" || f.Path == "server.key" {
			t.Errorf("fs.find must never surface the secrets file %q", f.Path)
		}
	}
	// Positive control: ordinary files ARE found, so the assertions above are not
	// passing merely because the finder returned nothing.
	var sawOrdinary bool
	for _, f := range files {
		if f.Path == "app.ts" {
			sawOrdinary = true
		}
	}
	if !sawOrdinary {
		t.Fatal("fs.find returned no ordinary files — the negative assertions above prove nothing")
	}
}

// TestListNeverReturnsSensitiveFiles: fs.list must apply the same secrets filter as
// fs.find/fs.search. It reports byte sizes now, and a project-controlled `!.env`
// negation must not be able to put a credential file (or its size) into a listing.
func TestListNeverReturnsSensitiveFiles(t *testing.T) {
	root := buildSecurityFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("!.env\n!server.key\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := callList(t, root, "", 3)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	entries := res.Result.(map[string]any)["entries"].([]listEntry)
	for _, e := range entries {
		if e.Name == ".env" || e.Name == "server.key" {
			t.Errorf("fs.list must never surface the secrets file %q", e.Name)
		}
	}
	var sawOrdinary bool
	for _, e := range entries {
		if e.Name == "app.ts" {
			sawOrdinary = true
		}
	}
	if !sawOrdinary {
		t.Fatal("fs.list returned no ordinary files — the negative assertions above prove nothing")
	}
}

// TestSearchPrunesCredentialDirsAtWalkTime is the load-bearing security test:
// the walk must NEVER descend into a credential
// dir. We assert it white-box against walkFiles — the dir's marker file never
// appears in the walk output, proving the prune fires at walk time (not as a
// post-hoc isSensitivePath filter on already-collected paths).
func TestSearchPrunesCredentialDirsAtWalkTime(t *testing.T) {
	root := buildSecurityFixture(t)
	entries := walkFiles(root)
	for _, e := range entries {
		segs := strings.Split(filepath.ToSlash(e.rel), "/")
		for _, seg := range segs {
			low := strings.ToLower(seg)
			if low == ".ssh" || low == ".aws" || low == ".env.local" || low == ".kube" {
				t.Fatalf("walk descended into a credential dir: %q (the prune must fire at walk time)", e.rel)
			}
		}
	}
	// And the ordinary nested sibling is still walked.
	found := false
	for _, e := range entries {
		if filepath.ToSlash(e.rel) == "nested/readme.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("ordinary nested file must still be walked (prune the sibling, not the parent)")
	}
}

// TestSearchNeverReturnsCredentialMarkers covers the matrix: searching for each
// credential dir's unique marker yields zero matches.
func TestSearchNeverReturnsCredentialMarkers(t *testing.T) {
	root := buildSecurityFixture(t)
	for _, marker := range []string{"SSH_MARKER_aaa", "AWS_MARKER_bbb", "ENV_MARKER_ccc", "KUBE_MARKER_ddd"} {
		res := callSearch(t, root, marker, "")
		if !res.Ok {
			t.Fatalf("search %s failed: %+v", marker, res.Error)
		}
		matches := res.Result.(map[string]any)["matches"].([]searchMatch)
		if len(matches) != 0 {
			t.Errorf("search for %q must return no matches, got %+v", marker, matches)
		}
	}
}

// TestSearchSkipsDotEnvContents: a query that WOULD
// match inside .env never returns the .env file.
func TestSearchSkipsDotEnvContents(t *testing.T) {
	root := buildSecurityFixture(t)
	res := callSearch(t, root, "DEEPSEEK_API_KEY", "")
	if !res.Ok {
		t.Fatalf("search failed: %+v", res.Error)
	}
	for _, m := range res.Result.(map[string]any)["matches"].([]searchMatch) {
		if m.File == ".env" {
			t.Fatal("fs.search must never surface a match from .env")
		}
	}
}

// TestListOmitsCredentialDirsAtAnyDepth: a deep
// listing never surfaces a credential segment anywhere in an entry path.
func TestListOmitsCredentialDirsAtAnyDepth(t *testing.T) {
	root := buildSecurityFixture(t)
	res := callList(t, root, "", 10)
	if !res.Ok {
		t.Fatalf("list failed: %+v", res.Error)
	}
	for _, e := range res.Result.(map[string]any)["entries"].([]listEntry) {
		for _, seg := range strings.Split(e.Name, "/") {
			low := strings.ToLower(seg)
			if low == ".ssh" || low == ".aws" || low == ".env.local" || low == ".kube" {
				t.Errorf("listing leaked a credential segment: %q", e.Name)
			}
		}
	}
}

// TestListRefusesCredentialDirDirectly: listing a
// credential dir directly (or nested) is refused with FS_SENSITIVE, not an empty
// success that could be misread as "directory is empty".
func TestListRefusesCredentialDirDirectly(t *testing.T) {
	root := buildSecurityFixture(t)
	for _, p := range []string{".ssh", "nested/.aws"} {
		res := callList(t, root, p, 1)
		if res.Ok || res.Error.Code != codeFSSensitive {
			t.Errorf("listing %q should be FS_SENSITIVE, got %+v", p, res)
		}
	}
}

// TestListStillListsOrdinaryNested: nested itself is
// fine — its .aws child is pruned but readme.txt survives.
func TestListStillListsOrdinaryNested(t *testing.T) {
	root := buildSecurityFixture(t)
	res := callList(t, root, "nested", 1)
	if !res.Ok {
		t.Fatalf("listing nested failed: %+v", res.Error)
	}
	entries := res.Result.(map[string]any)["entries"].([]listEntry)
	var sawAws, sawReadme bool
	for _, e := range entries {
		if e.Name == ".aws" {
			sawAws = true
		}
		if e.Name == "readme.txt" {
			sawReadme = true
		}
	}
	if sawAws {
		t.Error("nested/.aws must be pruned from the listing")
	}
	if !sawReadme {
		t.Error("nested/readme.txt must survive the listing")
	}
}

// TestListRefusesSymlinkToCredentialDir: a
// benign-named symlink (cloud → nested/.aws) that RESOLVES to a credential store
// is refused with FS_SENSITIVE.
func TestListRefusesSymlinkToCredentialDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation may require privilege on Windows")
	}
	root := buildSecurityFixture(t)
	target := filepath.Join(root, "nested", ".aws")
	link := filepath.Join(root, "cloud")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}
	res := callList(t, root, "cloud", 1)
	if res.Ok || res.Error.Code != codeFSSensitive {
		t.Fatalf("symlink to a credential dir must be refused FS_SENSITIVE, got %+v", res)
	}
}
