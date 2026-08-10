// Package scenario defines the benchmark cases: a frozen prompt + a scripted
// fake-Daintree world + objective pass predicates. Grading philosophy: a
// scenario passes when the orchestrator REACHED THE RESULT — the world's call
// log shows the right effects and the final answer carries facts that only
// exist inside the fake world (nonce strings planted in agent output) — never
// when its prose merely "sounds right", and never by pinning the exact route
// it took.
package scenario

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/daintreehq/assistant/benchmarks/orchestration/world"
)

// Scenario is one benchmark case.
type Scenario struct {
	ID       string
	Category string // status | spawn | supervise | extract | interact | fault
	Prompt   string
	Timeout  time.Duration // hard kill for the one-shot process
	Setup    func(w *world.World)
	Checks   []Check
	Notes    string
}

// Check is one named pass predicate over a finished run.
type Check struct {
	Name string
	Fn   func(r *RunResult) error
}

// RunResult is everything a check can grade: the world (ground truth), the
// parsed JSONL stream, and the debug-log usage figures.
type RunResult struct {
	World        *world.World
	Events       []Event
	FinalContent string
	Status       string // success | error | cancelled
	ExitCode     int
	Duration     time.Duration
	Rounds       int // backend.respond.done count from the debug log
	Usage        UsageTotals
	DebugLogPath string
	Stderr       string
	TimedOut     bool

	// Latency decomposition, reconstructed from the debug-log timeline.
	TurnMS          int64         // turn.start → turn.end (excludes process boot/exit)
	FirstRawMetaMS  int64         // turn.start → round 0's raw SSE meta
	FirstSkillCueMS int64         // turn.start → first eager skill-loaded cue (0 when none)
	FirstContentMS  int64         // turn.start → first visible content across all rounds (0 when none)
	RoundDetail     []RoundMetric // one entry per model round, in round order
}

// RoundMetric is one model round's latency decomposition. RawMetaMS observes the
// actual SSE meta arrival; SkillCueMS observes the optional eager user cue;
// CommittedMetaMS observes retry-safe state adoption; and FirstTokenMS observes
// visible model content.
type RoundMetric struct {
	Round            int    `json:"round"`
	GapBeforeMS      int64  `json:"gapBeforeMs"`               // prior round's done → this request: tool execution + CLI bookkeeping (round 0: turn.start → request)
	RawMetaMS        int64  `json:"rawMetaMs,omitempty"`       // request → raw SSE meta arrival
	SkillCueMS       int64  `json:"skillCueMs,omitempty"`      // request → eager skill-loaded cue (0 when none)
	CommittedMetaMS  int64  `json:"committedMetaMs,omitempty"` // request → retry-safe OnMeta callback
	FirstTokenMS     int64  `json:"firstTokenMs,omitempty"`    // request → first content delta (0 on tool-call-only rounds)
	TotalMS          int64  `json:"totalMs"`                   // request → done (the whole round)
	PromptTokens     int    `json:"promptTokens"`
	CachedTokens     int    `json:"cachedTokens"`
	CompletionTokens int    `json:"completionTokens"`
	FinishReason     string `json:"finishReason,omitempty"`
}

// CacheHitPct is the round's prompt-cache hit rate in percent (0 when unknown).
func (m RoundMetric) CacheHitPct() float64 {
	if m.PromptTokens <= 0 {
		return 0
	}
	return 100 * float64(m.CachedTokens) / float64(m.PromptTokens)
}

// Event is one parsed JSONL line from the --json stream.
type Event struct {
	Type string
	Seq  int
	Raw  map[string]any
}

