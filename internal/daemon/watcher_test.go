package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func termWatcher(id string, targets []string) domain.WatcherRecord {
	tj, _ := json.Marshal(targets)
	return domain.WatcherRecord{
		ID: id, Kind: "terminal", Title: "Sup", Goal: "do the task",
		TargetsJson: string(tj), CadenceMs: int(SchedulerTickMS), ModelTier: domain.ModelSmall,
		Status: "active", NextCheckAt: 0, CreatedAt: 0,
	}
}

// statusResult builds an MCPResult whose text body carries a terminals array (the
// shape Daintree actually populates).
func statusResult(entries ...map[string]any) MCPResult {
	body, _ := json.Marshal(map[string]any{"terminals": entries})
	return MCPResult{Text: string(body)}
}

func TestWatcher_WaitingForInputPublishesAttention(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "waiting", "waitingReason": "question",
		"recentOutput": "Proceed? (y/n)",
	})
	rec := termWatcher("wch_w", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassWaitingForInput {
		t.Fatalf("waiting/question → waiting_for_input, got %s", out.Classification)
	}
	if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityAttention {
		t.Fatalf("waiting_for_input should publish attention")
	}
	pub := queue.published[0]
	// Recommended actions: focus FIRST (look-before-you-leap), then a reply path.
	ra := pub.RecommendedActions
	if len(ra) != 2 || ra[0].ToolName != "terminal.focus" || ra[1].ToolName != "terminal.sendCommand" {
		t.Fatalf("waiting_for_input should recommend terminal.focus then terminal.sendCommand, got %+v", ra)
	}
	if !ra[1].RequiresConfirmation || ra[1].Risk != domain.RiskTerminal {
		t.Errorf("reply action must be RiskTerminal + RequiresConfirmation, got %+v", ra[1])
	}
	if args, ok := ra[1].Args.(map[string]any); !ok || args["terminalId"] != "t1" {
		t.Errorf("reply action args must carry terminalId=t1, got %+v", ra[1].Args)
	}
	// The actual question text is folded into the summary and evidence.
	if !strings.Contains(pub.Summary, "Proceed? (y/n)") {
		t.Errorf("question summary should include the tail snippet, got %q", pub.Summary)
	}
	foundQ := false
	for _, e := range pub.Evidence {
		if strings.Contains(e, "question:") && strings.Contains(e, "Proceed? (y/n)") {
			foundQ = true
		}
	}
	if !foundQ {
		t.Errorf("question evidence should include the tail snippet, got %v", pub.Evidence)
	}
}

func TestWatcher_ExitedIsUrgentAndStops(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(1), "recentOutput": "boom",
	})
	rec := termWatcher("wch_e", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassTerminalExited || !out.Stop {
		t.Fatalf("exited should be terminal_exited+stop, got %s stop=%v", out.Classification, out.Stop)
	}
	if store.revoked["wch_e"] != 1 {
		t.Error("a stopped watcher must revoke its grants")
	}
	if store.watchPatches["wch_e"]["status"] != "condition_met" {
		t.Errorf("terminal stop should set status condition_met, got %v", store.watchPatches["wch_e"]["status"])
	}
}

func TestWatcher_SpawnGraceNoFalseExit(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	// getStatus succeeds but returns NO terminals; list also empty.
	mcp.results["terminal.getStatus"] = statusResult()
	mcp.results["terminal.list"] = MCPResult{Text: `{"terminals":[]}`}
	rec := termWatcher("wch_g", []string{"t1"})
	rec.CreatedAt = domain.NowMS() // just created → inside grace
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification == domain.ClassTerminalExited {
		t.Fatal("a never-seen terminal inside the spawn grace must NOT be declared exited")
	}
	if out.Classification != domain.ClassNoChange {
		t.Errorf("expected no_change during grace, got %s", out.Classification)
	}
}

