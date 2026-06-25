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

// A finished agent whose batched recentOutput is BLANK (some agent TUIs bottom-pad their
// viewport — Codex parks its composer high and fills the rest of the screen with blank
// lines) must not strand the background watcher: resolvePresent falls back to the deep
// terminal.getOutput read for the real tail, exactly like the in-turn awaitAll path. A
// NON-blank inline tail still skips the deep read (the cheap fast path). Without the
// blank-tail guard the watcher would feed an empty tail to the finished judge forever.
func TestWatcher_BlankRecentOutputFallsBackToDeepRead(t *testing.T) {
	for _, tc := range []struct {
		name     string
		recent   string
		wantDeep bool
	}{
		{"blank inline falls back to deep read", "\r\n\r\n\r\n\r\n", true},
		{"content inline skips deep read", "the agent's real answer", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			queue := newFakeQueue()
			mcp := newFakeMCP()
			mcp.results["terminal.getStatus"] = statusResult(map[string]any{
				"terminalId": "t1", "agentState": "waiting", "waitingReason": "prompt",
				"recentOutput": tc.recent,
			})
			mcp.results["terminal.getOutput"] = MCPResult{StructuredContent: map[string]any{"content": "deep scrollback content"}}
			rec := termWatcher("wch_blank", []string{"t1"})
			store.watchers = []domain.WatcherRecord{rec}

			RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)

			calledDeep := false
			for _, c := range mcp.calls {
				if c == "terminal.getOutput" {
					calledDeep = true
				}
			}
			if calledDeep != tc.wantDeep {
				t.Fatalf("recentOutput=%q: deep terminal.getOutput called=%v, want %v (calls=%v)",
					tc.recent, calledDeep, tc.wantDeep, mcp.calls)
			}
		})
	}
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

// A working agent must NOT consult the model even when its tail looks alarming
// (intermediate "FAIL" output is not a terminal verdict). agentState=="working" is
// authoritative, so the engine returns still_working deterministically — the guard
// model would error if reached.
func TestWatcher_WorkingAgentBypassesModel(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working", "recentOutput": "FAIL: 3 tests failed",
	})
	rec := termWatcher("wch_m", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassStillWorking {
		t.Fatalf("a working agent must bypass the model → still_working, got %s", out.Classification)
	}
	if out.EpistemicKind != domain.EpistemicObserved {
		t.Errorf("a model-free still_working must be observed, got %s", out.EpistemicKind)
	}
	// still_working is not "meaningful" → nothing surfaces to the operator.
	if len(queue.published) != 0 {
		t.Errorf("a still-working agent must not publish, got %d events", len(queue.published))
	}
}

// The same bypass must hold on the list-fallback path: getStatus dropped the
// terminal but terminal.list still reports it working, so the engine deep-reads
// the tail — and must still skip the model for a working agent.
func TestWatcher_WorkingViaListFallbackBypassesModel(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult() // t1 absent from getStatus
	mcp.results["terminal.list"] = MCPResult{Text: `{"terminals":[{"id":"t1","agentState":"working"}]}`}
	mcp.results["terminal.getOutput"] = MCPResult{StructuredContent: map[string]any{"content": "building module 3/9…"}}
	rec := aged(termWatcher("wch_lf", []string{"t1"}))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassStillWorking {
		t.Fatalf("list-fallback working agent must bypass the model → still_working, got %s", out.Classification)
	}
	if out.EpistemicKind != domain.EpistemicObserved {
		t.Errorf("a model-free still_working must be observed, got %s", out.EpistemicKind)
	}
}

// The working bypass MUST be ordered before the lastClassifyKey dedup check:
// seed the dedup key to exactly what this tick computes, so without the ordering
// the dedup path would win and return no_change. The bypass must still return
// still_working.
func TestWatcher_WorkingBypassPrecedesDedup(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	tail := "still compiling…"
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working", "recentOutput": tail,
	})
	rec := termWatcher("wch_wk", []string{"t1"})
	key := "working||" + hashTail(tail) // agentState|exitCode(empty)|outHash
	opts, _ := json.Marshal(watcherOptions{PerTerminal: map[string]TerminalState{
		"t1": {OutHash: hashTail(tail), LastClassifyKey: key, Prev: "still_working", Seen: true},
	}})
	rec.OptionsJson = ptrStr(string(opts))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassStillWorking {
		t.Fatalf("the working bypass must precede the dedup check → still_working, got %s", out.Classification)
	}
}

// A working agent with empty output never reaches classifyTail (and so never the
// bypass) — it resolves to no_change before any model interaction.
func TestWatcher_WorkingWithEmptyTailIsNoChange(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "working", "recentOutput": "",
	})
	rec := termWatcher("wch_we", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{classErr: errModelMustNotRun}), rec)
	if out.Classification != domain.ClassNoChange {
		t.Fatalf("a working agent with empty output → no_change, got %s", out.Classification)
	}
}

