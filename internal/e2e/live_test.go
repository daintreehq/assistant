package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestLiveFireworksOneShot is the ONE test that proves a real model round-trip
// actually works end-to-end: it builds the real binary, runs `--json "<prompt>"`
// against the REAL Fireworks API (the default large model, no FIREWORKS_BASE_URL
// override), with NO Daintree MCP, and asserts the JSONL schema-v1 stream came
// back well-formed with non-empty assistant content over the wire.
//
// GATING (why it is so defensive): this test makes a billable network call to a
// third party. It MUST NOT run on a normal `go test ./...` / CI / a contributor's
// laptop and silently spend money or flake on a network hiccup. So it is opt-in
// on FOUR independent guards, each of which skips (never fails) when not met:
//
//  1. DAINTREE_E2E_LIVE=1 — explicit opt-in. Absent → skip. This is the master
//     switch; without it the live test never touches the network.
//  2. FIREWORKS_API_KEY present in the real env — no key, nothing to authenticate
//     with, so skip rather than emit a confusing auth error.
//  3. -short mode — `go test -short` is the "fast, no-network" contract; honor it.
//  4. -race — buildBinary(t) already skips: it spawns a separate, non-instrumented
//     process, so -race adds no coverage and only flakes under load.
//
// ASSERTIONS are STRUCTURAL, never on exact text. Model output is nondeterministic
// (and the prompt asks for "pong", but models drift/embellish), so we prove the
// PIPE works — exit 0, pure JSONL, monotonic seq, a terminal success envelope, and
// at least one chunk of non-empty assistant text — not that any specific tokens
// came back.
func TestLiveFireworksOneShot(t *testing.T) {
	// Guard 3: the fast/no-network contract.
	if testing.Short() {
		t.Skip("live Fireworks e2e skipped in -short mode")
	}
	// Guard 1: the master opt-in switch. Keep money/network off by default.
	if os.Getenv("DAINTREE_E2E_LIVE") != "1" {
		t.Skip("live Fireworks e2e is opt-in; run with DAINTREE_E2E_LIVE=1 and a FIREWORKS_API_KEY in the env " +
			"(e.g. DAINTREE_E2E_LIVE=1 go test ./internal/e2e/ -run TestLiveFireworks -v -count=1)")
	}
	// Guard 2: no real key → nothing to call. Skip (not fail) so a misconfigured
	// opt-in is obvious but non-fatal.
	if strings.TrimSpace(os.Getenv("FIREWORKS_API_KEY")) == "" {
		t.Skip("live Fireworks e2e requires FIREWORKS_API_KEY in the environment; none set")
	}

	// Guard 4 lives inside buildBinary(t): it t.Skip()s under -race.
	bin := buildBinary(t)

	// Generous deadline so a hung socket fails CLEANLY (the process is killed and
	// we report) rather than blocking the suite until the global test timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// A tiny, cheap, tool-free prompt: a single short reply needs no project tool,
	// so the turn is one model round-trip → minimal tokens, minimal cost. We do NOT
	// assert on the word "pong" — only that real content streamed back.
	cmd := exec.CommandContext(ctx, bin, "--json", "Reply with exactly the word: pong, nothing else.")

	// Inherit the real environment FIRST (this carries the real FIREWORKS_API_KEY),
	// then layer the test-isolation overrides. Crucially we do NOT set
	// FIREWORKS_BASE_URL (so it hits the real default Fireworks endpoint) and do NOT
	// set DAINTREE_ASSISTANT_OFFLINE (offline mode would short-circuit the call).
	cmd.Env = append(os.Environ(),
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
	// "live" failure (network/auth hang) and the generic exit-code path would hide it.
	if ctx.Err() == context.DeadlineExceeded {
		t.Fatalf("live request exceeded 90s deadline (network/auth hang?)\nstderr:\n%s", stderr.String())
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
	// Real content surfaces either as assistant:content lines (streamed prose) or as
	// the authoritative `content` on assistant:end / the terminal result. We collect
	// from all of them and assert the concatenation is non-empty — proof that bytes
	// came back from Fireworks, WITHOUT pinning the exact (nondeterministic) text.
	var assistantText strings.Builder
	for _, l := range lines {
		switch l.Type {
		case "assistant:content", "assistant:end":
			if c, ok := l.raw["content"].(string); ok {
				assistantText.WriteString(c)
			}
		}
	}
	// Fall back to the terminal result's content if nothing was on the content/end
	// lines (defensive: the envelope always carries the final content).
	if strings.TrimSpace(assistantText.String()) == "" {
		if c, ok := last.raw["content"].(string); ok {
			assistantText.WriteString(c)
		}
	}
	if strings.TrimSpace(assistantText.String()) == "" {
		t.Errorf("no non-empty assistant content in the live stream — the model produced nothing over the wire.\ntypes: %v\nstderr:\n%s", types, stderr.String())
	}
}
