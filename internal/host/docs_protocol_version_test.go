package host

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The host protocol version is written down in the prose docs as well as generated into
// docs/generated/COMPATIBILITY.md, and prose does not regenerate. It drifted before —
// the generated manifest said 3 while HEADLESS.md, ARCHITECTURE.md and CLAUDE.md all
// still said 2 — which is worse than saying nothing: an integrator reads the
// hand-written guide, builds against the wrong envelope, and finds out at runtime.
//
// So the ONE constant is the source of truth and this test is the gate. It is
// deliberately a grep rather than a generator: these files are narrative and must stay
// hand-written, but the version inside them may not disagree with the code.
var protocolVersionMentions = regexp.MustCompile(`(?i)PROTOCOL_VERSION[ =]+(\d+)|protocol v(\d+)\b|host protocol (?:version )?(\d+)\b`)

// docsScanned are the hand-written surfaces an integrator actually reads. The generated
// manifest is excluded because it is projected from the constant already.
func docsScanned(t *testing.T) []string {
	t.Helper()
	root := repoRoot(t)
	var out []string
	for _, rel := range []string{"CLAUDE.md", "README.md"} {
		out = append(out, filepath.Join(root, rel))
	}
	matches, err := filepath.Glob(filepath.Join(root, "docs", "*.md"))
	if err != nil {
		t.Fatalf("glob docs: %v", err)
	}
	return append(out, matches...)
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root (no go.mod above the test's working directory)")
	return ""
}

func TestDocsNameTheCurrentHostProtocolVersion(t *testing.T) {
	want := strconv.Itoa(ProtocolVersion)
	for _, path := range docsScanned(t) {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			// A changelog or a historical note may legitimately name an old version.
			if isHistoricalContext(line) {
				continue
			}
			for _, m := range protocolVersionMentions.FindAllStringSubmatch(line, -1) {
				got := firstNonEmpty(m[1:])
				if got == "" || got == want {
					continue
				}
				t.Errorf("%s:%d names host protocol version %s, but host.ProtocolVersion is %s:\n  %s\n"+
					"Fix the prose, or if the protocol really changed, change the constant and regenerate "+
					"docs/generated/COMPATIBILITY.md.",
					mustRel(t, path), i+1, got, want, strings.TrimSpace(line))
			}
		}
	}
}

// isHistoricalContext exempts a line that is explicitly talking about the past. Without
// it this gate would forbid the project from ever describing what changed.
func isHistoricalContext(line string) bool {
	lower := strings.ToLower(line)
	for _, marker := range []string{"historical", "changelog", "was protocol", "protocol v2 (retired", "previously", "before v", "superseded"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func firstNonEmpty(groups []string) string {
	for _, g := range groups {
		if g != "" {
			return g
		}
	}
	return ""
}

func mustRel(t *testing.T, path string) string {
	t.Helper()
	rel, err := filepath.Rel(repoRoot(t), path)
	if err != nil {
		return path
	}
	return rel
}
