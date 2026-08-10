package daemon

import (
	"fmt"
	"math"
	"regexp"
	"strings"

	"github.com/daintreehq/assistant/internal/domain"
)

// countChangedFiles counts uncommitted file changes from a git.getProjectPulse
// structuredContent, tolerating a flat count, a changed-files array, or grouped
// staged/unstaged/untracked collections. Returns (n, false) when no recognized
// shape is present, so the caller falls back to text parsing.
func countChangedFiles(sc map[string]any) (int, bool) {
	for _, k := range []string{"changedFiles", "changed_files", "fileCount", "changeCount"} {
		v, ok := sc[k]
		if !ok {
			continue
		}
		if n, isNum := asNumber(v); isNum {
			return maxInt(0, int(math.Floor(n))), true
		}
		if arr, isArr := v.([]any); isArr {
			return len(arr), true
		}
	}
	total := 0
	found := false
	for _, k := range []string{"staged", "unstaged", "untracked", "modified", "added", "deleted"} {
		v, ok := sc[k]
		if !ok {
			continue
		}
		if arr, isArr := v.([]any); isArr {
			total += len(arr)
			found = true
		} else if n, isNum := asNumber(v); isNum {
			total += maxInt(0, int(math.Floor(n)))
			found = true
		}
	}
	if found {
		return total, true
	}
	return 0, false
}

// extractChangedFileList pulls changed-file paths (string entries or {path}
// objects) from the recognized shapes, deduped in first-seen order.
func extractChangedFileList(sc map[string]any) []string {
	var out []string
	seen := make(map[string]bool)
	push := func(v any) {
		arr, ok := v.([]any)
		if !ok {
			return
		}
		for _, item := range arr {
			switch t := item.(type) {
			case string:
				if strings.TrimSpace(t) != "" && !seen[t] {
					seen[t] = true
					out = append(out, t)
				}
			case map[string]any:
				if p, ok := t["path"].(string); ok && strings.TrimSpace(p) != "" && !seen[p] {
					seen[p] = true
					out = append(out, p)
				}
			}
		}
	}
	for _, k := range []string{"changedFiles", "changed_files"} {
		push(sc[k])
	}
	for _, k := range []string{"staged", "unstaged", "untracked", "modified", "added", "deleted"} {
		push(sc[k])
	}
	return out
}

var dirtyMarkerRe = regexp.MustCompile(`(?i)Changes not staged|Changes to be committed|Untracked files|modified:|new file:|deleted:|renamed:`)
var cleanMarkerRe = regexp.MustCompile(`(?i)nothing to commit|working tree clean|no changes`)

// DeriveVerification derives the git artifact state of a VerificationResult from a
// git.getProjectPulse result. Defensive and pure: prefer an explicit dirty/clean
// flag, then a changed-file count, then text markers. A clean tree is "verified";
// a dirty tree is "unknown" (uncommitted work is NORMAL, not failure); an
// unreadable state is "unknown". NEVER returns "failed" — only the acceptance
// judge can confidently fail. Exported for unit testing without MCP.
func DeriveVerification(sc map[string]any, text string) domain.VerificationResult {
	changedFiles, hasCount := countChangedFiles(sc)
	changedFileList := extractChangedFileList(sc)

	// First defined of isDirty, dirty, !clean, !isClean.
	var dirtyFlag *bool
	if b, ok := sc["isDirty"].(bool); ok {
		dirtyFlag = &b
	} else if b, ok := sc["dirty"].(bool); ok {
		dirtyFlag = &b
	} else if b, ok := sc["clean"].(bool); ok {
		nb := !b
		dirtyFlag = &nb
	} else if b, ok := sc["isClean"].(bool); ok {
		nb := !b
		dirtyFlag = &nb
	}

	var hasGitChanges *bool
	switch {
	case dirtyFlag != nil:
		hasGitChanges = dirtyFlag
	case hasCount:
		v := changedFiles > 0
		hasGitChanges = &v
	case text != "":
		// Check dirty markers BEFORE clean — a status body listing changes never
		// contains "working tree clean", but paths could spuriously match /clean/.
		if dirtyMarkerRe.MatchString(text) {
			v := true
			hasGitChanges = &v
		} else if cleanMarkerRe.MatchString(text) {
			v := false
			hasGitChanges = &v
		}
	}
	// Dirty wins: a positive changed-file count overrides a clean flag, so a
	// self-contradictory pulse ({isDirty:false, changedFiles:3}) is never clean.
	if hasCount && changedFiles > 0 {
		v := true
		hasGitChanges = &v
	}

	if hasGitChanges == nil {
		return domain.VerificationResult{
			Verdict:            domain.VerdictUnknown,
			HasGitChanges:      false,
			ChangedFiles:       0,
			ChangedFileList:    []string{},
			GitSummary:         "git state could not be determined from the project pulse",
			UnresolvedWarnings: []string{},
		}
	}
	if *hasGitChanges {
		count := changedFiles
		if !hasCount {
			count = len(changedFileList)
		}
		summary := "uncommitted changes present in the worktree"
		if count > 0 {
			summary = fmt.Sprintf("%d uncommitted file change(s) in the worktree", count)
		}
		return domain.VerificationResult{
			Verdict:            domain.VerdictUnknown,
			HasGitChanges:      true,
			ChangedFiles:       count,
			ChangedFileList:    nonNil(changedFileList),
			GitSummary:         summary,
			UnresolvedWarnings: []string{},
		}
	}
	return domain.VerificationResult{
		Verdict:            domain.VerdictVerified,
		HasGitChanges:      false,
		ChangedFiles:       0,
		ChangedFileList:    []string{},
		GitSummary:         "working tree clean (no uncommitted changes)",
		UnresolvedWarnings: []string{},
	}
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return 0, false
		}
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
