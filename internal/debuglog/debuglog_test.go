package debuglog

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// sessionRe matches a valid per-session log filename.
var sessionRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}-[\w.-]+\.log$`)

// freshLogDir returns a logDir path that does NOT exist yet (a child of a temp
// dir), so we can assert it is only created on the first write.
func freshLogDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "logs")
}

// resetState clears the package-global active-path / warned-once latch so tests
// don't leak into each other (the production singleton persists across calls).
func resetState(t *testing.T) {
	t.Helper()
	mu.Lock()
	activeLogPath = ""
	warnedOnce = false
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		activeLogPath = ""
		warnedOnce = false
		mu.Unlock()
	})
}

func TestLogDebug_NoOpWhenDisabled(t *testing.T) {
	resetState(t)
	logDir := freshLogDir(t)
	LogDebug(Config{DebugLog: false, LogDir: logDir}, "tool.call", map[string]any{"tool": "fs.read"})
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("disabled logging created the dir %q", logDir)
	}
}

func TestLogDebug_WritesDatedSessionFileInlineAndBlock(t *testing.T) {
	resetState(t)
	logDir := freshLogDir(t)
	cfg := Config{DebugLog: true, LogDir: logDir}
	longContent := strings.Repeat("y", 5000)

	LogDebug(cfg, "model.response", map[string]any{
		"tier":         "large",
		"finishReason": "stop",
		"content":      longContent,
		"toolCalls":    []map[string]any{{"name": "fs.read", "args": map[string]any{"path": "a.ts"}}},
	})

	file := CurrentDebugLogPath()
	if file == "" {
		t.Fatal("no active log path after write")
	}
	if !sessionRe.MatchString(filepath.Base(file)) {
		t.Errorf("filename %q does not match session pattern", filepath.Base(file))
	}
	txt := readFile(t, file)
	// Short scalars render inline as key=value on the event line.
	if !regexp.MustCompile(`model\.response.*tier=large`).MatchString(txt) ||
		!strings.Contains(txt, "finishReason=stop") {
		t.Errorf("inline scalars missing:\n%s", txt)
	}
	// The long string renders as an indented block, untruncated.
	if !strings.Contains(txt, "content:") {
		t.Errorf("content block header missing")
	}
	if !strings.Contains(txt, longContent) {
		t.Errorf("long content was truncated")
	}
	// The nested toolCalls block is JSON-rendered.
	if !strings.Contains(txt, `"fs.read"`) {
		t.Errorf("toolCalls block missing fs.read")
	}
}

func TestStartDebugLog_OpensFileReturnsPathAndWritesHeader(t *testing.T) {
	resetState(t)
	logDir := freshLogDir(t)
	cfg := Config{DebugLog: true, LogDir: logDir}
	header := map[string]any{
		"sessionId":  "ses_ab12cd34",
		"project":    "/Users/dev/some-project",
		"tier":       "system",
		"smallModel": "S",
	}
	file := StartDebugLog(cfg, header)

	if file == "" {
		t.Fatal("StartDebugLog returned empty path while enabled")
	}
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}-ses_ab12cd34\.log$`).MatchString(filepath.Base(file)) {
		t.Errorf("filename = %q", filepath.Base(file))
	}
	if file != CurrentDebugLogPath() {
		t.Errorf("returned path %q != CurrentDebugLogPath %q", file, CurrentDebugLogPath())
	}
	txt := readFile(t, file)
	for _, want := range []string{"session.start", "project=/Users/dev/some-project", "tier=system", "smallModel=S"} {
		if !strings.Contains(txt, want) {
			t.Errorf("header missing %q:\n%s", want, txt)
		}
	}
}

func TestStartDebugLog_DisabledReturnsEmptyWritesNothing(t *testing.T) {
	resetState(t)
	logDir := freshLogDir(t)
	if file := StartDebugLog(Config{DebugLog: false, LogDir: logDir}, map[string]any{}); file != "" {
		t.Errorf("disabled StartDebugLog returned %q, want empty", file)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("disabled StartDebugLog created the dir")
	}
}

func TestStartDebugLog_PrunesOldKeepsRecent(t *testing.T) {
	resetState(t)
	logDir := freshLogDir(t)
	if err := os.MkdirAll(logDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(logDir, "2026-01-01-old.log")
	recent := filepath.Join(logDir, "2026-06-17-recent.log")
	// A non-matching file must NEVER be pruned regardless of age.
	unrelated := filepath.Join(logDir, "notes.txt")
	mustWrite(t, stale, "old")
	mustWrite(t, recent, "recent")
	mustWrite(t, unrelated, "keep me")

	// Age the stale + unrelated files well past the 7-day cutoff via mtime.
	old := time.Now().Add(-time.Duration(maxLogAgeMs)*time.Millisecond - 24*time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(unrelated, old, old); err != nil {
		t.Fatal(err)
	}

	StartDebugLog(Config{DebugLog: true, LogDir: logDir}, map[string]any{"sessionId": "ses_new"})

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale matching log was not pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent log was pruned")
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Error("non-matching old file was pruned (only <date>-*.log files are eligible)")
	}
	if !sessionRe.MatchString(CurrentDebugLogPath()) {
		t.Errorf("active path = %q", CurrentDebugLogPath())
	}
}

func TestLogDebug_NeverPanicsOnUnwritableDir(t *testing.T) {
	resetState(t)
	// Point logDir at a path whose parent is a regular file → MkdirAll fails. The
	// logger must swallow the error and not panic.
	parent := filepath.Join(t.TempDir(), "afile")
	mustWrite(t, parent, "x")
	logDir := filepath.Join(parent, "logs")
	// Must not panic; we only assert it returns.
	LogDebug(Config{DebugLog: true, LogDir: logDir}, "tool.call", map[string]any{"tool": "fs.read"})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func mustWrite(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}