func TestWatcher_GetStatusOmitsButListAliveNoExit(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	// getStatus omits t1, but terminal.list reports it waiting.
	mcp.results["terminal.getStatus"] = statusResult()
	mcp.results["terminal.list"] = MCPResult{Text: `{"terminals":[{"id":"t1","agentState":"waiting","waitingReason":"question"}]}`}
	rec := termWatcher("wch_l", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification == domain.ClassTerminalExited {
		t.Fatal("a terminal alive in terminal.list must never be declared exited")
	}
	if out.Classification != domain.ClassWaitingForInput {
		t.Errorf("list-alive waiting → waiting_for_input, got %s", out.Classification)
	}
}

func TestWatcher_CompletedGatesOnGitClean(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "completed", "recentOutput": "all done",
	})
	// Clean tree → completed_success.
	mcp.results["git.getProjectPulse"] = MCPResult{StructuredContent: map[string]any{"clean": true}}
	rec := termWatcher("wch_c", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassCompletedSuccess || !out.Stop {
		t.Fatalf("completed + clean → completed_success stop, got %s stop=%v", out.Classification, out.Stop)
	}
	// Evidence carries the verification bundle.
	found := false
	for _, e := range out.Evidence {
		if len(e) > len(domain.VerificationEvidencePrefix) && e[:len(domain.VerificationEvidencePrefix)] == domain.VerificationEvidencePrefix {
			found = true
		}
	}
	if !found {
		t.Errorf("gated completion must attach a verification: evidence bundle, got %v", out.Evidence)
	}
}

func TestWatcher_CompletedDirtyStaysUnverifiedAlive(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "completed", "recentOutput": "done editing",
	})
	mcp.results["git.getProjectPulse"] = MCPResult{StructuredContent: map[string]any{"changedFiles": float64(2)}}
	rec := termWatcher("wch_d", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("completed + dirty → completed_unverified, got %s", out.Classification)
	}
	if out.Stop {
		t.Error("completed_unverified must keep the watcher alive (not stop)")
	}
}

func TestWatcher_RateLimitedBacksOff(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working",
		"recentOutput": "Error 429: too many requests, retry-after 30s",
	})
	rec := termWatcher("wch_r", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassRateLimited {
		t.Fatalf("rate-limit signature → rate_limited (model-free), got %s", out.Classification)
	}
	// nextCheckAt backs off to at least the cooldown.
	next := store.watchPatches["wch_r"]["nextCheckAt"].(int64)
	// cadence is the 3s tick; cooldown is 60s, so the back-off must exceed cadence.
	if next-domain.NowMS() < rateLimitCooldownMS-1000 {
		t.Errorf("rate_limited must back off to ~cooldown, nextCheckAt delta too small")
	}
}

func TestWatcher_ModelClassifiesWorkingTail(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working", "recentOutput": "FAIL: 3 tests failed",
	})
	model := &fakeModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassTestsFailed, Confidence: 0.9, Summary: "3 tests failed",
		Evidence: []string{"FAIL"}, RecommendedAction: domain.ActionNone,
	}}
	rec := termWatcher("wch_m", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	if out.Classification != domain.ClassTestsFailed {
		t.Fatalf("model verdict should drive classification, got %s", out.Classification)
	}
	if out.EpistemicKind != domain.EpistemicInferred {
		t.Errorf("a model-derived verdict should be inferred, got %s", out.EpistemicKind)
	}
}

func TestWatcher_CorruptTargetsDisabled(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	rec := domain.WatcherRecord{
		ID: "wch_bad", Kind: "terminal", Title: "Bad", Goal: "g",
		TargetsJson: `not json`, CadenceMs: 3000, ModelTier: domain.ModelSmall,
		Status: "active",
	}
	store.watchers = []domain.WatcherRecord{rec}
	out := RunTerminalWatcherCheck(ctxFor(store, queue, newFakeMCP(), &fakeModel{}), rec)
	if !out.Stop || out.StopReason != StopTerminal {
		t.Error("corrupt watcher should stop")
	}
	if store.watchPatches["wch_bad"]["status"] != "error" {
		t.Error("corrupt watcher should be set to error")
	}
	if len(queue.published) != 1 || queue.published[0].Severity != domain.SeverityError {
		t.Error("corrupt watcher should publish a visible error event")
	}
}

