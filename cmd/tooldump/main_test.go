package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// run() writes the inventory to the named file and nothing else. The inventory content
// itself is covered by internal/app's tests; what is only testable here is the plumbing
// — that -o produces a real, complete, parseable file.
func TestRunWritesTheInventoryToAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tools.json")
	if err := run(path, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(data, &tools); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if len(tools) == 0 {
		t.Fatal("wrote an empty inventory")
	}
	if !strings.Contains(tools[0].Function.Name, "__") {
		t.Errorf("first tool is %q, which is not a wire name", tools[0].Function.Name)
	}

	// The temp file must not survive a successful run: a directory slowly filling with
	// .tools.json.tmp-* leftovers is the usual way an atomic-write helper rots.
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".*tmp-*"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(leftovers) != 0 {
		t.Errorf("left temp files behind: %v", leftovers)
	}
}

// A destination that cannot be written must fail LOUDLY and leave the previous contents
// untouched. This is the regression that matters for a committed fixture: the old
// behaviour (os.WriteFile) truncated first and wrote second, so a failure destroyed the
// last known-good copy in the course of failing.
func TestWriteAtomicPreservesTheDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fixture.json")
	const original = "[the previous inventory]"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Make the DIRECTORY unwritable so the temp file cannot be created — the earliest
	// possible failure, and the one that would previously have already truncated.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	if err := writeAtomic(path, []byte("[the new inventory]")); err == nil {
		t.Fatal("writeAtomic succeeded against an unwritable directory")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != original {
		t.Errorf("destination is now %q — the previous contents were destroyed by a failed write", got)
	}
}

// The written file must be world-readable. os.CreateTemp makes its file 0600, so without
// an explicit chmod a regenerated fixture would be readable only by whoever ran the
// generator — which is wrong for something committed to a repository.
func TestWriteAtomicSetsAReadableMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.json")
	if err := writeAtomic(path, []byte("[]")); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("mode is %04o, want 0644", perm)
	}
}