// UsageTotals aggregates backend-reported usage across rounds.
type UsageTotals struct {
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// ToolCalls returns the tool:call events, optionally filtered by name.
func (r *RunResult) ToolCalls(name string) []Event {
	var out []Event
	for _, e := range r.Events {
		if e.Type != "tool:call" {
			continue
		}
		if n, _ := e.Raw["name"].(string); name == "" || n == name {
			out = append(out, e)
		}
	}
	return out
}

// ToolResults returns the tool:result events, optionally filtered by name.
func (r *RunResult) ToolResults(name string) []Event {
	var out []Event
	for _, e := range r.Events {
		if e.Type != "tool:result" {
			continue
		}
		if n, _ := e.Raw["name"].(string); name == "" || n == name {
			out = append(out, e)
		}
	}
	return out
}

// --- check helpers -----------------------------------------------------------

// ResultSuccess passes when the run finished (no timeout) with a success envelope.
func ResultSuccess() Check {
	return Check{Name: "result:success", Fn: func(r *RunResult) error {
		if r.TimedOut {
			return fmt.Errorf("run hit the %s scenario timeout (turn never ended)", r.Duration.Round(time.Second))
		}
		if r.Status != "success" {
			return fmt.Errorf("terminal status = %q (exit %d), want success", r.Status, r.ExitCode)
		}
		return nil
	}}
}

// AnswerContains passes when the final assistant content contains EVERY needle
// (case-insensitive). Use with nonces planted in fake agent output: it proves
// end-to-end information flow, not phrasing.
func AnswerContains(needles ...string) Check {
	return Check{Name: "answer:contains " + strings.Join(needles, "+"), Fn: func(r *RunResult) error {
		content := strings.ToLower(r.FinalContent)
		for _, n := range needles {
			if !strings.Contains(content, strings.ToLower(n)) {
				return fmt.Errorf("final answer missing %q (answer: %s)", n, truncate(r.FinalContent, 400))
			}
		}
		return nil
	}}
}

// AnswerMatches passes when the final content matches the pattern.
func AnswerMatches(pattern string) Check {
	re := regexp.MustCompile(pattern)
	return Check{Name: "answer:matches " + pattern, Fn: func(r *RunResult) error {
		if !re.MatchString(r.FinalContent) {
			return fmt.Errorf("final answer does not match /%s/ (answer: %s)", pattern, truncate(r.FinalContent, 400))
		}
		return nil
	}}
}

// WorldCalled passes when the fake MCP served tool at least min times.
func WorldCalled(tool string, min int) Check {
	return Check{Name: fmt.Sprintf("world:%s>=%d", tool, min), Fn: func(r *RunResult) error {
		if got := r.World.CallCount(tool); got < min {
			return fmt.Errorf("world served %s %d times, want >= %d", tool, got, min)
		}
		return nil
	}}
}

// WorldNotCalled passes when the fake MCP never served tool.
func WorldNotCalled(tool string) Check {
	return Check{Name: "world:not " + tool, Fn: func(r *RunResult) error {
		if got := r.World.CallCount(tool); got > 0 {
			return fmt.Errorf("world served %s %d times, want 0", tool, got)
		}
		return nil
	}}
}

// SpawnCount passes when exactly n agents were launched during the run.
func SpawnCount(n int) Check {
	return Check{Name: fmt.Sprintf("spawn:count=%d", n), Fn: func(r *RunResult) error {
		if got := len(r.World.Spawned()); got != n {
			return fmt.Errorf("%d agents launched, want %d", got, n)
		}
		return nil
	}}
}

// SpawnedInWorktree passes when at least one launched agent landed in worktreeID.
func SpawnedInWorktree(worktreeID string) Check {
	return Check{Name: "spawn:worktree=" + worktreeID, Fn: func(r *RunResult) error {
		var got []string
		for _, t := range r.World.Spawned() {
			if t.WorktreeID == worktreeID {
				return nil
			}
			got = append(got, t.WorktreeID)
		}
		return fmt.Errorf("no agent launched in worktree %q (spawned in: %v)", worktreeID, got)
	}}
}

// InputSent passes when some terminal received input containing needle.
func InputSent(needle string) Check {
	return Check{Name: "input:contains " + needle, Fn: func(r *RunResult) error {
		if !strings.Contains(strings.ToLower(r.World.AllInputs()), strings.ToLower(needle)) {
			return fmt.Errorf("no terminal received input containing %q (inputs: %s)", needle, truncate(r.World.AllInputs(), 300))
		}
		return nil
	}}
}

// ToolOK passes when at least one local tool:result for name succeeded.
func ToolOK(name string) Check {
	return Check{Name: "tool:ok " + name, Fn: func(r *RunResult) error {
		for _, e := range r.ToolResults(name) {
			if ok, _ := e.Raw["ok"].(bool); ok {
				return nil
			}
		}
		return fmt.Errorf("no successful %s tool result in the stream", name)
	}}
}

// AnyToolOK passes when at least one of the named local tools succeeded.
func AnyToolOK(names ...string) Check {
	return Check{Name: "tool:any-ok " + strings.Join(names, "|"), Fn: func(r *RunResult) error {
		for _, n := range names {
			for _, e := range r.ToolResults(n) {
				if ok, _ := e.Raw["ok"].(bool); ok {
					return nil
				}
			}
		}
		return fmt.Errorf("none of %v produced a successful tool result", names)
	}}
}

// ToolNotCalled passes when the model never invoked the named local tool.
func ToolNotCalled(name string) Check {
	return Check{Name: "tool:not " + name, Fn: func(r *RunResult) error {
		if got := len(r.ToolCalls(name)); got > 0 {
			return fmt.Errorf("model called %s %d times, want 0", name, got)
		}
		return nil
	}}
}

// WorldCalledAny passes when the fake MCP served any of the named tools at
// least min times in total. Use when several routes are equally legitimate
// (e.g. a tail can arrive via getOutput OR getStatus includeOutput).
func WorldCalledAny(min int, tools ...string) Check {
	return Check{Name: fmt.Sprintf("world:any(%s)>=%d", strings.Join(tools, "|"), min), Fn: func(r *RunResult) error {
		got := 0
		for _, tool := range tools {
			got += r.World.CallCount(tool)
		}
		if got < min {
			return fmt.Errorf("world served %v %d times in total, want >= %d", tools, got, min)
		}
		return nil
	}}
}

// TerminalClosed passes when the named pre-seeded terminal ended the run closed.
func TerminalClosed(id string) Check {
	return Check{Name: "terminal:closed " + id, Fn: func(r *RunResult) error {
		t := r.World.Terminal(id)
		if t == nil {
			return fmt.Errorf("terminal %s does not exist in the world", id)
		}
		if !r.World.IsClosed(id) {
			return fmt.Errorf("terminal %s is still open", id)
		}
		return nil
	}}
}

// Under passes when the whole run finished within d.
func Under(d time.Duration) Check {
	return Check{Name: "time:under " + d.String(), Fn: func(r *RunResult) error {
		if r.Duration > d {
			return fmt.Errorf("run took %s, want under %s", r.Duration.Round(time.Second), d)
		}
		return nil
	}}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