func TestWatcher_ExploreIdleIsCompletion(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "waiting", "recentOutput": "$ ",
	})
	rec := termWatcher("wch_x", []string{"t1"})
	opts, _ := json.Marshal(watcherOptions{SpawnMode: "explore"})
	rec.OptionsJson = ptrStr(string(opts))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if out.Classification != domain.ClassCompletedSuccess {
		t.Fatalf("explore idle-at-prompt → completed_success, got %s", out.Classification)
	}
}

// --- linked workflow ledger advance (issue #206) ----------------------------

// A supervisor that reaches condition_met advances its linked workflow row to
// done and stamps completedAt.
func TestWatcher_AdvancesLinkedWorkflowOnConditionMet(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(0), "recentOutput": "done",
	})
	rec := termWatcher("wch_wf", []string{"t1"})
	rec.WorkflowRunID = ptrStr("wfr_done")
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if !out.Stop {
		t.Fatalf("exited should stop, got stop=%v", out.Stop)
	}
	p := store.workflowPatch["wfr_done"]
	if p == nil {
		t.Fatal("a stopped supervisor must advance its linked workflow row")
	}
	if p["status"] != string(domain.WorkflowDone) {
		t.Errorf("condition_met → done, got %v", p["status"])
	}
	if _, ok := p["completedAt"].(int64); !ok {
		t.Errorf("advance must stamp completedAt, got %v", p["completedAt"])
	}
}

// A supervisor that times out fails its linked workflow row.
func TestWatcher_TimeoutFailsLinkedWorkflow(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working", "recentOutput": "still going",
	})
	rec := termWatcher("wch_to", []string{"t1"})
	rec.WorkflowRunID = ptrStr("wfr_to")
	rec.StopAfterMs = ptrInt64(1) // created at 0; now >> 1 → timed out
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if !out.Stop || out.StopReason != StopTimeout {
		t.Fatalf("expected timeout stop, got stop=%v reason=%v", out.Stop, out.StopReason)
	}
	if store.workflowPatch["wfr_to"]["status"] != string(domain.WorkflowFailed) {
		t.Errorf("timeout → failed, got %v", store.workflowPatch["wfr_to"]["status"])
	}
}

// A corrupt-state supervisor fails its linked workflow row.
func TestWatcher_CorruptDisablesFailsLinkedWorkflow(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	rec := domain.WatcherRecord{
		ID: "wch_badwf", Kind: "terminal", Title: "Bad", Goal: "g",
		TargetsJson: `not json`, CadenceMs: 3000, ModelTier: domain.ModelSmall,
		Status: "active", WorkflowRunID: ptrStr("wfr_bad"),
	}
	store.watchers = []domain.WatcherRecord{rec}

	_ = RunTerminalWatcherCheck(ctxFor(store, queue, newFakeMCP(), &fakeModel{}), rec)
	if store.workflowPatch["wfr_bad"]["status"] != string(domain.WorkflowFailed) {
		t.Errorf("corrupt watcher → linked workflow failed, got %v", store.workflowPatch["wfr_bad"]["status"])
	}
}

// A watcher with no workflow link never touches a ledger row on stop.
func TestWatcher_NoWorkflowLinkNoAdvance(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(0),
	})
	rec := termWatcher("wch_nolink", []string{"t1"}) // WorkflowRunID nil
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if !out.Stop {
		t.Fatalf("exited should stop")
	}
	if len(store.workflowPatch) != 0 {
		t.Fatalf("a watcher with no workflow link must not touch a ledger row, got %v", store.workflowPatch)
	}
}
