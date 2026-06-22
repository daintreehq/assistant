package skills

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// embeddedSkills mirrors the repo-root skills/ directory inside the module so the
// binary is self-contained. This replaces the TS findSkillsDir package-root walk
// (a Node runtime-resolution artifact). The on-disk override path
// (DAINTREE_ASSISTANT_SKILLS_DIR) is preserved for dev and tests.
//
//go:embed all:files/*.md
var embeddedSkills embed.FS

// embeddedDir is the subdirectory inside the embed FS holding the .md files.
const embeddedDir = "files"

// SkillsDirEnv overrides the skill source with an on-disk directory (absolute
// path; mainly for tests). When set (trimmed non-empty), skills load from the
// real filesystem instead of the embedded FS (spec §2.7 / §9.3).
const SkillsDirEnv = "DAINTREE_ASSISTANT_SKILLS_DIR"

// isSkillFile reports whether a directory entry name is a loadable skill file
// (spec §2.5): lowercased ends ".md", does NOT start with "." or "_", and is not
// "readme.md".
func isSkillFile(name string) bool {
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".md") {
		return false
	}
	if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") {
		return false
	}
	if lower == "readme.md" {
		return false
	}
	return true
}

// LoadSkills loads every skill, honoring the env override. One bad file aborts
// the whole load (fail-loud at boot, spec §2.6). Skills are returned in
// filename-sorted order for deterministic catalog text (prompt-cache stability).
func LoadSkills() ([]Skill, error) {
	if dir := strings.TrimSpace(os.Getenv(SkillsDirEnv)); dir != "" {
		return loadSkillsFromDir(dir)
	}
	return loadSkillsFromEmbedded()
}

// loadSkillsFromEmbedded reads the baked-in skill files. embed.FS ReadDir
// returns names already sorted, but we sort defensively to keep the contract
// explicit.
func loadSkillsFromEmbedded() ([]Skill, error) {
	entries, err := embeddedSkills.ReadDir(embeddedDir)
	if err != nil {
		return nil, fmt.Errorf("reading embedded skills: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isSkillFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	skills := make([]Skill, 0, len(names))
	for _, name := range names {
		content, err := fs.ReadFile(embeddedSkills, embeddedDir+"/"+name)
		if err != nil {
			return nil, fmt.Errorf("reading embedded skill %s: %w", name, err)
		}
		sk, err := parseAndCheck(string(content), name)
		if err != nil {
			return nil, err
		}
		skills = append(skills, sk)
	}
	return skills, nil
}

// loadSkillsFromDir reads skill files from an on-disk directory (spec §2.6):
// filter isSkillFile → lexicographic filename sort → parse each. One bad file
// aborts the entire load.
func loadSkillsFromDir(dir string) ([]Skill, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading skills dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !isSkillFile(e.Name()) {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	skills := make([]Skill, 0, len(names))
	for _, name := range names {
		content, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, fmt.Errorf("reading skill %s: %w", name, err)
		}
		sk, err := parseAndCheck(content2string(content), name)
		if err != nil {
			return nil, err
		}
		skills = append(skills, sk)
	}
	return skills, nil
}

func content2string(b []byte) string { return string(b) }

// parseAndCheck parses one file and enforces that the filename (sans ".md")
// equals the dotted skill id. The TS loader does not enforce this, but the Go
// port treats a filename/id mismatch as a load error so a rename can't silently
// orphan a named handle.
func parseAndCheck(content, name string) (Skill, error) {
	sk, err := parseSkillFile(content, name)
	if err != nil {
		return Skill{}, err
	}
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base != sk.ID {
		return Skill{}, fmt.Errorf("%s: filename does not match skill id %q (expected file %q.md)", name, sk.ID, sk.ID)
	}
	return sk, nil
}
