package daemon

import (
	"encoding/json"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Ports watcherMultiTerminal.test.ts: batching, recentOutput-vs-getOutput
// fallback, deep getOutput on contains, terminal.list cross-checks, explore-mode
// waitingReason routing, mixed-batch classification, and supervisor promotion.

func TestWatcher_BatchesGetStatusAndThreadsWaitingReason(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "waiting", waitingReason: "question", recentOutput: strptr("")},
		"term-b": {agentState: "working", recentOutput: strptr("compiling")},
	})
	rec := watcherWith("wch_b", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}), rec)

	// Exactly ONE status call covering BOTH terminals.
	status := mcp.callsFor("terminal.getStatus")
	if len(status) != 1 {
		t.Fatalf("expected one batched getStatus, got %d", len(status))
	}
	ids := stringIDs(status[0].args["terminalIds"])
	if len(ids) != 2 {
		t.Errorf("status call should batch both terminals, got %v", ids)
	}
	// It piggybacks a bounded inline tail (includeOutput).
	if _, ok := status[0].args["includeOutput"]; !ok {
		t.Error("batched status must request includeOutput")
	}
	// waitingReason=question reaches the published event's evidence.
	found := false
	for _, p := range queue.published {
		if p.Target != nil && p.Target.TerminalID == "term-a" {
			for _, e := range p.Evidence {
				if containsStr(e, "question") {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("waitingReason=question must surface in the term-a event evidence")
	}
}

func TestWatcher_UsesInlineRecentOutputSkipsGetOutput(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "working", recentOutput: strptr("building A...")},
		"term-b": {agentState: "working", recentOutput: strptr("compiling B...")},
	})
	rec := watcherWith("wch_i", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}), rec)

	if got := len(mcp.callsFor("terminal.getOutput")); got != 0 {
		t.Errorf("inline recentOutput should skip getOutput entirely, got %d calls", got)
	}
	if got := len(mcp.callsFor("terminal.getStatus")); got != 1 {
		t.Errorf("one batched status call, got %d", got)
	}
}

func TestWatcher_FallsBackToGetOutputWhenRecentAbsent(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "working", tail: "deep scrollback A"},
		"term-b": {agentState: "working", tail: "deep scrollback B"},
	})
	rec := watcherWith("wch_f", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}), rec)

	// One getOutput per terminal since the inline tail was not provided.
	if got := len(mcp.callsFor("terminal.getOutput")); got != 2 {
		t.Errorf("absent recentOutput should fall back to one getOutput per terminal, got %d", got)
	}
}

func TestWatcher_DeepGetOutputWhenContainsCondition(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {
			agentState:   "working",
			recentOutput: strptr("...recent clean progress lines..."),
			tail:         "earlier output\nFAILED: build broke\nmore lines",
		},
	})
	rec := watcherWith("wch_dc", []string{"term-a"}, withAlert(`{"contains":"FAILED"}`))
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "working",
	}}), rec)

	// The contains condition forces a deep read despite recentOutput present.
	if got := len(mcp.callsFor("terminal.getOutput")); got != 1 {
		t.Errorf("a contains condition must force a deep getOutput, got %d", got)
	}
	// The marker in deep output produced an attention-level alert for term-a.
	if !hasAttentionFor(queue, "term-a") {
		t.Error("the FAILED marker in deep output should publish an attention alert")
	}
}

func TestWatcher_StopsOnlyWhenEveryTerminalTerminal(t *testing.T) {
	t.Run("both completed → stop", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{
			"term-a": {agentState: "completed"}, "term-b": {agentState: "completed"},
		})
		rec := watcherWith("wch_s1", []string{"term-a", "term-b"})
		store.watchers = []domain.WatcherRecord{rec}
		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
		if store.watchPatches["wch_s1"]["status"] != "condition_met" {
			t.Errorf("both terminal → condition_met, got %v", store.watchPatches["wch_s1"]["status"])
		}
	})
	t.Run("one still waiting → stays active", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{
			"term-a": {agentState: "completed"}, "term-b": {agentState: "waiting"},
		})
		rec := watcherWith("wch_s2", []string{"term-a", "term-b"})
		store.watchers = []domain.WatcherRecord{rec}
		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
		if store.watchPatches["wch_s2"]["status"] != "active" {
			t.Errorf("a still-waiting terminal must keep the watcher active, got %v", store.watchPatches["wch_s2"]["status"])
		}
	})
}