// The bypass is the ONLY model-free still_working: a model that classifies a
// non-working tail as still_working stays inferred (usedModel=true).
func TestWatcher_ModelDerivedStillWorkingIsInferred(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "running", "recentOutput": "linking…",
	})
	model := &fakeModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.6, Summary: "working",
	}}
	rec := termWatcher("wch_mi", []string{"t1"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	if out.Classification != domain.ClassStillWorking {
		t.Fatalf("non-working tail model-classified still_working, got %s", out.Classification)
	}
	if out.EpistemicKind != domain.EpistemicInferred {
		t.Errorf("a model-derived still_working must be inferred, got %s", out.EpistemicKind)
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

	// `waiting` after working is only a SETTLE candidate now — the small model must
	// also confirm the agent actually finished. Wire a confident-YES judge so this
	// asserts the (deterministic gate + confirmed) completion.
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{judgeFn: finishedYesJudge}), rec)
	if out.Classification != domain.ClassCompletedSuccess {
		t.Fatalf("explore idle-at-prompt → completed_success, got %s", out.Classification)
	}
}

// The dual of the above: an explore agent idle at "waiting" after working, but the
// small model says it has NOT finished (the false-"waiting" of a backgrounded /
// paused agent). The watcher must NOT complete — it stays active and re-arms, with
// no completion attention published.
func TestWatcher_ExploreWaitingNotConfirmedStaysActive(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "waiting", "recentOutput": "⏺ still digging through TerminalInstanceService…",
	})
	rec := termWatcher("wch_nf", []string{"t1"})
	opts, _ := json.Marshal(watcherOptions{
		SpawnMode:   "explore",
		PerTerminal: map[string]TerminalState{"t1": {SeenWorking: true}}, // already worked → settle candidate
	})
	rec.OptionsJson = ptrStr(string(opts))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{judgeFn: finishedNoJudge}), rec)
	if out.Classification == domain.ClassCompletedSuccess || out.Stop {
		t.Fatalf("unconfirmed waiting must NOT complete; got %s stop=%v", out.Classification, out.Stop)
	}
	if store.watchPatches["wch_nf"]["status"] != "active" {
		t.Error("watcher must stay active when completion is not confirmed")
	}
	if hasAttentionFor(queue, "t1") {
		t.Error("no completion attention should be published when not confirmed finished")
	}
}

// The finished judge is throttled per terminal: after a not-finished verdict, a
// second tick inside the cooldown window must NOT re-consult the small model (this is
// what stops a repainting/churning tail from burning a judge call every 3s).
func TestWatcher_ExploreFinishJudgeCooldownSkipsModel(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "waiting", "recentOutput": "⏺ mid-task, paused…",
	})
	rec := termWatcher("wch_cd", []string{"t1"})
	opts, _ := json.Marshal(watcherOptions{
		SpawnMode:   "explore",
		PerTerminal: map[string]TerminalState{"t1": {SeenWorking: true}},
	})
	rec.OptionsJson = ptrStr(string(opts))
	store.watchers = []domain.WatcherRecord{rec}

	calls := 0
	judge := func(q, _ string) domain.ModelJudgeAnswer {
		if q == domain.FinishedJudgeQuestion {
			calls++
		}
		return domain.ModelJudgeAnswer{Matched: false, Confidence: 0.9, Reason: "still working"}
	}

	// Tick 1: judges (not finished) and stamps LastFinishJudgeAt.
	out1 := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{judgeFn: judge}), rec)
	if out1.Classification == domain.ClassCompletedSuccess {
		t.Fatalf("tick 1 should not complete, got %s", out1.Classification)
	}
	if calls != 1 {
		t.Fatalf("tick 1 should consult the finished judge once, got %d", calls)
	}

	// Tick 2: replay the persisted state; the two checks run milliseconds apart (well
	// inside finishJudgeCooldownMS), so the model must be skipped.
	rec.OptionsJson = ptrStr(store.watchPatches["wch_cd"]["optionsJson"].(string))
	out2 := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{judgeFn: judge}), rec)
	if out2.Classification == domain.ClassCompletedSuccess {
		t.Fatalf("tick 2 should not complete, got %s", out2.Classification)
	}
	if calls != 1 {
		t.Fatalf("tick 2 inside the cooldown must NOT re-judge; total judge calls=%d", calls)
	}
}

// --- linked workflow ledger advance (issue #206) ----------------------------

