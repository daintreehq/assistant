package daemon

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Covers the runTerminalWatcherCheck modelJudge end-to-end behavior and the
// watcher rate-limit behavior.

const judgeQ = "Did the migration finish?"

func judgeModel(matched bool, conf float64, reason string) *progModel {
	return &progModel{
		verdict: domain.WatcherVerdict{Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "still working"},
		judgeFn: func(_, _ string) domain.ModelJudgeAnswer {
			return domain.ModelJudgeAnswer{Matched: matched, Confidence: conf, Reason: reason}
		},
	}
}

func TestWatcher_AlertJudgePublishesOnYesSilentOnNo(t *testing.T) {
	t.Run("YES fires the alert with judge evidence", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
		rec := watcherWith("wch_jy", []string{"term-a"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
		store.watchers = []domain.WatcherRecord{rec}

		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, judgeModel(true, 0.9, "migration done")), rec)
		if !hasAttentionFor(queue, "term-a") {
			t.Fatal("a confident YES judge must publish an attention alert")
		}
		// The matched judge reason is surfaced as evidence (judge[Q]: ...).
		found := false
		for _, p := range queue.published {
			for _, e := range p.Evidence {
				if containsStr(e, "judge["+judgeQ+"]") {
					found = true
				}
			}
		}
		if !found {
			t.Error("the matched judge reason must be attached as evidence")
		}
	})
	t.Run("NO stays silent", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
		rec := watcherWith("wch_jn", []string{"term-a"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
		store.watchers = []domain.WatcherRecord{rec}

		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, judgeModel(false, 0.9, "still running")), rec)
		if hasAttentionFor(queue, "term-a") {
			t.Error("a NO judge must not publish an attention alert")
		}
	})
}

func TestWatcher_JudgeBelowConfidenceFloorNoFire(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
	rec := watcherWith("wch_jlc", []string{"term-a"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
	store.watchers = []domain.WatcherRecord{rec}

	// matched=true but confidence 0.4 < 0.6 floor → must not fire.
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, judgeModel(true, 0.4, "maybe done")), rec)
	if hasAttentionFor(queue, "term-a") {
		t.Error("a matched judge below the confidence floor must not fire")
	}
}

func TestWatcher_StopJudgeStopsOnYesActiveOnNo(t *testing.T) {
	t.Run("YES → condition_met", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
		rec := watcherWith("wch_jsy", []string{"term-a"}, withStop(`{"modelJudge":"`+judgeQ+`"}`))
		store.watchers = []domain.WatcherRecord{rec}

		out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, judgeModel(true, 0.9, "done")), rec)
		if out.StopReason != StopConditionMet {
			t.Fatalf("a YES stop judge must stop with condition_met, got %s", out.StopReason)
		}
		if store.watchPatches["wch_jsy"]["status"] != "condition_met" {
			t.Error("watcher status should be condition_met")
		}
	})
	t.Run("NO → active", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
		rec := watcherWith("wch_jsn", []string{"term-a"}, withStop(`{"modelJudge":"`+judgeQ+`"}`))
		store.watchers = []domain.WatcherRecord{rec}

		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, judgeModel(false, 0.9, "not yet")), rec)
		if store.watchPatches["wch_jsn"]["status"] != "active" {
			t.Error("a NO stop judge must keep the watcher active")
		}
	})
}

func TestWatcher_JudgePerTerminalAgainstOwnTail(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "working", recentOutput: strptr("migration DONE")},
		"term-b": {agentState: "working", recentOutput: strptr("still working")},
	})
	rec := watcherWith("wch_jpt", []string{"term-a", "term-b"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
	store.watchers = []domain.WatcherRecord{rec}

	// The judge says YES only when THIS terminal's tail contains "DONE"; a shared
	// answer would wrongly fire term-b too.
	model := &progModel{
		verdict: domain.WatcherVerdict{Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working"},
		judgeFn: func(_, tail string) domain.ModelJudgeAnswer {
			if containsStr(tail, "DONE") {
				return domain.ModelJudgeAnswer{Matched: true, Confidence: 0.9, Reason: "tail says DONE"}
			}
			return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "not done"}
		},
	}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)

	// Only term-a should have produced an attention alert.
	if !hasAttentionFor(queue, "term-a") {
		t.Error("term-a (tail DONE) must alert")
	}
	if hasAttentionFor(queue, "term-b") {
		t.Error("term-b (tail not DONE) must NOT alert — judge is per-terminal")
	}
}

// --- tier pinning ------------------------------------------------------------