func TestWatcher_PreviouslySeenClosedAlerts(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// getStatus empty, list empty → the previously-seen terminal is gone.
	mcp := newProgMCP(map[string]termCfg{})
	mcp.list = []map[string]any{}
	rec := watcherWith("wch_gone", []string{"term-x"},
		withOptions(watcherOptions{PerTerminal: map[string]TerminalState{"term-x": {Seen: true}}}))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassTerminalExited || !out.Stop {
		t.Fatalf("a previously-seen, now-absent terminal must be terminal_exited+stop, got %s stop=%v", out.Classification, out.Stop)
	}
	if !hasEventFor(queue, "term-x") {
		t.Error("the closed terminal must surface an event")
	}
}

func TestWatcher_NeverSeenAbsentPastGraceExits(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{})
	mcp.list = []map[string]any{}
	rec := aged(watcherWith("wch_late", []string{"term-x"}))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassTerminalExited || !out.Stop {
		t.Fatalf("a never-seen terminal absent past the grace = failed launch → exited, got %s", out.Classification)
	}
}

func TestWatcher_ListAliveStaysAlive(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{})
	mcp.list = []map[string]any{{"id": "term-x", "agentState": "waiting", "waitingReason": "question"}}
	rec := aged(watcherWith("wch_alive", []string{"term-x"}))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassWaitingForInput || out.Stop {
		t.Fatalf("getStatus-absent but list-alive → waiting_for_input, alive; got %s stop=%v", out.Classification, out.Stop)
	}
	if store.watchPatches["wch_alive"]["status"] != "active" {
		t.Error("a list-alive terminal must keep the watcher active")
	}
}

func TestWatcher_ExploreWaitingReasonRouting(t *testing.T) {
	cases := []struct {
		name          string
		spawnMode     string
		waitingReason string
		wantClass     domain.WatcherClassification
		wantAttention bool
	}{
		{"explore prompt → completion", "explore", "prompt", domain.ClassCompletedSuccess, false},
		{"explore absent reason → completion", "explore", "", domain.ClassCompletedSuccess, false},
		{"explore question → waiting", "explore", "question", domain.ClassWaitingForInput, true},
		{"edit prompt → waiting (no explore leak)", "edit", "prompt", domain.ClassWaitingForInput, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			queue := newFakeQueue()
			mcp := newProgMCP(map[string]termCfg{
				"term-x": {agentState: "waiting", waitingReason: tc.waitingReason, recentOutput: strptr("$ ")},
			})
			rec := watcherWith("wch_e", []string{"term-x"}, withOptions(watcherOptions{SpawnMode: tc.spawnMode}))
			store.watchers = []domain.WatcherRecord{rec}

			out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
			if out.Classification != tc.wantClass {
				t.Fatalf("got %s, want %s", out.Classification, tc.wantClass)
			}
			if got := hasAttentionFor(queue, "term-x"); got != tc.wantAttention {
				t.Errorf("attention=%v, want %v", got, tc.wantAttention)
			}
		})
	}
}

func TestWatcher_ExploreCompletionTerminalEvenWhenDirty(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-x": {agentState: "waiting", waitingReason: "prompt", recentOutput: strptr("$ ")},
	})
	// A pre-existing dirty worktree an explore agent can never clean — completion
	// must still be terminal (not completed_unverified looping forever).
	mcp.pulse = &MCPResult{StructuredContent: map[string]any{"isDirty": true, "changedFiles": float64(3)}}
	rec := watcherWith("wch_ed", []string{"term-x"},
		withOptions(watcherOptions{SpawnMode: "explore", VerificationScope: &verificationScope{WorktreeID: "wt-x"}}))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassCompletedSuccess {
		t.Fatalf("explore idle is completion regardless of git state, got %s", out.Classification)
	}
}

