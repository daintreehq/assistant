package fsx

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

func TestLooksBinary(t *testing.T) {
	if !looksBinary([]byte{'a', 0, 'b'}) {
		t.Error("NUL byte should be binary")
	}
	if looksBinary([]byte("hello\tworld\n")) {
		t.Error("plain text with tab/newline should not be binary")
	}
	// >30% control bytes → binary.
	ctrl := []byte{1, 2, 3, 4, 'a', 'b'}
	if !looksBinary(ctrl) {
		t.Error("high control-byte ratio should be binary")
	}
	if looksBinary(nil) {
		t.Error("empty should not be binary")
	}
}

func tctx(root string) *tools.ToolContext {
	return &tools.ToolContext{Config: config.AppConfig{ProjectPath: root}, ProjectPath: root}
}

func TestSearchSkipsSensitiveAndSkipDirs(t *testing.T) {
	root := t.TempDir()
	must := func(rel, content string) {
		p := filepath.Join(root, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	must("a.txt", "needle here\nsecond line")
	must(".env", "needle SECRET=1")           // sensitive basename — never scanned
	must("node_modules/dep.js", "needle dep") // skip dir — never walked
	must(".ssh/config", "needle in ssh")      // sensitive dir — pruned at walk

	tool := newSearchTool()
	decoded, err := tool.Decode(json.RawMessage(`{"query":"needle"}`))
	if err != nil {
		t.Fatal(err)
	}
	res := tool.Handle(context.Background(), decoded, tctx(root))
	if !res.Ok {
		t.Fatalf("search failed: %+v", res.Error)
	}
	m := res.Result.(map[string]any)
	matches := m["matches"].([]searchMatch)
	if len(matches) != 1 || matches[0].File != "a.txt" {
		t.Fatalf("expected only a.txt, got %+v", matches)
	}
}

func TestReadRefusesSensitiveAndBinary(t *testing.T) {
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("SECRET=1"), 0o644)
	_ = os.WriteFile(filepath.Join(root, "bin.dat"), []byte{0, 1, 2, 3}, 0o644)
	_ = os.WriteFile(filepath.Join(root, "ok.txt"), []byte("plain text"), 0o644)

	read := func(path string) tools.ToolResult {
		tool := newReadTool()
		decoded, _ := tool.Decode(json.RawMessage(`{"path":"` + path + `"}`))
		return tool.Handle(context.Background(), decoded, tctx(root))
	}

	if r := read(".env"); r.Ok || r.Error.Code != codeFSSensitive {
		t.Errorf(".env should be FS_SENSITIVE: %+v", r)
	}
	if r := read("bin.dat"); r.Ok || r.Error.Code != codeFSBinary {
		t.Errorf("binary should be FS_BINARY: %+v", r)
	}
	if r := read("ok.txt"); !r.Ok {
		t.Errorf("plain text should read ok: %+v", r.Error)
	}
}
