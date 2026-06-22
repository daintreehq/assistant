package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Covers runVerificationPass paths and the acceptance-contract gate inside
// runTerminalWatcherCheck.

func pulseMCP(sc map[string]any, text string, isErr bool, connected bool) *progMCP {
	m := newProgMCP(map[string]termCfg{})
	m.connected = connected
	m.pulse = &MCPResult{StructuredContent: sc, Text: text, IsError: isErr}
	return m
}

func TestRunVerificationPass_Paths(t *testing.T) {
	mkCtx := func(m *progMCP) *CheckContext { return ctxFor(newFakeStore(), newFakeQueue(), m, &progModel{}) }

	t.Run("clean pulse → verified", func(t *testing.T) {
		v := runVerificationPass(mkCtx(pulseMCP(map[string]any{"isDirty": false, "changedFiles": float64(0)}, "", false, true)), nil)
		if v.Verdict != domain.VerdictVerified {
			t.Errorf("clean → verified, got %s", v.Verdict)
		}
	})
	t.Run("dirty pulse → unknown with git changes", func(t *testing.T) {
		v := runVerificationPass(mkCtx(pulseMCP(map[string]any{"isDirty": true, "changedFiles": float64(2)}, "", false, true)), nil)
		if v.Verdict != domain.VerdictUnknown || !v.HasGitChanges || v.ChangedFiles != 2 {
			t.Errorf("dirty → unknown+2 changes, got %+v", v)
		}
	})
	t.Run("pulse error → unknown", func(t *testing.T) {
		v := runVerificationPass(mkCtx(pulseMCP(nil, "", true, true)), nil)
		if v.Verdict != domain.VerdictUnknown {
			t.Errorf("errored pulse → unknown, got %s", v.Verdict)
		}
	})
	t.Run("disconnected → unknown (never throws)", func(t *testing.T) {
		v := runVerificationPass(mkCtx(pulseMCP(nil, "", false, false)), nil)
		if v.Verdict != domain.VerdictUnknown {
			t.Errorf("disconnected → unknown, got %s", v.Verdict)
		}
	})
	t.Run("text markers when structuredContent absent", func(t *testing.T) {
		v := runVerificationPass(mkCtx(pulseMCP(nil, "working tree clean", false, true)), nil)
		if v.Verdict != domain.VerdictVerified {
			t.Errorf("clean text → verified, got %s", v.Verdict)
		}
	})
}

// --- post-completion git gate ------------------------------------------------

func TestWatcher_CompletedCleanIsSuccessWithVerifiedEvidence(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed"}})
	rec := watcherWith("wch_vc", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassCompletedSuccess || out.Severity != domain.SeverityDone {
		t.Fatalf("clean completion → completed_success@done, got %s/%s", out.Classification, out.Severity)
	}
	if v := verdictFromEvidence(t, queue, "term-a"); v != domain.VerdictVerified {
		t.Errorf("clean completion evidence must be verified, got %s", v)
	}
}

func TestWatcher_ModelClaimedCompletionRoutedThroughGate(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// FSM working, model classifies completed_success, but worktree is dirty.
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "working", recentOutput: strptr("All done! Task complete.")}})
	mcp.pulse = &MCPResult{StructuredContent: map[string]any{"isDirty": true, "changedFiles": float64(2)}}
	rec := watcherWith("wch_mc", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}
	model := &progModel{verdict: domain.WatcherVerdict{Classification: domain.ClassCompletedSuccess, Confidence: 0.7, Summary: "looks done"}}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, model), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("a model-claimed completion on a dirty tree must demote, got %s", out.Classification)
	}
	if store.watchPatches["wch_mc"]["status"] != "active" {
		t.Error("completed_unverified keeps polling")
	}
}

func TestWatcher_CompletedUnverifiableGitErrorStaysUnverified(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed"}})
	mcp.pulse = &MCPResult{IsError: true}
	rec := watcherWith("wch_ge", []string{"term-a"})
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, &progModel{}), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("an unverifiable git state must be treated as unverified, got %s", out.Classification)
	}
}

// --- acceptance-contract gate ------------------------------------------------

const acceptanceCriteria = "All tests pass and the bug is fixed."

func contractModel(matched bool, conf float64, reason string) *progModel {
	return &progModel{
		verdict: domain.WatcherVerdict{Classification: domain.ClassStillWorking, Confidence: 0.7, Summary: "still working"},
		judgeFn: func(question, _ string) domain.ModelJudgeAnswer {
			// Only the acceptance contract question is asked here.
			if strings.Contains(question, "acceptance contract") {
				return domain.ModelJudgeAnswer{Matched: matched, Confidence: conf, Reason: reason}
			}
			return domain.ModelJudgeAnswer{}
		},
	}
}

func contractWatcher(id string) func(*domain.WatcherRecord) {
	return withOptions(watcherOptions{AcceptanceCriteria: acceptanceCriteria})
}

func TestContractGate_CleanConfidentMatchSuccess(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: "All tests pass. Done."}})
	rec := watcherWith("wch_cm", []string{"term-a"}, contractWatcher("wch_cm"))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(true, 0.92, "Tests pass; bug fixed.")), rec)
	if out.Classification != domain.ClassCompletedSuccess {
		t.Fatalf("clean + confident match → completed_success, got %s", out.Classification)
	}
	if v := verdictFromEvidence(t, queue, "term-a"); v != domain.VerdictVerified {
		t.Errorf("evidence verdict = %s, want verified", v)
	}
}

