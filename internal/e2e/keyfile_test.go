package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// keyfile_test.go pins the --api-key-file contract end to end, through the real binary:
// an unreadable or malformed key file is FATAL, and the run never quietly falls back to
// a key the caller did not name. Each case deliberately sets DAINTREE_API_KEY to a
// working-looking fallback — if the fallback were ever used the run would get past the
// sign-in gate and reach the backend, so "exit 1 before any turn" is the assertion that
// the fallback was refused.

// runKeyFile invokes the binary with the given extra args plus an isolated state dir and
// a decoy DAINTREE_API_KEY, returning stdout, stderr and the exit code.
func runKeyFile(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	bin := buildBinary(t)
	cmd := exec.Command(bin, args...)
	cmd.Env = append(cmd.Environ(),
		// The decoy. Reaching for this instead of failing is the bug under test.
		"DAINTREE_API_KEY=sk-or-v1-fake-decoy-key-must-not-be-used",
		"DAINTREE_BACKEND_URL=http://127.0.0.1:1", // refused instantly if ever dialled
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	code := 0
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError)
		if !ok {
			t.Fatalf("run binary: %v (stderr: %s)", runErr, stderr.String())
		}
		code = ee.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

func TestKeyFileFailureIsFatalInHumanMode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-key")
	stdout, stderr, code := runKeyFile(t, "--api-key-file", missing, "hello")

	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q stderr %q)", code, stdout, stderr)
	}
	// stdout is the ANSWER channel: a failed human run must leave it empty so a caller
	// that captures stdout gets nothing rather than an error rendered as the reply.
	if strings.TrimSpace(stdout) != "" {
		t.Errorf("stdout must stay empty on failure, got %q", stdout)
	}
	// Actionable means: which flag, and which path. Not the OS's wording, which differs
	// across platforms.
	if !strings.Contains(stderr, "--api-key-file") {
		t.Errorf("stderr should name the flag, got %q", stderr)
	}
	if !strings.Contains(stderr, missing) {
		t.Errorf("stderr should name the path, got %q", stderr)
	}
}

func TestKeyFileFailureIsFatalInJSONMode(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-key")
	stdout, stderr, code := runKeyFile(t, "--json", "--api-key-file", missing, "hello")

	if code != 1 {
		t.Fatalf("exit = %d, want 1 (stdout %q stderr %q)", code, stdout, stderr)
	}
	// The JSONL contract holds even for a setup failure: stdout stays pure JSONL and the
	// terminal envelope carries the structured error.
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var last map[string]any
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(l), &obj); err != nil {
			t.Fatalf("non-JSON line on stdout: %q", l)
		}
		last = obj
	}
	if last == nil {
		t.Fatalf("no JSONL lines on stdout; stderr: %s", stderr)
	}
	if last["type"] != "result" || last["status"] != "error" {
		t.Fatalf("terminal line = %v, want a result/error envelope", last)
	}
	if c, _ := last["exitCode"].(float64); int(c) != 1 {
		t.Errorf("result.exitCode = %v, want 1", last["exitCode"])
	}
	errObj, _ := last["error"].(map[string]any)
	msg, _ := errObj["message"].(string)
	if !strings.Contains(msg, "--api-key-file") || !strings.Contains(msg, missing) {
		t.Errorf("result.error should name the flag and the path, got %q", msg)
	}
}

// TestKeyFileRejectsMalformedKey: the file path applies the SAME structural check as the
// login prompt, so a stray newline or a pasted-in wrapper becomes a readable message
// here rather than an opaque 401 a round trip later.
func TestKeyFileRejectsMalformedKey(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"embedded newline": "sk-or-v1-fake\nextra-line",
		"internal space":   "sk-or-v1-fake key",
		"blank":            "   \n\t\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			stdout, stderr, code := runKeyFile(t, "--api-key-file", path, "hello")
			if code != 1 {
				t.Fatalf("exit = %d, want 1 (stdout %q stderr %q)", code, stdout, stderr)
			}
			if !strings.Contains(stderr, "--api-key-file") {
				t.Errorf("stderr should name the flag, got %q", stderr)
			}
		})
	}
}