// decodeNotesJSON extracts and unmarshals the notesJson entry from a workflow
// patch. Fails the test when it is missing or not a JSON-encoded []string.
func decodeNotesJSON(t *testing.T, patch map[string]any) []string {
	t.Helper()
	raw, ok := patch["notesJson"]
	if !ok {
		t.Fatal("patch is missing notesJson")
	}
	s, ok := raw.(string)
	if !ok {
		t.Fatalf("notesJson must be a JSON string, got %T", raw)
	}
	var notes []string
	if err := json.Unmarshal([]byte(s), &notes); err != nil {
		t.Fatalf("notesJson is not a JSON []string: %v (%q)", err, s)
	}
	return notes
}

// notesHasPrefix reports whether any digest note line starts with prefix.
func notesHasPrefix(notes []string, prefix string) bool {
	for _, n := range notes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

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
	// The terminating outcome digest is persisted into notesJson in the SAME patch.
	notes := decodeNotesJSON(t, p)
	if !notesHasPrefix(notes, "classification: "+string(domain.ClassTerminalExited)) {
		t.Errorf("notesJson must carry the classification, got %v", notes)
	}
	if !notesHasPrefix(notes, "confidence: ") {
		t.Errorf("notesJson must carry the confidence, got %v", notes)
	}
	if !notesHasPrefix(notes, "summary: ") {
		t.Errorf("notesJson must carry the summary, got %v", notes)
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
	// A timed-out supervisor still has a headline digest to persist, and it must
	// lead with the timeout stop-reason (the headline class is usually "working").
	notes := decodeNotesJSON(t, store.workflowPatch["wfr_to"])
	if !notesHasPrefix(notes, "classification: ") || !notesHasPrefix(notes, "summary: ") {
		t.Errorf("timeout digest must carry classification + summary, got %v", notes)
	}
	if !notesHasPrefix(notes, "watcher: timed out") {
		t.Errorf("timeout digest must record the timeout stop-reason, got %v", notes)
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
	// The disable path has no real digest — it must NOT fabricate a note.
	if _, ok := store.workflowPatch["wfr_bad"]["notesJson"]; ok {
		t.Errorf("corrupt-disable must not write notesJson, got %v", store.workflowPatch["wfr_bad"]["notesJson"])
	}
}

// A lost finalize claim (the watcher was cancelled mid-check) must NOT advance the
// workflow row — mirrors the grant-revoke skip, so a cancelled run isn't clobbered.
func TestWatcher_LostClaimDoesNotAdvanceWorkflow(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(0),
	})
	rec := termWatcher("wch_lost", []string{"t1"})
	rec.WorkflowRunID = ptrStr("wfr_lost")
	// The store's copy was cancelled out from under the check, so ClaimDueWatcher fails.
	cancelled := rec
	cancelled.Status = "cancelled"
	store.watchers = []domain.WatcherRecord{cancelled}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if !out.Stop {
		t.Fatalf("exited still reports stop in the outcome")
	}
	if len(store.workflowPatch) != 0 {
		t.Fatalf("a lost claim must NOT advance the workflow row, got %v", store.workflowPatch)
	}
	if store.revoked["wch_lost"] != 0 {
		t.Fatalf("a lost claim must NOT revoke grants either, got %v", store.revoked)
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

// A terminating supervisor with a MemoryWriter wired mirrors its short outcome into an
// episodic memory: kind=episodic, source=watcher, runId=linked run, namespaced to the
// session — off the SAME finalize path, with no extra model call.
func TestWatcher_WritesEpisodicMemoryOnFinalize(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(0), "recentOutput": "done",
	})
	rec := termWatcher("wch_mem", []string{"t1"})
	rec.WorkflowRunID = ptrStr("wfr_mem")
	store.watchers = []domain.WatcherRecord{rec}

	ctx := ctxFor(store, queue, mcp, &fakeModel{})
	mw := &fakeMemoryWriter{}
	ctx.MemoryWriter = mw
	ctx.SessionID = "ses_live"

	out := RunTerminalWatcherCheck(ctx, rec)
	if !out.Stop {
		t.Fatalf("exited should stop, got stop=%v", out.Stop)
	}
	recs := mw.records()
	if len(recs) != 1 {
		t.Fatalf("finalize must write exactly one episodic memory, got %d: %+v", len(recs), recs)
	}
	m := recs[0]
	if m.Kind != domain.MemoryKindEpisodic {
		t.Errorf("kind = %q, want episodic", m.Kind)
	}
	if m.Source != domain.MemoryWatcher {
		t.Errorf("source = %q, want watcher", m.Source)
	}
	if m.RunID == nil || *m.RunID != "wfr_mem" {
		t.Errorf("runId = %v, want the linked run wfr_mem", m.RunID)
	}
	if m.SessionID == nil || *m.SessionID != "ses_live" {
		t.Errorf("sessionId = %v, want ses_live", m.SessionID)
	}
	if strings.TrimSpace(m.Content) == "" {
		t.Error("episodic memory must carry the outcome summary, got empty")
	}
}