func TestWatcher_ListedOmittedWorkingModelClassified(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// getStatus omits term-x; list reports it plain working; getOutput has scrollback.
	mcp := newProgMCP(map[string]termCfg{"term-x": {tail: "npm ERR! command failed\n"}})
	mcp.list = []map[string]any{{"id": "term-x", "agentState": "working"}}
	rec := aged(watcherWith("wch_lo", []string{"term-x"}))
	store.watchers = []domain.WatcherRecord{rec}

	model := &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassCommandFailed, Confidence: 0.9, Summary: "A command failed.",
		Evidence: []string{"npm ERR!"},
	}}
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	if out.Classification != domain.ClassCommandFailed {
		t.Fatalf("listed-but-omitted working must read scrollback and model-classify, got %s", out.Classification)
	}
	if got := len(mcp.callsFor("terminal.getOutput")); got != 1 {
		t.Errorf("scrollback should be read once, got %d getOutput calls", got)
	}
}

func TestWatcher_ListedOmittedWorkingEmptyOutputNoModel(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// list reports working but getOutput is empty → must attempt read but NOT model.
	mcp := newProgMCP(map[string]termCfg{"term-x": {tail: ""}})
	mcp.list = []map[string]any{{"id": "term-x", "agentState": "working"}}
	rec := aged(watcherWith("wch_le", []string{"term-x"}))
	store.watchers = []domain.WatcherRecord{rec}

	// classErr would surface if the model were consulted; the empty-tail guard holds.
	model := &progModel{classErr: errModelMustNotRun}
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	if out.Classification != domain.ClassNoChange {
		t.Fatalf("empty scrollback must degrade to no_change, got %s", out.Classification)
	}
	if got := len(mcp.callsFor("terminal.getOutput")); got != 1 {
		t.Errorf("the read must still be attempted once, got %d", got)
	}
}

func TestWatcher_ListedOmittedModelCompletionGatedByGit(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-x": {tail: "All done.\n"}})
	mcp.list = []map[string]any{{"id": "term-x", "agentState": "working"}}
	mcp.pulse = &MCPResult{StructuredContent: map[string]any{"isDirty": true, "changedFiles": float64(2)}}
	rec := aged(watcherWith("wch_lg", []string{"term-x"}))
	store.watchers = []domain.WatcherRecord{rec}

	model := &progModel{verdict: domain.WatcherVerdict{
		Classification: domain.ClassCompletedSuccess, Confidence: 0.9, Summary: "Agent reports done.",
	}}
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	// A model-claimed completion on a dirty tree must demote to unverified.
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("model completion on the list path must still pass the git gate, got %s", out.Classification)
	}
}

func TestWatcher_ExitedOnlyWhenListAlsoExited(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{})
	mcp.list = []map[string]any{{"id": "term-x", "agentState": "exited", "exitCode": float64(1)}}
	rec := watcherWith("wch_ex", []string{"term-x"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassTerminalExited || !out.Stop {
		t.Fatalf("getStatus-absent + list-exited → terminal_exited+stop, got %s stop=%v", out.Classification, out.Stop)
	}
}

func TestWatcher_ListUnreadableStaysAlive(t *testing.T) {
	t.Run("errored list", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{})
		mcp.listResult = &MCPResult{Text: "boom", IsError: true}
		rec := aged(watcherWith("wch_ur", []string{"term-x"},
			withOptions(watcherOptions{PerTerminal: map[string]TerminalState{"term-x": {Seen: true}}})))
		store.watchers = []domain.WatcherRecord{rec}
		out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
		if out.Classification == domain.ClassTerminalExited || out.Stop {
			t.Fatalf("an unreadable list cannot prove an exit, got %s stop=%v", out.Classification, out.Stop)
		}
		if store.watchPatches["wch_ur"]["status"] != "active" {
			t.Error("stays active")
		}
	})
	t.Run("non-errored but unparseable list", func(t *testing.T) {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{})
		mcp.listResult = &MCPResult{Text: "not json", StructuredContent: map[string]any{"stuff": float64(1)}}
		rec := aged(watcherWith("wch_up", []string{"term-x"},
			withOptions(watcherOptions{PerTerminal: map[string]TerminalState{"term-x": {Seen: true}}})))
		store.watchers = []domain.WatcherRecord{rec}
		out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
		if out.Classification == domain.ClassTerminalExited {
			t.Fatalf("an unparseable list is unreadable, not 'all gone', got %s", out.Classification)
		}
		if store.watchPatches["wch_up"]["status"] != "active" {
			t.Error("stays active")
		}
	})
}

