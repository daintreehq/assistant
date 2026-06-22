// Package projectinstructions loads a repo's DAINTREE.md instruction file. It
// NEVER panics: a missing file is the normal case (silent skip); an oversized or
// unreadable file yields a warning and no content. Spec: docs/port/domain-config.md §3.
package projectinstructions

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const (
	// Filename is the per-repo instruction file at the project root.
	Filename = "DAINTREE.md"
	// MaxBytes is the hard cap; oversized files are SKIPPED with a warning, not
	// truncated.
	MaxBytes = 16 * 1024
)

// Result carries trimmed instruction content and/or a warning. Both are empty
// when there is nothing to inject.
type Result struct {
	Content string
	Warning string
}

// Load reads <projectPath>/DAINTREE.md and returns a Result. It never returns an
// error — failures become warnings. The content is folded into the DYNAMIC prompt
// layer, never the cached base prefix.
func Load(projectPath string) Result {
	file := filepath.Join(projectPath, Filename)

	// Lstat (NOT Stat) so a SYMLINK is detected and rejected rather than followed: the
	// bound project is untrusted, and a repo could point DAINTREE.md at a secret file
	// (e.g. ~/.ssh/config) to exfiltrate its contents into the model prompt. We only ever
	// read a real regular file located at this exact path.
	info, err := os.Lstat(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{} // normal case: no file
		}
		return Result{Warning: fmt.Sprintf("Could not read %s: %v", Filename, err)}
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return Result{Warning: fmt.Sprintf("Skipping %s: it is a symlink (not followed, to avoid reading a linked file into the prompt).", Filename)}
	}
	if !info.Mode().IsRegular() {
		return Result{} // silent skip (e.g. a directory named DAINTREE.md)
	}
	if info.Size() > MaxBytes {
		return Result{Warning: fmt.Sprintf("Skipping %s: %d bytes exceeds the %d-byte limit.", Filename, info.Size(), MaxBytes)}
	}

	raw, err := os.ReadFile(file)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Result{}
		}
		return Result{Warning: fmt.Sprintf("Could not read %s: %v", Filename, err)}
	}
	// Re-check byte length: stat+read are two syscalls; guard against growth.
	if len(raw) > MaxBytes {
		return Result{Warning: fmt.Sprintf("Skipping %s: %d bytes exceeds the %d-byte limit.", Filename, len(raw), MaxBytes)}
	}

	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return Result{}
	}
	return Result{Content: trimmed}
}
