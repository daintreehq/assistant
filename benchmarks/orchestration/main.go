// Command orchestration runs the end-to-end orchestration benchmark: the REAL
// CLI binary + the LIVE local backend (127.0.0.1:8473) against a scripted fake
// Daintree world. It measures whether the assistant reaches each task's final
// result — and how long / how many rounds / how many tokens it took.
//
// Usage (from the repo root, backend running):
//
//	go run ./benchmarks/orchestration                 # full suite
//	go run ./benchmarks/orchestration -filter spawn   # scenarios whose id/category matches
//	go run ./benchmarks/orchestration -trials 3       # pass-rate over N trials
//	go run ./benchmarks/orchestration -list           # list scenarios, no runs
//	go run ./benchmarks/orchestration -parallel 4     # concurrent scenarios
//
// Every trial gets its own fake world, state dir, and debug log; the model
// turns are real (a few cents each) but every spawned "agent" is a script —
// zero tokens spent on the agents under supervision.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/daintreehq/daintree-assistant/benchmarks/orchestration/runner"
	"github.com/daintreehq/daintree-assistant/benchmarks/orchestration/scenario"
)

func main() {
	var (
		filter   = flag.String("filter", "", "substring filter on scenario id/category")
		trials   = flag.Int("trials", 1, "trials per scenario")
		parallel = flag.Int("parallel", 1, "concurrent scenario trials")
		backend  = flag.String("backend", "http://127.0.0.1:8473", "live backend URL")
		binPath  = flag.String("bin", "", "pre-built CLI binary (default: build from source)")
		list     = flag.Bool("list", false, "list scenarios and exit")
		outPath  = flag.String("out", "", "results JSON path (default benchmarks/orchestration/results/<ts>.json)")
	)
	flag.Parse()

	if *trials < 1 || *parallel < 1 {
		fmt.Fprintln(os.Stderr, "-trials and -parallel must be >= 1")
		os.Exit(2)
	}

	scenarios := filterScenarios(scenario.All(), *filter)
	if *list {
		for _, sc := range scenarios {
			fmt.Printf("%-28s [%s] timeout=%s\n    %s\n", sc.ID, sc.Category, sc.Timeout, sc.Notes)
		}
		fmt.Printf("\n%d scenarios\n", len(scenarios))
		return
	}
	if len(scenarios) == 0 {
		fmt.Fprintln(os.Stderr, "no scenarios match the filter")
		os.Exit(2)
	}

	if !backendUp(*backend) {
		fmt.Fprintf(os.Stderr, "backend not reachable at %s — start it first (cd ../assistant-backend && python -m daintree_assistant_server)\n", *backend)
		os.Exit(2)
	}

	repoRoot, err := findRepoRoot()
	if err != nil {
		fatal(err)
	}
	workDir, err := os.MkdirTemp("", "daintree-bench-*")
	if err != nil {
		fatal(err)
	}
	fmt.Fprintf(os.Stderr, "workdir: %s\n", workDir)

	bin := *binPath
	if bin == "" {
		fmt.Fprintln(os.Stderr, "building CLI binary…")
		bin, err = runner.BuildBinary(repoRoot, workDir)
		if err != nil {
			fatal(err)
		}
	}
	// The runner sets each trial's CWD to its scratch dir, so the binary path
	// must be absolute or it would resolve against the wrong directory.
	if bin, err = filepath.Abs(bin); err != nil {
		fatal(err)
	}

	opts := runner.Options{BackendURL: *backend, BinPath: bin, WorkDir: workDir}

	// Fan trials out over a bounded worker pool. Every trial is fully isolated
	// (own world, own state dir), so concurrency is safe; the backend and
	// DeepSeek handle parallel sessions.
	type job struct {
		sc    scenario.Scenario
		trial int
	}
	var jobs []job
	for _, sc := range scenarios {
		for t := 1; t <= *trials; t++ {
			jobs = append(jobs, job{sc: sc, trial: t})
		}
	}

	results := make([]runner.ScenarioResult, len(jobs))
	sem := make(chan struct{}, *parallel)
	var wg sync.WaitGroup
	ctx := context.Background()
	started := time.Now()
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			fmt.Fprintf(os.Stderr, "▶ %s (trial %d)…\n", j.sc.ID, j.trial)
			res := runner.RunScenario(ctx, bin, j.sc, opts, j.trial)
			verdict := "PASS"
			if !res.Passed {
				verdict = "FAIL"
			}
			fmt.Fprintf(os.Stderr, "  %s %s (trial %d) — %.0fs, %d rounds, %d tool calls\n",
				verdict, j.sc.ID, j.trial, float64(res.DurationMS)/1000, res.Rounds, res.ToolCalls)
			results[i] = res
		}(i, j)
	}
	wg.Wait()

	printSummary(results, time.Since(started))
	if p := saveResults(results, *outPath, repoRoot); p != "" {
		fmt.Printf("\nsaved: %s\n", p)
	}

	for _, r := range results {
		if !r.Passed {
			os.Exit(1)
		}
	}
}

