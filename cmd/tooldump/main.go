// Command tooldump writes this CLI's tool inventory — the exact `input.tools` payload
// sent to the backend on every turn — to stdout as JSON.
//
//	go run ./cmd/tooldump                          # the projection a normal launch sends
//	go run ./cmd/tooldump -o tools.json            # …to a file
//	go run ./cmd/tooldump -workflow-intelligence   # …including the flag-gated graph tools
//
// It exists for the backend, which pins a captured copy of this payload and needs to
// regenerate it in CI to catch catalog drift (a removed tool that its skills still name).
// Refreshing that pin must not require editing this repo, which is why this is a
// committed command and not a throwaway test. See internal/app/toolinventory.go.
//
// Output is deterministic: the same registry produces byte-identical bytes, so a refresh
// is a reviewable diff. Diagnostics go to stderr so stdout carries nothing but the JSON;
// a build or write failure exits 1 (bad flags exit 2, via the flag package).
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/daintreehq/assistant/internal/app"
)

func main() {
	var (
		out      = flag.String("o", "", "write to this file instead of stdout")
		workflow = flag.Bool("workflow-intelligence", false,
			"include the execution-graph tools that register only under DAINTREE_WORKFLOW_INTELLIGENCE=1")
	)
	flag.Parse()

	if err := run(*out, *workflow); err != nil {
		fmt.Fprintf(os.Stderr, "tooldump: %v\n", err)
		os.Exit(1)
	}
}

func run(outPath string, workflowIntelligence bool) error {
	inv, err := app.BuildToolInventory(app.ToolInventoryOptions{
		WorkflowIntelligence: workflowIntelligence,
	})
	if err != nil {
		return err
	}
	data, err := app.RenderToolInventory(inv)
	if err != nil {
		return err
	}
	if outPath == "" {
		_, err = os.Stdout.Write(data)
		return err
	}
	if err := writeAtomic(outPath, data); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d tools to %s\n", len(inv), outPath)
	return nil
}

// writeAtomic replaces path with data through a temp-file-and-rename, so a failure
// partway through leaves the previous contents intact.
//
// os.WriteFile would truncate first and write second. The destination here is typically
// a committed fixture, and truncating one to nothing on a full disk — or on any late
// write error — destroys the last known-good copy in order to fail. Rename within the
// same directory is atomic, so the file is either the old inventory or the new one.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	// Same directory as the destination: a rename across filesystems is not atomic (and
	// on many systems not even permitted).
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has succeeded

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Close before rename, and check its error: a buffered write can fail here and
	// nowhere else, which is exactly the truncated-output case this function prevents.
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	// 0644: a tool inventory is public product surface, not a secret. CreateTemp makes
	// the file 0600, so set the mode explicitly rather than shipping a fixture the
	// regenerating user cannot let anyone else read.
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}
