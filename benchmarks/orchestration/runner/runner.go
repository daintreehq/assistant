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
	"sort"
	"strconv"
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

	// Latency decomposition (see scenario.RoundMetric).
	TurnMS          int64                  `json:"turnMs,omitempty"`
	FirstRawMetaMS  int64                  `json:"firstRawMetaMs,omitempty"`
	FirstSkillCueMS int64                  `json:"firstSkillCueMs,omitempty"`
	FirstContentMS  int64                  `json:"firstContentMs,omitempty"`
	RoundDetail     []scenario.RoundMetric `json:"roundDetail,omitempty"`
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
	res.TurnMS = rr.TurnMS
	res.FirstRawMetaMS = rr.FirstRawMetaMS
	res.FirstSkillCueMS = rr.FirstSkillCueMS
	res.FirstContentMS = rr.FirstContentMS
	res.RoundDetail = rr.RoundDetail
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

// roundTimeline accumulates one model round's trace events while scanning the log.
type roundTimeline struct {
	round           int
	requestTS       time.Time
	rawMetaTS       time.Time
	skillCueTS      time.Time
	committedMetaTS time.Time
	doneTS          time.Time
	totalMS         int64
	firstTokMS      int64
	finish          string
	usage           scenario.UsageTotals
	hasDone         bool
}

