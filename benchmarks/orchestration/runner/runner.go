// Package runner executes benchmark scenarios: it builds the real CLI binary,
// spins a fresh fake-Daintree world + isolated state dir per run, drives one
// one-shot `--json` turn against the LIVE local backend, and grades the result
// with the scenario's checks. The model turns are real (they are the system
// under test); everything the model orchestrates is simulated.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/daintreehq/daintree-assistant/benchmarks/orchestration/scenario"
	"github.com/daintreehq/daintree-assistant/benchmarks/orchestration/world"
)

// Options configures a bench run.
type Options struct {
	BackendURL string        // live backend (default http://127.0.0.1:8473)
	BinPath    string        // pre-built CLI binary; empty = build from source
	WorkDir    string        // scratch root; empty = os.MkdirTemp
	Timeout    time.Duration // default per-scenario timeout when the scenario sets none
}

// DefaultTimeout bounds a scenario when it doesn't set its own. Generous: a
// spawn+supervise scenario legitimately spends 20s+ in settle grace alone.
const DefaultTimeout = 5 * time.Minute

// BuildBinary compiles the CLI into dir and returns the binary path.
func BuildBinary(repoRoot, dir string) (string, error) {
	bin := filepath.Join(dir, "daintree-assistant")
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/daintree-assistant")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %v\n%s", err, out)
	}
	return bin, nil
}

// CheckOutcome is one graded check.
type CheckOutcome struct {
	Name  string `json:"name"`
	Pass  bool   `json:"pass"`
	Error string `json:"error,omitempty"`
}

// ScenarioResult is the persisted outcome of one scenario trial.
type ScenarioResult struct {
	ID           string         `json:"id"`
	Category     string         `json:"category"`
	Trial        int            `json:"trial"`
	Passed       bool           `json:"passed"`
	Checks       []CheckOutcome `json:"checks"`
	DurationMS   int64          `json:"durationMs"`
	Rounds       int            `json:"rounds"`
	ToolCalls    int            `json:"toolCalls"`
	WorldCalls   int            `json:"worldCalls"`
	PromptTok    int            `json:"promptTokens"`
	CompletionTk int            `json:"completionTokens"`
	CachedTok    int            `json:"cachedTokens"`
	Status       string         `json:"status"`
	TimedOut     bool           `json:"timedOut"`
	Answer       string         `json:"answer"`
	DebugLog     string         `json:"debugLog"`
	Error        string         `json:"error,omitempty"`
}

// RunScenario executes one trial of one scenario.
func RunScenario(ctx context.Context, bin string, sc scenario.Scenario, opts Options, trial int) ScenarioResult {
	res := ScenarioResult{ID: sc.ID, Category: sc.Category, Trial: trial}

	w := world.New()
	if sc.Setup != nil {
		sc.Setup(w)
	}
	srv := world.Serve(w)
	defer srv.Close()

	scratch := filepath.Join(opts.WorkDir, fmt.Sprintf("%s-t%d", sc.ID, trial))
	stateDir := filepath.Join(scratch, "state")
	logDir := filepath.Join(scratch, "logs")
	for _, d := range []string{stateDir, logDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			res.Error = err.Error()
			return res
		}
	}

	timeout := sc.Timeout
	if timeout <= 0 {
		timeout = opts.Timeout
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, "--json", sc.Prompt)
	// Run in the scratch dir: the CLI treats its CWD as the bound project path
	// (project instructions etc.), so an empty dir isolates the trial from
	// whatever directory the harness was invoked from.
	cmd.Dir = scratch
	// Kill the whole process group on timeout — CommandContext alone only kills
	// the direct child, and a stuck grandchild holding the pipes would hang
	// cmd.Run() past the deadline. WaitDelay is the final bound either way.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 15 * time.Second
	// Inherit the real env first, then pin every knob that matters. The trusted
	// vars (tier, autoApprove, state dir, debug log) must be real process env.
	cmd.Env = append(os.Environ(),
		"DAINTREE_MCP_URL="+srv.URL,
		"DAINTREE_MCP_TOKEN=bench-token",
		"DAINTREE_BACKEND_URL="+opts.BackendURL,
		"DAINTREE_ASSISTANT_STATE_DIR="+stateDir,
		"DAINTREE_ASSISTANT_LOG_DIR="+logDir,
		"DAINTREE_ASSISTANT_DEBUG_LOG=1",
		"DAINTREE_ASSISTANT_TIER=system",
		"DAINTREE_ASSISTANT_AUTO_APPROVE=1",
		"DAINTREE_ASSISTANT_NO_DAEMON=1",
		"DAINTREE_ASSISTANT_OFFLINE=",
		"DAINTREE_PROJECT_ID=",
		"DAINTREE_WINDOW_ID=",
		// Clears the CLI's vestigial one-shot key gate; the backend holds the real key.
		"DEEPSEEK_API_KEY=placeholder-cli-gate-only",
	)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	runErr := cmd.Run()
	duration := time.Since(started)

	rr := &scenario.RunResult{
		World:    w,
		Duration: duration,
		TimedOut: runCtx.Err() == context.DeadlineExceeded,
		Stderr:   stderr.String(),
	}
	procExit := 0
	if runErr != nil && !rr.TimedOut {
		if ee, ok := runErr.(*exec.ExitError); ok {
			procExit = ee.ExitCode()
		} else {
			res.Error = fmt.Sprintf("run binary: %v (stderr: %s)", runErr, stderr.String())
			return res
		}
	}

	parseJSONL(stdout.Bytes(), rr)
	// A process that died (panic, signal) without writing the terminal `result`
	// envelope must not read as a clean-but-empty run: surface it explicitly.
	if !rr.TimedOut && rr.Status == "" {
		rr.ExitCode = procExit
		res.Error = fmt.Sprintf("process exited %d without a terminal result envelope (stderr: %s)",
			procExit, truncateStr(stderr.String(), 400))
	}
	rr.DebugLogPath = findDebugLog(logDir)
	parseDebugLog(rr)

	// Grade.
	passed := true
	for _, c := range sc.Checks {
		out := CheckOutcome{Name: c.Name, Pass: true}
		if err := c.Fn(rr); err != nil {
			out.Pass = false
			out.Error = err.Error()
			passed = false
		}
		res.Checks = append(res.Checks, out)
	}

	res.Passed = passed
	res.DurationMS = duration.Milliseconds()
	res.Rounds = rr.Rounds
	res.ToolCalls = len(rr.ToolCalls(""))
	res.WorldCalls = len(w.Calls())
	res.PromptTok = rr.Usage.PromptTokens
	res.CompletionTk = rr.Usage.CompletionTokens
	res.CachedTok = rr.Usage.CachedTokens
	res.Status = rr.Status
	res.TimedOut = rr.TimedOut
	res.Answer = rr.FinalContent
	res.DebugLog = rr.DebugLogPath
	return res
}