func TestContractGate_ConfidentNonMatchFailedEvenDirty(t *testing.T) {
	cases := []struct {
		name  string
		dirty bool
	}{{"clean tree", false}, {"dirty tree", true}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newFakeStore()
			queue := newFakeQueue()
			mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: "Ran the suite. Gave up."}})
			if tc.dirty {
				mcp.pulse = &MCPResult{StructuredContent: map[string]any{"isDirty": true, "changedFiles": float64(2)}}
			}
			rec := watcherWith("wch_nm", []string{"term-a"}, contractWatcher("wch_nm"))
			store.watchers = []domain.WatcherRecord{rec}

			out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(false, 0.9, "Two tests still failing.")), rec)
			if out.Classification != domain.ClassCompletedUnverified {
				t.Fatalf("confident non-match → completed_unverified, got %s", out.Classification)
			}
			// A confident non-match is FAILED regardless of git state.
			if v := verdictFromEvidence(t, queue, "term-a"); v != domain.VerdictFailed {
				t.Errorf("confident non-match verdict = %s, want failed", v)
			}
		})
	}
}

func TestContractGate_LowConfidenceUnknownNeverUpgraded(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: "Finished, I think."}})
	rec := watcherWith("wch_lc", []string{"term-a"}, contractWatcher("wch_lc"))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(true, 0.3, "Not sure.")), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("low-confidence judge → completed_unverified, got %s", out.Classification)
	}
	if v := verdictFromEvidence(t, queue, "term-a"); v != domain.VerdictUnknown {
		t.Errorf("low-confidence verdict = %s, want unknown", v)
	}
}

func TestContractGate_MetButDirtyUnknown(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: "Done with the changes."}})
	mcp.pulse = &MCPResult{StructuredContent: map[string]any{"isDirty": true, "changedFiles": float64(3)}}
	rec := watcherWith("wch_md", []string{"term-a"}, contractWatcher("wch_md"))
	store.watchers = []domain.WatcherRecord{rec}

	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(true, 0.95, "Criteria satisfied.")), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("contract met but dirty → completed_unverified, got %s", out.Classification)
	}
	v := verResultFromEvidence(t, queue, "term-a")
	if v.Verdict != domain.VerdictUnknown || !v.HasGitChanges {
		t.Errorf("met-but-dirty → unknown+hasGitChanges, got %+v", v)
	}
}

func TestContractGate_EmptyTailNeverFalseFailed(t *testing.T) {
	store := newFakeStore()
	queue := newFakeQueue()
	// completed with no readable scrollback → judge must NOT run → unknown.
	mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: ""}})
	rec := watcherWith("wch_et", []string{"term-a"}, contractWatcher("wch_et"))
	store.watchers = []domain.WatcherRecord{rec}

	// Judge would say "not met" if asked — but it must not be asked.
	out := RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(false, 0.95, "Would fail if asked.")), rec)
	if out.Classification != domain.ClassCompletedUnverified {
		t.Fatalf("empty tail → completed_unverified, got %s", out.Classification)
	}
	v := verResultFromEvidence(t, queue, "term-a")
	if v.Verdict != domain.VerdictUnknown {
		t.Errorf("empty tail → unknown, got %s", v.Verdict)
	}
	if v.CriteriaMetSummary != "" {
		t.Error("the judge never ran, so no criteriaMetSummary should be recorded")
	}
}

func TestContractGate_ConfidenceFloorBoundary(t *testing.T) {
	cases := []struct {
		conf float64
		want domain.VerificationVerdict
	}{{0.6, domain.VerdictVerified}, {0.599, domain.VerdictUnknown}}
	for _, tc := range cases {
		store := newFakeStore()
		queue := newFakeQueue()
		mcp := newProgMCP(map[string]termCfg{"term-a": {agentState: "completed", tail: "All done."}})
		rec := watcherWith("wch_cf", []string{"term-a"}, contractWatcher("wch_cf"))
		store.watchers = []domain.WatcherRecord{rec}

		RunTerminalWatcherCheck(ctxFor(store, queue, mcp, contractModel(true, tc.conf, "Met.")), rec)
		if v := verdictFromEvidence(t, queue, "term-a"); v != tc.want {
			t.Errorf("confidence %v → %s, want %s", tc.conf, v, tc.want)
		}
	}
}

// --- evidence helpers --------------------------------------------------------

func verResultFromEvidence(t *testing.T, q *fakeQueue, terminalID string) domain.VerificationResult {
	t.Helper()
	for _, p := range q.published {
		if p.Target == nil || p.Target.TerminalID != terminalID {
			continue
		}
		for _, e := range p.Evidence {
			if strings.HasPrefix(e, domain.VerificationEvidencePrefix) {
				var v domain.VerificationResult
				if err := json.Unmarshal([]byte(e[len(domain.VerificationEvidencePrefix):]), &v); err != nil {
					t.Fatalf("verification evidence did not parse: %v", err)
				}
				return v
			}
		}
	}
	t.Fatalf("no verification evidence published for %s", terminalID)
	return domain.VerificationResult{}
}

func verdictFromEvidence(t *testing.T, q *fakeQueue, terminalID string) domain.VerificationVerdict {
	t.Helper()
	return verResultFromEvidence(t, q, terminalID).Verdict
}