// parseDebugLog reconstructs the turn's timeline from the trace events:
// turn.start → per-round backend.respond.{request,raw_meta,skill_cue,meta,done}
// → turn.end. It fills round counts + usage totals (as before) AND the per-round
// latency decomposition. Format
// (debuglog.formatLine): "<ISO-ts>  <event>[  k=v ...]" lines with ms-precision
// UTC timestamps, then indented blocks; `usage` is a block of indented JSON
// under backend.respond.done.
func parseDebugLog(rr *scenario.RunResult) {
	if rr.DebugLogPath == "" {
		return
	}
	f, err := os.Open(rr.DebugLogPath)
	if err != nil {
		return
	}
	defer f.Close()

	var (
		turnStart time.Time
		runID     string // first turn's runId — later turns (a wake, a retry) are ignored
		rounds    = map[int]*roundTimeline{}
		curDone   *roundTimeline // done event whose indented usage block is pending
		inUsage   bool
		usageBuf  strings.Builder
	)
	ensure := func(n int) *roundTimeline {
		if rt, ok := rounds[n]; ok {
			return rt
		}
		rt := &roundTimeline{round: n}
		rounds[n] = rt
		return rt
	}
	flushUsage := func() {
		if usageBuf.Len() == 0 {
			return
		}
		var u struct {
			PromptTokens     int `json:"promptTokens"`
			CompletionTokens int `json:"completionTokens"`
			CachedTokens     int `json:"cachedTokens"`
		}
		if json.Unmarshal([]byte(usageBuf.String()), &u) == nil && curDone != nil {
			curDone.usage = scenario.UsageTotals{
				PromptTokens:     u.PromptTokens,
				CompletionTokens: u.CompletionTokens,
				CachedTokens:     u.CachedTokens,
			}
		}
		usageBuf.Reset()
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		isEventLine := len(line) > 0 && line[0] >= '0' && line[0] <= '9'
		if isEventLine {
			flushUsage()
			inUsage = false
			// Line shape: "<ts>  <event>[  k=v ...]" — compare the event token
			// exactly (a substring match could hit an inline field value).
			parts := strings.SplitN(line, "  ", 3)
			if len(parts) < 2 {
				curDone = nil
				continue
			}
			ts, tsErr := time.Parse(time.RFC3339, parts[0])
			rest := ""
			if len(parts) == 3 {
				rest = parts[2]
			}
			// Scope to the FIRST turn: a wake/retry turn in the same log (runId
			// differs) must not merge its round numbers into this timeline. Read
			// runId from the RIGHT — free-text previews sort before it and could
			// contain a "  runId=" lookalike; every real field after it is a
			// machine value.
			if evRun := strFieldLast(rest, "runId"); runID != "" && evRun != "" && evRun != runID {
				curDone = nil
				continue
			}
			switch parts[1] {
			case "turn.start":
				// promptPreview (free user text) sorts before runId — read from the right.
				if turnStart.IsZero() && tsErr == nil {
					turnStart = ts
					runID = strFieldLast(rest, "runId")
				}
				curDone = nil
			case "backend.respond.request":
				if n, ok := intField(rest, "round"); ok && tsErr == nil {
					rt := ensure(n)
					if rt.requestTS.IsZero() {
						rt.requestTS = ts
					}
				}
				curDone = nil
			case "backend.respond.raw_meta":
				if n, ok := intField(rest, "round"); ok && tsErr == nil {
					rt := ensure(n)
					if rt.rawMetaTS.IsZero() {
						rt.rawMetaTS = ts
					}
				}
				curDone = nil
			case "backend.respond.skill_cue":
				if n, ok := intField(rest, "round"); ok && tsErr == nil {
					rt := ensure(n)
					if rt.skillCueTS.IsZero() {
						rt.skillCueTS = ts
					}
				}
				curDone = nil
			case "backend.respond.meta":
				if n, ok := intField(rest, "round"); ok && tsErr == nil {
					rt := ensure(n)
					if rt.committedMetaTS.IsZero() {
						rt.committedMetaTS = ts
					}
				}
				curDone = nil
			case "backend.respond.done":
				// done lines carry contentPreview — free model text that sorts
				// BEFORE these keys and may itself contain "  key=" lookalikes, so
				// extract from the RIGHT (the fields after the preview are all
				// machine values). See strFieldLast.
				if n, ok := intFieldLast(rest, "round"); ok {
					rt := ensure(n)
					if tsErr == nil {
						rt.doneTS = ts
					}
					rt.hasDone = true
					rt.totalMS = int64FieldLast(rest, "durationMs")
					rt.firstTokMS = int64FieldLast(rest, "firstTokenMs")
					rt.finish = strFieldLast(rest, "finishReason")
					curDone = rt
				} else {
					curDone = nil
				}
			case "backend.respond.error":
				// A failed round has no done event. Record its end so RoundDetail is
				// honest (TotalMS + finish=error) and the NEXT round's GapBefore
				// chains from the failure instant, not from this round's request.
				// hasDone stays false: rr.Rounds remains "completed rounds".
				if n, ok := intField(rest, "round"); ok {
					rt := ensure(n)
					if tsErr == nil {
						rt.doneTS = ts
					}
					rt.totalMS = int64Field(rest, "durationMs")
					rt.finish = "error"
				}
				curDone = nil
			case "turn.end":
				// replyPreview sorts AFTER durationMs here, so first-match is the
				// real field (the preview can't shadow a key that precedes it).
				if rr.TurnMS == 0 {
					rr.TurnMS = int64Field(rest, "durationMs")
				}
				curDone = nil
			default:
				curDone = nil
			}
			continue
		}
		if curDone == nil {
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
				flushUsage()
				inUsage = false
			}
		}
	}
	flushUsage()

	// Assemble the ordered decomposition. GapBefore chains prior-done → request
	// (round 0 chains from turn.start), so tool time and CLI bookkeeping between
	// rounds is visible; an errored round (no done) chains from its request.
	nums := make([]int, 0, len(rounds))
	for n := range rounds {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	prev := turnStart
	for _, n := range nums {
		rt := rounds[n]
		rm := scenario.RoundMetric{
			Round:            n,
			TotalMS:          rt.totalMS,
			FirstTokenMS:     rt.firstTokMS,
			FinishReason:     rt.finish,
			PromptTokens:     rt.usage.PromptTokens,
			CachedTokens:     rt.usage.CachedTokens,
			CompletionTokens: rt.usage.CompletionTokens,
		}
		if !rt.requestTS.IsZero() {
			if !rt.rawMetaTS.IsZero() {
				rm.RawMetaMS = rt.rawMetaTS.Sub(rt.requestTS).Milliseconds()
			}
			if !rt.skillCueTS.IsZero() {
				rm.SkillCueMS = rt.skillCueTS.Sub(rt.requestTS).Milliseconds()
			}
			if !rt.committedMetaTS.IsZero() {
				rm.CommittedMetaMS = rt.committedMetaTS.Sub(rt.requestTS).Milliseconds()
			}
		}
		if !rt.requestTS.IsZero() && !prev.IsZero() {
			rm.GapBeforeMS = rt.requestTS.Sub(prev).Milliseconds()
		}
		if rt.hasDone {
			rr.Rounds++
			rr.Usage.PromptTokens += rt.usage.PromptTokens
			rr.Usage.CompletionTokens += rt.usage.CompletionTokens
			rr.Usage.CachedTokens += rt.usage.CachedTokens
		}
		switch {
		case !rt.doneTS.IsZero():
			prev = rt.doneTS
		case !rt.requestTS.IsZero():
			prev = rt.requestTS
		}
		rr.RoundDetail = append(rr.RoundDetail, rm)
	}
	if len(nums) > 0 && !turnStart.IsZero() {
		first := rounds[nums[0]]
		if !first.rawMetaTS.IsZero() {
			rr.FirstRawMetaMS = first.rawMetaTS.Sub(turnStart).Milliseconds()
		}
		var firstCue, firstContent time.Time
		for _, n := range nums {
			rt := rounds[n]
			if !rt.skillCueTS.IsZero() && (firstCue.IsZero() || rt.skillCueTS.Before(firstCue)) {
				firstCue = rt.skillCueTS
			}
			if rt.firstTokMS > 0 && !rt.requestTS.IsZero() {
				ts := rt.requestTS.Add(time.Duration(rt.firstTokMS) * time.Millisecond)
				if firstContent.IsZero() || ts.Before(firstContent) {
					firstContent = ts
				}
			}
		}
		if !firstCue.IsZero() {
			rr.FirstSkillCueMS = firstCue.Sub(turnStart).Milliseconds()
		}
		if !firstContent.IsZero() {
			rr.FirstContentMS = firstContent.Sub(turnStart).Milliseconds()
		}
	}
}

