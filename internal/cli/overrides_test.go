package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/redact"
)

// boolPtr is the tri-state helper the Options booleans need: nil (flag absent),
// &false (explicitly off), &true (explicitly on).
func boolPtr(v bool) *bool { return &v }

// overrides_test.go pins the flag→ConfigOverrides mapping. The interesting cases are
// all about what does NOT get set: a flag that was never passed must leave a nil
// pointer, because a non-nil false would override an intentional env var with silence.

func TestOverridesFromOptionsMapsHarnessFlags(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "key")
	if err := os.WriteFile(keyPath, []byte("  sk-or-v1-fake-test-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// RegisterSecret writes to a process-global registry; reset it so this test neither
	// inherits another test's secrets nor leaks its own into one.
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	o, err := overridesFromOptions(Options{
		BackendURL:  "http://127.0.0.1:8473",
		APIKeyFile:  keyPath,
		StateDir:    "/tmp/state",
		LogDir:      "/tmp/logs",
		AutoApprove: boolPtr(true),
		DebugLog:    boolPtr(true),
		Tier:        "operator",
		McpURL:      "http://127.0.0.1:9000/mcp",
		McpToken:    "mcp-fake-test-token",
		Project:     "/tmp/project",
	})
	if err != nil {
		t.Fatalf("overridesFromOptions() error = %v", err)
	}
	if o.BackendURL == nil || *o.BackendURL != "http://127.0.0.1:8473" {
		t.Errorf("BackendURL = %v", o.BackendURL)
	}
	// Surrounding whitespace is trimmed: a key file almost always ends with a newline.
	if o.APIKey == nil || *o.APIKey != "sk-or-v1-fake-test-key" {
		t.Errorf("APIKey = %v, want the trimmed file contents", o.APIKey)
	}
	if o.StateDir == nil || *o.StateDir != "/tmp/state" {
		t.Errorf("StateDir = %v", o.StateDir)
	}
	if o.LogDir == nil || *o.LogDir != "/tmp/logs" {
		t.Errorf("LogDir = %v", o.LogDir)
	}
	if o.AutoApprove == nil || !*o.AutoApprove {
		t.Errorf("AutoApprove = %v", o.AutoApprove)
	}
	if o.DebugLog == nil || !*o.DebugLog {
		t.Errorf("DebugLog = %v", o.DebugLog)
	}
	if o.Tier == nil || *o.Tier != "operator" {
		t.Errorf("Tier = %v", o.Tier)
	}
	// The refactor rewrote the legacy mappings too; pin them so a future edit to the
	// shared `set` helper cannot drop one silently.
	if o.McpURL == nil || *o.McpURL != "http://127.0.0.1:9000/mcp" {
		t.Errorf("McpURL = %v", o.McpURL)
	}
	if o.McpToken == nil || *o.McpToken != "mcp-fake-test-token" {
		t.Errorf("McpToken = %v", o.McpToken)
	}
	if o.ProjectPath == nil || *o.ProjectPath != "/tmp/project" {
		t.Errorf("ProjectPath = %v", o.ProjectPath)
	}
}

// TestAPIKeyFileRegistersTheKeyForRedaction: the key must be masked in the debug log
// from the FIRST line written, so registration happens at read time rather than
// wherever the key is eventually used. The fixture is deliberately shapeless — an
// `sk-or-...` value would be caught by shape-based redaction even if exact
// registration were broken, so it would not test what it claims to.
func TestAPIKeyFileRegistersTheKeyForRedaction(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	const shapeless = "zqx-fake-test-credential-9f4c2ae1"
	keyPath := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyPath, []byte(shapeless+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Prove the fixture is not caught by shape alone, or the assertion below is vacuous.
	if redact.String(shapeless) != shapeless {
		t.Fatalf("fixture is shape-redacted on its own; pick a different one")
	}

	if _, err := overridesFromOptions(Options{APIKeyFile: keyPath}); err != nil {
		t.Fatalf("overridesFromOptions() error = %v", err)
	}
	if got := redact.String("bearer " + shapeless); strings.Contains(got, shapeless) {
		t.Errorf("the key was not registered for redaction: %q", got)
	}
}

// TestAPIKeyFileAppliesKeyShape: the same structural check DAINTREE_API_KEY gets, so a
// stray newline or smart quote becomes a readable message here instead of an opaque
// header error on every turn.
func TestAPIKeyFileAppliesKeyShape(t *testing.T) {
	redact.ResetSecretsForTest()
	t.Cleanup(redact.ResetSecretsForTest)

	dir := t.TempDir()
	for name, body := range map[string]string{
		"embedded newline": "fake-test-key\nsecond-line",
		"internal space":   "fake-test key",
		"over the ceiling": strings.Repeat("k", 5000),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := overridesFromOptions(Options{APIKeyFile: path}); err == nil {
				t.Fatalf("%s must be rejected", name)
			}
		})
	}
}

// TestOverridesFromOptionsLeavesUnsetFlagsNil is the load-bearing one: an unpassed
// --auto-approve must NOT resolve to a false override, or it would quietly cancel
// DAINTREE_ASSISTANT_AUTO_APPROVE=1 for every caller that did not type the flag.
func TestOverridesFromOptionsLeavesUnsetFlagsNil(t *testing.T) {
	o, err := overridesFromOptions(Options{})
	if err != nil {
		t.Fatalf("overridesFromOptions() error = %v", err)
	}
	if o.AutoApprove != nil || o.DebugLog != nil || o.Offline != nil {
		t.Errorf("unset booleans must stay nil: autoApprove=%v debugLog=%v offline=%v",
			o.AutoApprove, o.DebugLog, o.Offline)
	}
	if o.BackendURL != nil || o.APIKey != nil || o.StateDir != nil || o.LogDir != nil {
		t.Errorf("unset strings must stay nil: %v %v %v %v",
			o.BackendURL, o.APIKey, o.StateDir, o.LogDir)
	}
}

// TestAPIKeyFileFailuresAreFatal: an unreadable or empty key file must be an error, not
// a fall-through to another credential. Falling back would run the job against a
// DIFFERENT key than the caller named and hide it behind a successful-looking run.
func TestAPIKeyFileFailuresAreFatal(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("   \n\t\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, path := range map[string]string{
		"missing":        filepath.Join(dir, "does-not-exist"),
		"blank":          empty,
		"is a directory": dir,
	} {
		t.Run(name, func(t *testing.T) {
			o, err := overridesFromOptions(Options{APIKeyFile: path})
			if err == nil {
				t.Fatalf("expected an error for %s", name)
			}
			if o.APIKey != nil {
				t.Errorf("a failed read must leave no partial override, got %v", o.APIKey)
			}
		})
	}
}
