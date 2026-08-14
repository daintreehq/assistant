package debuglog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/redact"
)

// readOnlyLog returns the single log file written into dir.
func readOnlyLog(t *testing.T, dir string) string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	if len(entries) != 1 {
		t.Fatalf("want exactly one log file, got %d", len(entries))
	}
	data, err := os.ReadFile(filepath.Join(dir, entries[0].Name()))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(data)
}

// The secret-injection test. Plant credentials in every shape a real turn can carry
// them — tool args, a tool result, a nested structure, a header, an env dump — write
// them through the ordinary logging path, then grep the file.
//
// This is the acceptance criterion, not a unit test of the patterns: the patterns are
// covered in internal/redact. What is proven here is the WIRING — that the redaction
// actually sits between a caller and the file, for inline values and block values alike,
// with no call site able to opt out.
func TestLogDebugRedactsPlantedSecrets(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	dir := t.TempDir()
	cfg := Config{DebugLog: true, LogDir: dir}

	const (
		apiKey   = "sk-or-v1-plantedkeyabcdefghijklmnop0123"
		mcpToken = "an-opaque-mcp-token-with-no-recognisable-shape"
		ghToken  = "ghp_plantedgithubtokenabcdefghijklmn"
	)
	// The MCP token matches no pattern; only registration can catch it. That asymmetry
	// is the whole reason RegisterSecret exists.
	redact.RegisterSecret(mcpToken)

	LogDebug(cfg, "tool.call", map[string]any{
		"tool": "terminal.sendCommand",
		// Inline string value (short, no newline).
		"summary": "ran export API_KEY=" + apiKey,
		// Block value: structured args, the way dispatch logs them.
		"args": map[string]any{
			"terminalId": "terminal-2b9f4c8e",
			"command":    "git remote add origin https://" + ghToken + "@github.com/x/y",
			"env":        map[string]any{"DAINTREE_MCP_TOKEN": mcpToken},
		},
		// Block value: a result carrying a terminal's echoed environment, PLUS the same
		// token in a neutral position. Without the neutral copy this test passed whether
		// or not RegisterSecret worked — the `DAINTREE_MCP_TOKEN=` shape was catching it
		// either way, so the exact-secret layer went unexercised.
		"result": map[string]any{
			"output": "DAINTREE_MCP_TOKEN=" + mcpToken + "\nAUTHORIZATION=Bearer " + apiKey,
			"note":   "reconnected using " + mcpToken + " successfully",
		},
		// Non-string values must pass through untouched.
		"durationMs": 38,
		"ok":         true,
	})

	body := readOnlyLog(t, dir)
	for name, secret := range map[string]string{
		"api key":      apiKey,
		"mcp token":    mcpToken,
		"github token": ghToken,
	} {
		if strings.Contains(body, secret) {
			t.Errorf("%s survived into the debug log:\n%s", name, body)
		}
	}
	if !strings.Contains(body, redact.Mark) {
		t.Errorf("nothing was redacted at all — is the wiring in place?\n%s", body)
	}

	// Redaction must not cost the trace its diagnostic value: the identifiers and the
	// non-secret half of every line have to survive, or the log stops being usable and
	// people turn it off.
	for _, keep := range []string{
		"tool.call", "terminal.sendCommand", "terminal-2b9f4c8e",
		"github.com/x/y", "durationMs=38", "ok=true",
	} {
		if !strings.Contains(body, keep) {
			t.Errorf("redaction destroyed load-bearing detail %q:\n%s", keep, body)
		}
	}
}

// A single terminal dump can be megabytes. Unbounded, one turn produced a log nobody
// could open; the value that mattered was buried under a screenful of build output.
func TestLogDebugCapsOversizedBlockValues(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{DebugLog: true, LogDir: dir}

	huge := strings.Repeat("build output line that goes on and on\n", 5000) + "FAILED: 3 tests failed\n"
	LogDebug(cfg, "tool.call", map[string]any{"tool": "terminal.read", "result": huge})

	body := readOnlyLog(t, dir)
	if len(body) > 2*blockValueMax {
		t.Errorf("an oversized value was not capped: log is %d bytes", len(body))
	}
	if !strings.Contains(body, "elided") || !strings.Contains(body, "sha256:") {
		t.Error("a capped value must record its true size and a content hash, so two occurrences stay comparable")
	}
	// The head must survive — the point of a cap is to keep the readable part.
	if !strings.Contains(body, "build output line that goes on and on") {
		t.Error("capping removed the head of the value, which is the part worth keeping")
	}
	// And so must the TAIL. In build and test output the failure is at the end, so a
	// head-only cap discards the one line the reader opened the log for.
	if !strings.Contains(body, "FAILED: 3 tests failed") {
		t.Error("capping removed the tail, where a build failure actually reports itself")
	}
}

// Disabled logging must stay a total no-op: no directory, no file, no redaction cost.
func TestLogDebugDisabledWritesNothing(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "never-created")
	LogDebug(Config{DebugLog: false, LogDir: dir}, "tool.call", map[string]any{"secret": "sk-or-v1-abcdefghijklmnop0123456"})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("disabled logging created %s", dir)
	}
}
