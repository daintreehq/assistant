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
			// Round 1 loads one skill but reports TWO active: the second is retained /
			// auto-paired, which is precisely what the delta-only skill:loaded cue can
			// never report and what skill:decision exists to expose.
			skills: skillsBlock(false,
				[]string{"multi_agent", "Multi-agent orchestration",
					"foundation", "Daintree orchestration foundation"},
				[]string{"multi_agent", "Multi-agent orchestration"}),
		},
		sseRound{
			contentTokens: []string{"Nothing ", "stored."},
			usage:         &fakeUsage{prompt: 70, completion: 4, total: 74, cached: 20},
			// Round 2 loads nothing and DEGRADES: the eager cue is silent for this round
			// entirely, so only the decision reports that the set was kept by fail-open.
			skills: skillsBlock(true,
				[]string{"multi_agent", "Multi-agent orchestration",
					"foundation", "Daintree orchestration foundation"},
				nil),
		},
	)

	dir := t.TempDir()
	cmd := exec.Command(bin, "--json", "what's in memory?")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_BACKEND_URL="+fake.baseURL(),
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

	// --- the skill decision reaches the real --json stream, once per round ---
	//
	// This is the ONLY assertion that covers the whole production path at once: SSE
	// decode → the committed OnMeta callback → the session's projection → MultiSink →
	// app.eventProxy → the jsonout sink. Every other test for this feature exercises one
	// of those links in isolation, so any of them could become a no-op undetected.
	var decisions []map[string]any
	starts := 0
	for _, l := range lines {
		switch l.Type {
		case "assistant:start":
			starts++
		case "skill:decision":
			decisions = append(decisions, l.raw)
		}
	}
	if starts != 2 {
		t.Fatalf("assistant:start count = %d, want 2 rounds: %v", starts, typesOf(lines))
	}
	if len(decisions) != starts {
		t.Fatalf("skill:decision count = %d, want one per round (%d): %v",
			len(decisions), starts, typesOf(lines))
	}

	// Round 1: ids AND titles, and the whole active set — including the retained skill
	// that never appeared in newly_loaded.
	active, _ := decisions[0]["active"].([]any)
	if len(active) != 2 {
		t.Fatalf("round 1 active = %#v, want both the loaded and the retained skill", decisions[0]["active"])
	}
	first, _ := active[0].(map[string]any)
	if first["id"] != "multi_agent" || first["title"] != "Multi-agent orchestration" {
		t.Errorf("round 1 active[0] = %#v, want id+title carried through the wire", first)
	}
	if newly, _ := decisions[0]["newlyLoaded"].([]any); len(newly) != 1 {
		t.Errorf("round 1 newlyLoaded = %#v, want just the delta", decisions[0]["newlyLoaded"])
	}
	// The backend sends snake_case; the stream is camelCase. A regression that passed the
	// wire block through verbatim would show up right here.
	if _, present := decisions[0]["newly_loaded"]; present {
		t.Error("round 1 line carries the backend's snake_case newly_loaded key")
	}

	// Round 2: nothing newly loaded, and the fail-open flag — the round the eager
	// skill:loaded cue is completely silent for.
	sel2, _ := decisions[1]["selector"].(map[string]any)
	if sel2["degraded"] != true {
		t.Errorf("round 2 selector = %#v, want degraded:true", decisions[1]["selector"])
	}
	// `ok` matters: an ignored assertion yields a nil slice, so a missing, null or
	// wrongly-typed field would satisfy a bare length check.
	if newly, ok := decisions[1]["newlyLoaded"].([]any); !ok || len(newly) != 0 {
		t.Errorf("round 2 newlyLoaded = %#v, want an empty array", decisions[1]["newlyLoaded"])
	}
	if act2, _ := decisions[1]["active"].([]any); len(act2) != 2 {
		t.Errorf("round 2 active = %#v, want the set it fell open into", decisions[1]["active"])
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

	// --- the session header is FIRST and names this run ---
	// Without it a --json consumer can parse a failing run perfectly and still have no
	// way to reach the trace that explains it: the session id and log path used to exist
	// only as human prose on stderr.
	if lines[0].Type != "session" {
		t.Fatalf("first line = %q, want session: %v", lines[0].Type, types)
	}
	sess := lines[0].raw
	if id, _ := sess["sessionId"].(string); !strings.HasPrefix(id, "ses_") {
		t.Errorf("session.sessionId = %v, want a ses_ id", sess["sessionId"])
	}
	if got, _ := sess["backendUrl"].(string); got != fake.baseURL() {
		t.Errorf("session.backendUrl = %v, want %q", sess["backendUrl"], fake.baseURL())
	}
	// This run has no MCP, which is exactly the degraded local mode a harness must be
	// able to detect: it is invisible in the content and is the commonest cause of a
	// confusing answer.
	// Check `ok`: a bare `v, _ := ….(bool)` yields false for a MISSING field too, so a
	// dropped key would pass this assertion silently.
	if connected, ok := sess["mcpConnected"].(bool); !ok || connected {
		t.Errorf("session.mcpConnected = %v (present=%v), want false and present", sess["mcpConnected"], ok)
	}
	for _, key := range []string{"project", "tier", "logPath", "version", "autoApprove"} {
		if _, ok := sess[key]; !ok {
			t.Errorf("session line is missing %q: %v", key, sess)
		}
	}

	// --- the terminal envelope reports what the run cost ---
	stats, ok := last.raw["stats"].(map[string]any)
	if !ok {
		t.Fatalf("result has no stats block: %v", last.raw)
	}
	// Every assertion checks `ok`: a bare `v, _ := ….(float64)` reads a MISSING key as 0,
	// so a dropped field would silently satisfy any zero-valued expectation.
	//
	// Two SSE rounds were scripted, with one tool call between them. The fake reports
	// 50+70 prompt and 6+4 completion tokens across those rounds; contextTokens is the
	// LAST round's prompt size, not the sum.
	for key, want := range map[string]int{
		"rounds": 2, "toolCalls": 1, "toolErrors": 0,
		"promptTokens": 120, "completionTokens": 10, "totalTokens": 130,
		"contextTokens": 70,
	} {
		got, ok := stats[key].(float64)
		if !ok {
			t.Errorf("stats.%s is missing or not a number: %v", key, stats[key])
			continue
		}
		if int(got) != want {
			t.Errorf("stats.%s = %v, want %d", key, got, want)
		}
	}
	if d, ok := stats["durationMs"].(float64); !ok || d <= 0 {
		t.Errorf("stats.durationMs = %v (present=%v), want > 0", stats["durationMs"], ok)
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

// schedulerActiveOnWire reads runtime.scheduler_active from the Nth respond request.
// The flag's whole invisible half is this wire value: the backend defaults it to true,
// so an explicit false is what tells the model background work is unavailable.
func schedulerActiveOnWire(t *testing.T, f *fakeBackend, n int) bool {
	t.Helper()
	body := f.request(n)
	if body == nil {
		t.Fatalf("no respond request at index %d", n)
	}
	rt, ok := body["runtime"].(map[string]any)
	if !ok {
		t.Fatalf("request %d has no runtime block: %v", n, body)
	}
	active, ok := rt["scheduler_active"].(bool)
	if !ok {
		t.Fatalf("runtime.scheduler_active missing or not a bool: %v", rt)
	}
	return active
}

// TestBinaryOneShotSchedulerActiveOnTheWire runs the real binary twice against the fake
// backend — once plain, once with --run-scheduler — and asserts the runtime fact the
// model actually reads. The default must keep reporting false (a one-shot that does not
// tick must not claim it does), and the opt-in must report true on the FIRST round,
// because that is the round where the model decides whether to start background work at
// all. Both runs must exit 0 and leave no owner lease behind.
func TestBinaryOneShotSchedulerActiveOnTheWire(t *testing.T) {
	bin := buildBinary(t)

	run := func(t *testing.T, extraArgs ...string) (*fakeBackend, string) {
		t.Helper()
		fake := newFakeBackend(t, sseRound{
			contentTokens: []string{"All ", "clear."},
			usage:         &fakeUsage{prompt: 40, completion: 3, total: 43},
		})
		dir := t.TempDir()
		args := append(append([]string{}, extraArgs...), "--json", "anything running?")
		cmd := exec.Command(bin, args...)
		cmd.Env = append(cmd.Environ(),
			"DAINTREE_BACKEND_URL="+fake.baseURL(),
			"DAINTREE_ASSISTANT_STATE_DIR="+dir,
			"DAINTREE_ASSISTANT_TIER=operator",
			"DAINTREE_ASSISTANT_DEBUG_LOG=0",
			"DAINTREE_MCP_URL=",
			"DAINTREE_MCP_TOKEN=",
		)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("run %v: %v (stderr: %s)", args, err, stderr.String())
		}
		if fake.callCount() == 0 {
			t.Fatalf("backend served no respond requests (stderr: %s)", stderr.String())
		}
		return fake, dir
	}

	t.Run("default reports an inactive scheduler", func(t *testing.T) {
		fake, _ := run(t)
		if schedulerActiveOnWire(t, fake, 0) {
			t.Error("runtime.scheduler_active = true without --run-scheduler, want false")
		}
	})

	t.Run("--run-scheduler reports an active scheduler on the first round", func(t *testing.T) {
		fake, dir := run(t, "--run-scheduler", "--timeout", "60s")
		if !schedulerActiveOnWire(t, fake, 0) {
			t.Error("runtime.scheduler_active = false with --run-scheduler, want true on the FIRST round")
		}
		// The lease must be free the moment the process is gone: a one-shot that takes
		// the lease and starts ticking still has to leave nothing behind.
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				if strings.Contains(e.Name(), "daemon") {
					t.Errorf("one-shot left a daemon artifact behind: %s", e.Name())
				}
			}
		}
	})
}

// TestBinaryRunSchedulerRequiresTimeout: the bound is not optional, and the rejection
// has to happen at the argument boundary — before a lease is taken or a turn is spent.
func TestBinaryRunSchedulerRequiresTimeout(t *testing.T) {
	bin := buildBinary(t)

	cmd := exec.Command(bin, "--run-scheduler", "--json", "anything running?")
	cmd.Env = append(cmd.Environ(),
		"DAINTREE_ASSISTANT_STATE_DIR="+t.TempDir(),
		"DAINTREE_ASSISTANT_DEBUG_LOG=0",
		"DAINTREE_MCP_URL=",
		"DAINTREE_MCP_TOKEN=",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err == nil {
		t.Fatalf("--run-scheduler without --timeout exited 0 (stdout: %q)", stdout.String())
	}
	if !strings.Contains(stderr.String(), "--timeout") {
		t.Errorf("rejection does not name --timeout:\n%s", stderr.String())
	}
}
