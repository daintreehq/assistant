package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/backend"
)

// liveBackendURL is the LOCAL development backend the live test targets — pinned
// explicitly, never to backend.DefaultBaseURL, which now points at the deployed
// endpoint. A test that quietly followed the default would spend real money against
// production every time someone ran `go test ./...`.
const liveBackendURL = backend.LocalBaseURL // http://127.0.0.1:8473

// backendReachable probes the backend's liveness endpoint with a short deadline.
// Returns false (skip the live test) when nothing is listening — so the suite passes
// offline, on CI, and on a contributor's laptop where the backend isn't running.
func backendReachable(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url+"/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// TestLiveBackendOneShot is the ONE test that proves a real end-to-end turn against the
// live Daintree backend actually works: it builds the real binary, runs
// `--json "<prompt>"` against a REAL backend on 127.0.0.1:8473 (pinned via
// DAINTREE_BACKEND_URL), with NO Daintree MCP, and asserts the JSONL schema-v1 stream
// came back well-formed with non-empty assistant content over the wire.
//
// GATING (so the suite passes offline). It is opt-in on independent guards, each of
// which SKIPS (never fails) when not met:
//
//  1. -short mode — `go test -short` is the "fast, no-network" contract; honor it.
//  2. backend reachability — if nothing answers /health on the dev endpoint, skip.
//     This is what keeps the suite green offline / on CI / on a laptop with no backend.
//  3. DAINTREE_API_KEY — the turn is funded by the caller's key; unset means opted out.
//  4. -race — buildBinary(t) already skips: it spawns a separate, non-instrumented
//     process, so -race adds no coverage and only flakes under load.
//
// ASSERTIONS are STRUCTURAL, never on exact text. Model output is nondeterministic, so we
// prove the PIPE works — exit 0, pure JSONL, monotonic seq, a terminal success envelope,
// and at least one chunk of non-empty assistant text — not that specific tokens came back.
func TestLiveBackendOneShot(t *testing.T) {
	// Guard 1: the fast/no-network contract.
	if testing.Short() {
		t.Skip("live backend e2e skipped in -short mode")
	}
	// Guard 2: nothing is serving the dev backend → skip so the suite stays green offline.
	if !backendReachable(liveBackendURL) {
		t.Skipf("live backend e2e requires a Daintree backend reachable at %s; none responded to /health", liveBackendURL)
	}
	// There is deliberately no third guard on a funding key. The backend holds its own
	// upstream credential now, so a reachable one should be able to fund this turn
	// unaided; requiring DAINTREE_API_KEY would skip the test on exactly the setup it is
	// meant to exercise. A local backend running WITHOUT a credential fails here rather
	// than skipping, which is the correct report — it cannot serve a turn, and that is
	// worth knowing.
	liveKey := strings.TrimSpace(os.Getenv("DAINTREE_API_KEY"))

	// Guard 3 lives inside buildBinary(t): it t.Skip()s under -race.
	bin := buildBinary(t)

	// Generous deadline so a hung socket fails CLEANLY (the process is killed and we
	// report) rather than blocking the suite until the global test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A tiny, cheap, tool-free prompt: a single short reply needs no project tool, so the
	// turn is one backend round-trip. We do NOT assert on the word "pong" — only that real
	// content streamed back.
	cmd := exec.CommandContext(ctx, bin, "--json", "Reply with exactly the word: pong, nothing else.")

	// Inherit the real environment FIRST, then layer the test-isolation overrides.
	// DAINTREE_BACKEND_URL is now set EXPLICITLY to the local backend: the default is
	// the deployed endpoint, and this test must never be the thing that quietly calls
	// production. DAINTREE_ASSISTANT_OFFLINE stays unset (offline would short-circuit
	// the call). DAINTREE_API_KEY is inherited from the environment when present: the
	// backend normally funds the turn from its own credential, but a local backend
	// running without one still completes if the caller supplies a funding key.
	cmd.Env = append(os.Environ(),
		"DAINTREE_BACKEND_URL="+liveBackendURL,
		"DAINTREE_API_KEY="+liveKey,
		"DAINTREE_MCP_URL=",                         // no MCP → clean degraded local mode, no Daintree dependency
		"DAINTREE_MCP_TOKEN=",                       // …and no stale token
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(), // isolate the SQLite state per run
		"DAINTREE_ASSISTANT_TIER=supervisor",        // read-only tier: safest, no mutating tools
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",            // keep stdout pure / no log files
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	runErr := cmd.Run()

	// If the context deadline fired, surface that explicitly — it's the most likely
	// "live" failure (network hang) and the generic exit-code path would hide it.
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("live request exceeded 90s deadline (backend hang?)\nstderr:\n%s", stderr.String())
	}

	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			t.Fatalf("run binary: %v (stderr: %s)", runErr, stderr.String())
		}
	}
	if exitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %q)", exitCode, stderr.String())
	}

	// --- stdout purity: no ANSI, every non-blank line is JSON, seq monotonic from 0 ---
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
			t.Fatalf("non-JSON line on stdout (impurity): %q\nfull stdout:\n%s\nstderr:\n%s", text, stdout.String(), stderr.String())
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
	for i, l := range lines {
		if l.Seq != i {
			t.Errorf("seq[%d] = %d, want %d (gaps/non-monotonic): %v", i, l.Seq, i, typesOf(lines))
			break
		}
	}

	// --- the stream opened a turn and ended with a terminal result line ---
	types := typesOf(lines)
	if firstIndex(types, "assistant:start") < 0 {
		t.Errorf("missing assistant:start in JSONL stream: %v", types)
	}
	last := lines[len(lines)-1]
	if last.Type != "result" {
		t.Fatalf("last line type = %q, want result (types: %v)\nstderr:\n%s", last.Type, types, stderr.String())
	}
	if v, _ := last.raw["schemaVersion"].(float64); int(v) != 1 {
		t.Errorf("result.schemaVersion = %v, want 1", last.raw["schemaVersion"])
	}
	if s, _ := last.raw["status"].(string); s != "success" {
		t.Errorf("result.status = %q, want success (stderr: %s)", s, stderr.String())
	}
	if c, _ := last.raw["exitCode"].(float64); int(c) != 0 {
		t.Errorf("result.exitCode = %v, want 0", last.raw["exitCode"])
	}

	// --- the model actually produced text over the wire ---
	// Real content surfaces either as assistant:content lines (streamed prose) or as the
	// authoritative `content` on assistant:end / the terminal result. Collect from all and
	// assert the concatenation is non-empty — proof that bytes came back from the backend,
	// WITHOUT pinning the exact (nondeterministic) text.
	var assistantText strings.Builder
	for _, l := range lines {
		switch l.Type {
		case "assistant:content", "assistant:end":
			if c, ok := l.raw["content"].(string); ok {
				assistantText.WriteString(c)
			}
		}
	}
	// Fall back to the terminal result's content if nothing was on the content/end lines.
	if strings.TrimSpace(assistantText.String()) == "" {
		if c, ok := last.raw["content"].(string); ok {
			assistantText.WriteString(c)
		}
	}
	if strings.TrimSpace(assistantText.String()) == "" {
		t.Errorf("no non-empty assistant content in the live stream — the backend produced nothing over the wire.\ntypes: %v\nstderr:\n%s", types, stderr.String())
	}
}