// parseJSONL fills events, final content, status and exit code from the --json stream.
func parseJSONL(out []byte, rr *scenario.RunResult) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var raw map[string]any
		if err := json.Unmarshal([]byte(line), &raw); err != nil {
			continue
		}
		typ, _ := raw["type"].(string)
		seq, _ := raw["seq"].(float64)
		rr.Events = append(rr.Events, scenario.Event{Type: typ, Seq: int(seq), Raw: raw})
		if typ == "result" {
			if s, ok := raw["status"].(string); ok {
				rr.Status = s
			}
			if c, ok := raw["exitCode"].(float64); ok {
				rr.ExitCode = int(c)
			}
			if c, ok := raw["content"].(string); ok {
				rr.FinalContent = c
			}
		}
	}
}

func truncateStr(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// findDebugLog returns the single per-session log file the run wrote.
func findDebugLog(logDir string) string {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".log") {
			return filepath.Join(logDir, e.Name())
		}
	}
	return ""
}

// parseDebugLog extracts rounds + usage totals from backend.respond.done events.
// Format (debuglog.formatLine): a timestamped event line with inline scalars,
// then indented blocks; `usage` is a block holding indented JSON.
func parseDebugLog(rr *scenario.RunResult) {
	if rr.DebugLogPath == "" {
		return
	}
	f, err := os.Open(rr.DebugLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	inDone := false
	inUsage := false
	var usageBuf strings.Builder
	flush := func() {
		if usageBuf.Len() == 0 {
			return
		}
		var u struct {
			PromptTokens     int `json:"promptTokens"`
			CompletionTokens int `json:"completionTokens"`
			CachedTokens     int `json:"cachedTokens"`
		}
		if json.Unmarshal([]byte(usageBuf.String()), &u) == nil {
			rr.Usage.PromptTokens += u.PromptTokens
			rr.Usage.CompletionTokens += u.CompletionTokens
			rr.Usage.CachedTokens += u.CachedTokens
		}
		usageBuf.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		isEventLine := len(line) > 0 && line[0] >= '0' && line[0] <= '9'
		if isEventLine {
			flush()
			inUsage = false
			// Line shape: "<ts>  <event>[  k=v ...]" — compare the event token
			// exactly (a substring match could hit an inline field value).
			parts := strings.SplitN(line, "  ", 3)
			inDone = len(parts) >= 2 && parts[1] == "backend.respond.done"
			if inDone {
				rr.Rounds++
			}
			continue
		}
		if !inDone {
			continue
		}
		if strings.HasPrefix(line, "  usage:") {
			inUsage = true
			continue
		}
		if inUsage {
			if strings.HasPrefix(line, "    ") {
				usageBuf.WriteString(strings.TrimPrefix(line, "    "))
				usageBuf.WriteString("\n")
			} else {
				flush()
				inUsage = false
			}
		}
	}
	flush()
}