// The corrupt-disable finalize path supplies no outcome digest — even with a
// MemoryWriter wired it must NOT fabricate an episodic memory (mirrors the
// no-fabricated-notesJson guarantee).
func TestWatcher_CorruptDisableWritesNoEpisodicMemory(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	rec := domain.WatcherRecord{
		ID: "wch_badmem", Kind: "terminal", Title: "Bad", Goal: "g",
		TargetsJson: `not json`, CadenceMs: 3000, ModelTier: domain.ModelSmall,
		Status: "active", WorkflowRunID: ptrStr("wfr_badmem"),
	}
	store.watchers = []domain.WatcherRecord{rec}

	ctx := ctxFor(store, queue, newFakeMCP(), &fakeModel{})
	mw := &fakeMemoryWriter{}
	ctx.MemoryWriter = mw
	ctx.SessionID = "ses_live"

	_ = RunTerminalWatcherCheck(ctx, rec)
	if recs := mw.records(); len(recs) != 0 {
		t.Fatalf("corrupt-disable must not write an episodic memory, got %+v", recs)
	}
}

// A finalize with no MemoryWriter wired (the default) must not panic and writes no
// memory — the episodic mirror is strictly optional.
func TestWatcher_NilMemoryWriterSkipsEpisodicWrite(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newFakeMCP()
	mcp.results["terminal.getStatus"] = statusResult(map[string]any{
		"terminalId": "t1", "agentState": "exited", "exitCode": float64(0), "recentOutput": "done",
	})
	rec := termWatcher("wch_nomw", []string{"t1"})
	rec.WorkflowRunID = ptrStr("wfr_nomw")
	store.watchers = []domain.WatcherRecord{rec}

	// ctxFor leaves MemoryWriter nil — the finalize must still advance the ledger.
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &fakeModel{}), rec)
	if !out.Stop {
		t.Fatalf("exited should stop")
	}
	if store.workflowPatch["wfr_nomw"] == nil {
		t.Fatal("ledger must still advance even without a MemoryWriter")
	}
}

// watcherDigestNote serializes a real outcome and returns nil when there is
// nothing worth recording (no fabricated empty array).
func TestWatcherDigestNote(t *testing.T) {
	if got := watcherDigestNote(nil, "condition_met"); got != nil {
		t.Errorf("nil outcome → nil note, got %q", *got)
	}
	// An empty outcome (no classification, blank summary, no evidence) records nothing.
	if got := watcherDigestNote(&CheckOutcome{Summary: "  "}, "condition_met"); got != nil {
		t.Errorf("empty outcome → nil note, got %q", *got)
	}
	// A full outcome serializes classification, confidence, summary, and each
	// non-blank evidence line as a JSON []string.
	note := watcherDigestNote(&CheckOutcome{
		Classification: domain.ClassTestsPassed,
		Confidence:     0.8,
		Summary:        "Tests passed on a clean tree.",
		Evidence:       []string{"exitCode=0", "  ", "git status clean"},
	}, "condition_met")
	if note == nil {
		t.Fatal("a full outcome must produce a note")
	}
	var notes []string
	if err := json.Unmarshal([]byte(*note), &notes); err != nil {
		t.Fatalf("note must be a JSON []string: %v", err)
	}
	want := []string{
		"classification: tests_passed",
		"confidence: 80%",
		"summary: Tests passed on a clean tree.",
		"evidence: exitCode=0",
		"evidence: git status clean",
	}
	if strings.Join(notes, "\n") != strings.Join(want, "\n") {
		t.Errorf("digest mismatch:\n got %v\nwant %v", notes, want)
	}

	// Zero confidence still renders cleanly as "0%" (not a blank or NaN).
	zero := watcherDigestNote(&CheckOutcome{Classification: domain.ClassUnknown}, "condition_met")
	if zero == nil || !strings.Contains(*zero, `"confidence: 0%"`) {
		t.Errorf("zero-confidence digest must render \"0%%\", got %v", zero)
	}

	// A timeout leads with the stop-reason so a failed row explains itself.
	to := watcherDigestNote(&CheckOutcome{
		Classification: domain.ClassStillWorking,
		Confidence:     0.6,
		Summary:        "Agent is still working.",
	}, "timeout")
	if to == nil {
		t.Fatal("timeout outcome must produce a note")
	}
	var toNotes []string
	if err := json.Unmarshal([]byte(*to), &toNotes); err != nil {
		t.Fatalf("timeout note must be a JSON []string: %v", err)
	}
	if len(toNotes) == 0 || !strings.HasPrefix(toNotes[0], "watcher: timed out") {
		t.Errorf("timeout note must lead with the stop-reason, got %v", toNotes)
	}
}