func TestWatcher_MixedBatchOneStatusOneList(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// term-a present in getStatus; term-b omitted but alive in list.
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "waiting", recentOutput: strptr("")}})
	mcp.list = []map[string]any{
		{"id": "term-a", "agentState": "waiting"},
		{"id": "term-b", "agentState": "waiting"},
	}
	rec := watcherWith("wch_mix", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification == domain.ClassTerminalExited {
		t.Fatalf("neither terminal exited, got %s", out.Classification)
	}
	if store.watchPatches["wch_mix"]["status"] != "active" {
		t.Error("watcher stays active")
	}
	// terminal.list consulted once for the whole batch, not per missing target.
	if got := len(mcp.callsFor("terminal.list")); got != 1 {
		t.Errorf("list should be consulted once per tick, got %d", got)
	}
}

func TestWatcher_SupervisorCleanCompletionPromotedToAttention(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-x": {agentState: "completed"}})
	rec := watcherWith("wch_sup", []string{"term-x"}, withSupervisor())
	store.watchers = []domain.WatcherRecord{rec}

	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	// "done" sits below the surfacing threshold; the supervisor bump promotes it.
	if !hasAttentionFor(queue, "term-x") {
		t.Error("a supervisor's clean completion must promote to >= attention so it surfaces")
	}
}

func TestWatcher_StatusCallFailureNotTreatedAsGone(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{})
	// getStatus itself errors → ok:false → absence must not read as "closed".
	mcp.listResult = nil
	mcp.calls = nil
	// Override getStatus to error via a model-less progMCP: set the status to error
	// by leaving perTerminal empty AND making the call return IsError. We model that
	// by routing getStatus through listResult-style: simplest is a dedicated fake.
	failing := &statusErrMCP{}
	rec := watcherWith("wch_tf", []string{"term-x"})
	store.watchers = []domain.WatcherRecord{rec}
	out := RunTerminalWatcherCheck(ctxFor(store, queue, failing, &progModel{}), rec)
	if out.Classification == domain.ClassTerminalExited {
		t.Fatalf("a failed status call must not be read as exited, got %s", out.Classification)
	}
	if store.watchPatches["wch_tf"]["status"] != "active" {
		t.Error("stays active")
	}
}

func TestWatcher_PerTerminalStatePersisted(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{
		"term-a": {agentState: "waiting", recentOutput: strptr("")},
		"term-b": {agentState: "waiting", recentOutput: strptr("")},
	})
	rec := watcherWith("wch_pp", []string{"term-a", "term-b"})
	store.watchers = []domain.WatcherRecord{rec}
	RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)

	raw, _ := store.watchPatches["wch_pp"]["optionsJson"].(string)
	var opts watcherOptions
	if err := json.Unmarshal([]byte(raw), &opts); err != nil {
		t.Fatalf("optionsJson should round-trip: %v", err)
	}
	if _, ok := opts.PerTerminal["term-a"]; !ok {
		t.Error("term-a state not persisted")
	}
	if _, ok := opts.PerTerminal["term-b"]; !ok {
		t.Error("term-b state not persisted")
	}
}

// --- small assertion helpers --------------------------------------------------

func containsStr(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func hasEventFor(q *fakeQueue, terminalID string) bool {
	for _, p := range q.published {
		if p.Target != nil && p.Target.TerminalID == terminalID {
			return true
		}
	}
	return false
}

func hasAttentionFor(q *fakeQueue, terminalID string) bool {
	for _, p := range q.published {
		if p.Target != nil && p.Target.TerminalID == terminalID &&
			domain.RankOf(p.Severity) >= domain.RankOf(domain.SeverityAttention) {
			return true
		}
	}
	return false
}
