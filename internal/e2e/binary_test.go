package e2e

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// buildBinary compiles cmd/daintree-assistant once for the whole package into an
// os-temp path (NOT t.TempDir — that is cleaned when the creating test ends, which
// would strand later binary-level tests). The compile is cached via sync.Once so
// multiple binary tests share one build.
var (
	binOnce sync.Once
	binPath string
	binErr  error
)

func buildBinary(t *testing.T) string {
	t.Helper()
	if raceEnabled {
		t.Skip("binary-spawning e2e test runs a separate, non-instrumented process; it adds no race coverage and only flakes under -race load")
	}
	binOnce.Do(func() {
		dir, err := os.MkdirTemp("", "dt-e2e-bin-")
		if err != nil {
			binErr = err
			return
		}
		out := filepath.Join(dir, "daintree-assistant")
		cmd := exec.Command("go", "build", "-o", out, "github.com/daintreehq/assistant/cmd/daintree-assistant")
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			binErr = err
			t.Logf("go build stderr: %s", stderr.String())
			return
		}
		binPath = out
	})
	if binErr != nil {
		t.Fatalf("build binary: %v", binErr)
	}
	return binPath
}

// jsonLine is a parsed JSONL envelope from the --json stream.
type jsonLine struct {
	Type string `json:"type"`
	Seq  int    `json:"seq"`
	raw  map[string]any
}

// TestBinaryJSONOneShot builds the binary and runs `--json "prompt"` pointed at the
// fake Daintree backend SSE server, asserting the JSONL schema-v1 envelope sequence
// (assistant:start → content → tool:call → tool:result → assistant:end → result),
// monotonic seq, exit code 0, and stdout purity (no ANSI / diagnostics — only the
// JSONL lines). The endpoint override is DAINTREE_BACKEND_URL (app.Create's dev/test
// hook), which is what makes a real-binary e2e feasible against the native backend.
func TestBinaryJSONOneShot(t *testing.T) {
	bin := buildBinary(t)

	fake := newFakeBackend(t,
		sseRound{
			contentTokens: []string{"Checking ", "memory."},
			toolName:      "memory__list",
			toolArgs:      `{"limit":3}`,
			usage:         &fakeUsage{prompt: 50, completion: 6, total: 56, cached: 0},
		},
		sseRound{
			contentTokens: []string{"Nothing ", "stored."},
			usage:         &fakeUsage{prompt: 70, completion: 4, total: 74, cached: 20},
		},
	)

	dir := t.TempDir()
	cmd := exec.Command(bin, "--json", "what's in memory?")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
		// The backend authenticates every request, so the CLI refuses to start a turn
		// while signed out (cli/run.go ensureSignedIn). DAINTREE_API_KEY satisfies that
		// gate without touching the real credentials file; the fake backend ignores it.
		"DAINTREE_API_KEY=test-key",
		"DAINTREE_ASSISTANT_STATE_DIR="+dir,
		"DAINTREE_ASSISTANT_TIER=operator",
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=", // no MCP → clean degraded local mode
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run binary: %v (stderr: %s)", runErr, stderr.String())
		}
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stdout: %q stderr: %q)", exitCode, stdout.String(), stderr.String())
	}

	// --- stdout purity: every stdout line is valid JSON, no ANSI escapes ---
	if strings.Contains(stdout.String(), "\x1b") {
		t.Errorf("stdout contains ANSI escape sequences (impurity):\n%q", stdout.String())
	}
	var lines []jsonLine
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		text := sc.Text()
		if strings.TrimSpace(text) == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(text), &raw); err != nil {
			t.Fatalf("non-JSON line on stdout (impurity): %q\nfull stdout:\n%s", text, stdout.String())
		}
		typ, _ := raw["type"].(string)
		seqF, _ := raw["seq"].(float64)
		lines = append(lines, jsonLine{Type: typ, Seq: int(seqF), raw: raw})
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan stdout: %v", err)
	}
	if len(lines) == 0 {
		t.Fatalf("no JSONL lines on stdout; stderr:\n%s", stderr.String())
	}

	// --- monotonic seq from 0 ---
	for i, l := range lines {
		if l.Seq != i {
			t.Errorf("seq[%d] = %d, want %d (gaps/non-monotonic): %+v", i, l.Seq, i, typesOf(lines))
			break
		}
	}

	// --- envelope sequence ---
	types := typesOf(lines)
	mustOrder := []string{"assistant:start", "tool:call", "tool:result", "assistant:end", "result"}
	prev := -1
	for _, want := range mustOrder {
		at := firstIndex(types, want)
		if at < 0 {
			t.Errorf("missing %q in JSONL stream: %v", want, types)
			continue
		}
		if at <= prev {
			t.Errorf("%q out of order (at %d, prev %d): %v", want, at, prev, types)
		}
		prev = at
	}

	// intermediate prose must be flushed as assistant:content BEFORE the tool call.
	contentIdx := firstIndex(types, "assistant:content")
	callIdx := firstIndex(types, "tool:call")
	if contentIdx >= 0 && callIdx >= 0 && contentIdx > callIdx {
		t.Errorf("assistant:content (%d) emitted after tool:call (%d): %v", contentIdx, callIdx, types)
	}

	// --- the terminal result line is last and reports schema v1 + success/exit 0 ---
	last := lines[len(lines)-1]
	if last.Type != "result" {
		t.Fatalf("last line type = %q, want result", last.Type)
	}
	if v, _ := last.raw["schemaVersion"].(float64); int(v) != 1 {
		t.Errorf("result.schemaVersion = %v, want 1", last.raw["schemaVersion"])
	}
	if s, _ := last.raw["status"].(string); s != "success" {
		t.Errorf("result.status = %q, want success", s)
	}
	if c, _ := last.raw["exitCode"].(float64); int(c) != 0 {
		t.Errorf("result.exitCode = %v, want 0", last.raw["exitCode"])
	}

	// --- the tool:call carried the resolved (dotted) internal tool name ---
	for _, l := range lines {
		if l.Type == "tool:call" {
			if name, _ := l.raw["name"].(string); name != "memory.list" {
				t.Errorf("tool:call name = %q, want memory.list", name)
			}
		}
	}
}

func typesOf(lines []jsonLine) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Type
	}
	return out
}

func firstIndex(ss []string, s string) int {
	for i, v := range ss {
		if v == s {
			return i
		}
	}
	return -1
}