// Inline-field extraction. formatLine renders fields ALPHABETICALLY as
// "  k=v" pairs, and free-text string values (contentPreview, replyPreview,
// promptPreview — bounded, newline-free) are NOT escaped, so a preview can
// contain a "  key=" lookalike. Disambiguation uses the sorted order:
//   - a key that sorts BEFORE every free-text field on its line → first match
//     is the real field (strField/intField);
//   - a key that sorts AFTER the free-text field, with only machine values
//     behind it → LAST match is the real field (strFieldLast/intFieldLast).
// Every extraction call site notes which case applies.

// intField extracts an integer "  k=v" inline field (first match).
func intField(rest, key string) (int, bool) {
	return parseIntField(strField(rest, key))
}

// intFieldLast is intField scanning from the right (last match).
func intFieldLast(rest, key string) (int, bool) {
	return parseIntField(strFieldLast(rest, key))
}

func parseIntField(v string) (int, bool) {
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

// int64Field is intField for int64, returning 0 when absent.
func int64Field(rest, key string) int64 {
	n, ok := intField(rest, key)
	if !ok {
		return 0
	}
	return int64(n)
}

// int64FieldLast is int64Field scanning from the right.
func int64FieldLast(rest, key string) int64 {
	n, ok := intFieldLast(rest, key)
	if !ok {
		return 0
	}
	return int64(n)
}

// strField extracts a string inline field value (up to the next two-space
// separator or end of line), taking the FIRST occurrence. Empty when absent.
func strField(rest, key string) string {
	tok := key + "="
	var idx int
	switch {
	case strings.HasPrefix(rest, tok):
		idx = 0
	default:
		i := strings.Index(rest, "  "+tok)
		if i < 0 {
			return ""
		}
		idx = i + 2
	}
	return fieldValueAt(rest, idx+len(tok))
}

// strFieldLast is strField taking the LAST occurrence — for keys that sort
// after a free-text preview field on their line.
func strFieldLast(rest, key string) string {
	tok := key + "="
	if i := strings.LastIndex(rest, "  "+tok); i >= 0 {
		return fieldValueAt(rest, i+2+len(tok))
	}
	if strings.HasPrefix(rest, tok) {
		return fieldValueAt(rest, len(tok))
	}
	return ""
}

// fieldValueAt returns the field value starting at byte offset idx, ending at
// the next two-space separator or end of line.
func fieldValueAt(rest string, idx int) string {
	val := rest[idx:]
	if end := strings.Index(val, "  "); end >= 0 {
		val = val[:end]
	}
	return val
}