// Watcher classification must ALWAYS run on the small tier, even when the watcher
// record carries ModelTier=medium (which routes to the large model on the main
// thread). A non-working agentState with fresh output is the path that still
// reaches the classifier.
func TestWatcher_ClassifierAlwaysUsesSmallTier(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "running", "recentOutput": "linking…",
	})
	rec := termWatcher("wch_ct", []string{"t1"})
	rec.ModelTier = domain.ModelMedium
	store.watchers = []domain.WatcherRecord{rec}

	model := &tierRecorder{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.6, Summary: "working",
	}}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	tier, called := model.classifyTier()
	if !called {
		t.Fatal("the classifier was expected to run for a non-working agentState with output")
	}
	if tier != domain.ModelSmall {
		t.Errorf("classifier must pin the small tier even when rec.ModelTier=medium, got %s", tier)
	}
}

// Judges must ALSO pin the small tier regardless of rec.ModelTier.
func TestWatcher_JudgeAlwaysUsesSmallTier(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("migrating…")}})
	rec := watcherWith("wch_jt", []string{"term-a"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
	rec.ModelTier = domain.ModelMedium
	store.watchers = []domain.WatcherRecord{rec}

	model := &tierRecorder{judgeFn: func(_, _ string) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "still"}
	}}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	tier, called := model.judgeTier()
	if !called {
		t.Fatal("the judge was expected to run for the alertWhen modelJudge condition")
	}
	if tier != domain.ModelSmall {
		t.Errorf("judge must pin the small tier even when rec.ModelTier=medium, got %s", tier)
	}
}

// The two issue concerns are independent: a working agent must skip the
// CLASSIFIER (concern 1) while alertWhen/stopWhen JUDGES still run (a judge is a
// user-declared condition, not a tier-burning auto-classification).
func TestWatcher_WorkingSkipsClassifierButRunsJudge(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("Running migration...")}})
	rec := watcherWith("wch_wj", []string{"term-a"}, withAlert(`{"modelJudge":"`+judgeQ+`"}`))
	store.watchers = []domain.WatcherRecord{rec}

	model := &tierRecorder{judgeFn: func(_, _ string) domain.ModelJudgeAnswer {
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "still"}
	}}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)

	if _, cCalled := model.classifyTier(); cCalled {
		t.Error("a working agent must NOT consult the classifier")
	}
	if _, jCalled := model.judgeTier(); !jCalled {
		t.Error("alertWhen modelJudge must still consult the judge for a working agent")
	}
}

// --- rate limit --------------------------------------------------------------

func TestWatcher_RateLimitPublishesOnceAndBacksOff(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "working", recentOutput: strptr("…working…\nAPI Error: Rate limit reached. Please retry-after 30s.")},
	})
	rec := watcherWith("wch_rl", []string{"term-a"}, withSupervisor())
	rec.CadenceMs = 10_000 // shorter than the 60s cooldown — proves the back-off
	store.watchers = []domain.WatcherRecord{rec}

	before := domain.NowMS()
	// Router must NOT be consulted (deterministic classification) — a panicking
	// model would surface if reached.
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassRateLimited || out.Severity != domain.SeverityAttention {
		t.Fatalf("throttled tail → rate_limited@attention (model-free), got %s/%s", out.Classification, out.Severity)
	}
	// Exactly one attention event.
	att := 0
	for _, p := range queue.published {
		if domain.RankOf(p.Severity) >= domain.RankOf(domain.SeverityAttention) {
			att++
		}
	}
	if att != 1 {
		t.Errorf("rate_limited should publish exactly one attention event, got %d", att)
	}
	// Backs off to at least the cooldown (not the 10s cadence). Status stays active.
	next := store.watchPatches["wch_rl"]["nextCheckAt"].(int64)
	if next < before+rateLimitCooldownMS-1000 {
		t.Errorf("rate_limited must back off to >= cooldown, got delta %d", next-before)
	}
	if store.watchPatches["wch_rl"]["status"] != "active" {
		t.Error("rate_limited must NOT stop the watcher")
	}
}

func TestWatcher_RateLimitViaTextBodyShape(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// progMCP already delivers status via the text body (#108 real shape).
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "working", recentOutput: strptr("Error: 429 Too Many Requests — quota exceeded")},
	})
	rec := watcherWith("wch_rt", []string{"term-a"}, withSupervisor())
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassRateLimited {
		t.Fatalf("429 in a text-body tail must classify rate_limited, got %s", out.Classification)
	}
}

func TestWatcher_RateLimitDoesNotFlagUnrelatedOutput(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// Empty output → engine resolves without a model (no_change) and never flags.
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("")}})
	rec := watcherWith("wch_ru", []string{"term-a"}, withSupervisor())
	rec.CadenceMs = 10_000
	store.watchers = []domain.WatcherRecord{rec}

	before := domain.NowMS()
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification == domain.ClassRateLimited {
		t.Error("unrelated output must not classify rate_limited")
	}
	// Normal cadence — no rate-limit back-off.
	next := store.watchPatches["wch_ru"]["nextCheckAt"].(int64)
	if next >= before+rateLimitCooldownMS {
		t.Error("an unthrottled terminal must not back off to the cooldown")
	}
}