func filterScenarios(all []scenario.Scenario, filter string) []scenario.Scenario {
	if filter == "" {
		return all
	}
	var out []scenario.Scenario
	for _, sc := range all {
		if strings.Contains(sc.ID, filter) || strings.Contains(sc.Category, filter) {
			out = append(out, sc)
		}
	}
	return out
}

func backendUp(url string) bool {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(strings.TrimRight(url, "/") + "/healthz")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 500
}

// findRepoRoot walks up from the CWD to the directory holding go.mod.
func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above CWD — run from inside the repo")
		}
		dir = parent
	}
}

func printSummary(results []runner.ScenarioResult, elapsed time.Duration) {
	fmt.Printf("\n%-28s %-9s %-6s %7s %7s %6s %9s %9s\n",
		"SCENARIO", "CATEGORY", "TRIAL", "PASS", "SECS", "ROUNDS", "TOOLCALLS", "TOKENS")
	passed := 0
	for _, r := range results {
		verdict := "FAIL"
		if r.Passed {
			verdict = "ok"
			passed++
		}
		fmt.Printf("%-28s %-9s %-6d %7s %7.0f %6d %9d %9d\n",
			r.ID, r.Category, r.Trial, verdict, float64(r.DurationMS)/1000, r.Rounds, r.ToolCalls,
			r.PromptTok+r.CompletionTk)
	}
	fmt.Printf("\n%d/%d trials passed in %s\n", passed, len(results), elapsed.Round(time.Second))

	// Failed-check detail: the part you actually read.
	for _, r := range results {
		if r.Passed {
			continue
		}
		fmt.Printf("\n--- FAIL %s (trial %d) status=%s timedOut=%v\n", r.ID, r.Trial, r.Status, r.TimedOut)
		for _, c := range r.Checks {
			if !c.Pass {
				fmt.Printf("    ✗ %s: %s\n", c.Name, c.Error)
			}
		}
		if r.Error != "" {
			fmt.Printf("    runner error: %s\n", r.Error)
		}
		if r.DebugLog != "" {
			fmt.Printf("    debug log: %s\n", r.DebugLog)
		}
	}
}

func saveResults(results []runner.ScenarioResult, outPath, repoRoot string) string {
	if outPath == "" {
		dir := filepath.Join(repoRoot, "benchmarks", "orchestration", "results")
		_ = os.MkdirAll(dir, 0o755)
		outPath = filepath.Join(dir, time.Now().Format("2006-01-02T15-04-05")+".json")
	}
	doc := map[string]any{
		"ranAt":   time.Now().Format(time.RFC3339),
		"results": results,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return ""
	}
	if err := os.WriteFile(outPath, b, 0o644); err != nil {
		return ""
	}
	return outPath
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "error:", err)
	os.Exit(2)
}
