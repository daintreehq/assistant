package daemon

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

func cond(t *testing.T, raw string) domain.WatchCondition {
	t.Helper()
	c, err := parseCondition(ptrStr(raw))
	if err != nil {
		t.Fatalf("parseCondition(%s): %v", raw, err)
	}
	return *c
}

func TestEvaluateCondition_Leaves(t *testing.T) {
	sig := WatcherSignals{AgentState: "working", RuntimeStatus: "running", Tail: "tests passed: 42"}

	if !EvaluateCondition(cond(t, `{"stateIs":"working"}`), sig, nil) {
		t.Error("stateIs working should match")
	}
	if EvaluateCondition(cond(t, `{"stateIs":"exited"}`), sig, nil) {
		t.Error("stateIs exited should not match")
	}
	if !EvaluateCondition(cond(t, `{"contains":"passed"}`), sig, nil) {
		t.Error("contains passed should match")
	}
	if !EvaluateCondition(cond(t, `{"regex":"tests (passed|failed)"}`), sig, nil) {
		t.Error("regex should match")
	}
	if !EvaluateCondition(cond(t, `{"runtimeStatusIs":"running"}`), sig, nil) {
		t.Error("runtimeStatusIs running should match")
	}
}

func TestEvaluateCondition_NoOutputForMs_UndefinedNeverTrips(t *testing.T) {
	c := cond(t, `{"noOutputForMs":1000}`)
	// nil msSinceOutput → never trips ("not observed" is not "silence").
	if EvaluateCondition(c, WatcherSignals{}, nil) {
		t.Error("nil msSinceOutput must not trip noOutputForMs")
	}
	if !EvaluateCondition(c, WatcherSignals{MsSinceOutput: ptrInt64(1500)}, nil) {
		t.Error("1500ms >= 1000ms should trip")
	}
	if EvaluateCondition(c, WatcherSignals{MsSinceOutput: ptrInt64(500)}, nil) {
		t.Error("500ms < 1000ms should not trip")
	}
}

func TestEvaluateCondition_Composite(t *testing.T) {
	sig := WatcherSignals{AgentState: "waiting", Tail: "y/n?"}
	all := cond(t, `{"all":[{"stateIs":"waiting"},{"contains":"y/n"}]}`)
	if !EvaluateCondition(all, sig, nil) {
		t.Error("all should match")
	}
	any := cond(t, `{"any":[{"stateIs":"exited"},{"contains":"y/n"}]}`)
	if !EvaluateCondition(any, sig, nil) {
		t.Error("any should match")
	}
	not := cond(t, `{"not":{"stateIs":"exited"}}`)
	if !EvaluateCondition(not, sig, nil) {
		t.Error("not exited should match a waiting terminal")
	}
}

func TestEvaluateCondition_ModelJudge(t *testing.T) {
	c := cond(t, `{"modelJudge":"did tests pass?"}`)
	judges := map[string]domain.ModelJudgeAnswer{
		"did tests pass?": {Matched: true, Confidence: 0.8},
	}
	if !EvaluateCondition(c, WatcherSignals{}, judges) {
		t.Error("confident matched judge should fire")
	}
	// Below floor → no fire.
	judges["did tests pass?"] = domain.ModelJudgeAnswer{Matched: true, Confidence: 0.5}
	if EvaluateCondition(c, WatcherSignals{}, judges) {
		t.Error("below-floor confidence must not fire")
	}
	// Missing answer → false; not:{missing} flips to true (documented wart).
	notC := cond(t, `{"not":{"modelJudge":"unknown q"}}`)
	if !EvaluateCondition(notC, WatcherSignals{}, map[string]domain.ModelJudgeAnswer{}) {
		t.Error("not of a missing judge answer must be true")
	}
}

func TestCollectModelJudges_DedupeFirstSeen(t *testing.T) {
	alert := cond(t, `{"any":[{"modelJudge":"q1"},{"modelJudge":"q2"}]}`)
	stop := cond(t, `{"all":[{"modelJudge":"q2"},{"modelJudge":"q3"}]}`)
	got := CollectModelJudges(&alert, &stop)
	want := []string{"q1", "q2", "q3"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestHasTextCondition(t *testing.T) {
	if HasTextCondition(nil) {
		t.Error("nil → false")
	}
	c := cond(t, `{"all":[{"stateIs":"working"},{"contains":"x"}]}`)
	if !HasTextCondition(&c) {
		t.Error("nested contains → true")
	}
	c2 := cond(t, `{"all":[{"stateIs":"working"},{"noOutputForMs":5}]}`)
	if HasTextCondition(&c2) {
		t.Error("no contains/regex → false")
	}
}

func TestHashTail_Stable(t *testing.T) {
	a := hashTail("hello world")
	b := hashTail("hello world")
	if a != b {
		t.Errorf("hashTail not deterministic: %s vs %s", a, b)
	}
	if hashTail("hello world") == hashTail("hello worle") {
		t.Error("different inputs should (almost always) differ")
	}
	if hashTail("") == "" {
		// empty string hashes to "0".
		if hashTail("") != "0" {
			t.Errorf("empty hash = %q, want 0", hashTail(""))
		}
	}
}

// TestHashTail_NonASCIIUTF16Parity locks the UTF-16-code-unit parity with the TS
// charCodeAt hash. A non-BMP rune (🚀) is two surrogate-pair code units there;
// ranging the Go string by rune would feed ONE code point > 0xFFFF and diverge,
// re-classifying the watcher every tick. The expected values are the TS algorithm
// over UTF-16 units (0xD83D,0xDE80 for 🚀): h=(h<<5)-h+unit, >>>0, base36.
func TestHashTail_NonASCIIUTF16Parity(t *testing.T) {
	if got := hashTail("🚀"); got != "1202r" {
		t.Fatalf("hashTail(🚀) = %q, want 1202r (UTF-16 surrogate-pair parity)", got)
	}
	if got := hashTail("café 🚀"); got != "92kr42" {
		t.Fatalf("hashTail(café 🚀) = %q, want 92kr42", got)
	}
	// ASCII is unaffected (single code unit per char either way).
	if hashTail("abc") == "" {
		t.Fatal("ascii hash should be non-empty")
	}
}

// TestDecideOutcome_InvalidDirectConditionFailsClosed asserts a directly-built
// (not JSON-decoded, so never validated) degenerate WatchCondition is treated as
// "does not match" at the eval entry rather than mis-evaluating or panicking.
func TestDecideOutcome_InvalidDirectConditionFailsClosed(t *testing.T) {
	cases := map[string]domain.WatchCondition{
		"empty (no variant key)": {},
		"two keys present":       {Contains: ptrStr("x"), Regex: ptrStr("y")},
		"blank contains":         {Contains: ptrStr("   ")},
		"bad regex":              {Regex: ptrStr("(")},
		"non-positive timeout":   {NoOutputForMs: ptrInt64(0)},
	}
	for name, cond := range cases {
		t.Run(name, func(t *testing.T) {
			c := cond
			// Must not panic; an invalid condition must not "match" → no stop/alert
			// publish from the condition. A real classification still drives publish.
			out := DecideOutcome(DecideArgs{
				Classification: domain.ClassNoChange,
				Confidence:     0.4,
				Previous:       string(domain.ClassNoChange),
				AlertWhen:      &c,
				StopWhen:       &c,
				Signals:        WatcherSignals{AgentState: "working", Tail: "anything at all"},
			})
			if out.Stop {
				t.Errorf("invalid condition must not trigger stop, got stop=true")
			}
			// no-change + invalid conditions ⇒ nothing meaningful to surface.
			if out.ShouldPublish {
				t.Errorf("invalid condition must not force a publish")
			}
		})
	}
}

func TestNextOutputState_StaleVsChanged(t *testing.T) {
	// First observation: outAt = now, msSinceOutput = 0.
	st, ms := NextOutputState(nil, "line1", 1000)
	if ms != 0 {
		t.Errorf("first observation msSinceOutput = %d, want 0", ms)
	}
	prev := st
	// Unchanged tail at a later time: outAt preserved, ms grows.
	st2, ms2 := NextOutputState(&prev, "line1", 4000)
	if ms2 != 3000 {
		t.Errorf("unchanged tail msSinceOutput = %d, want 3000", ms2)
	}
	if st2.OutAt != 1000 {
		t.Errorf("outAt should be preserved at 1000, got %d", st2.OutAt)
	}
	// Changed tail: outAt resets to now, ms = 0.
	st3, ms3 := NextOutputState(&prev, "line2", 5000)
	if ms3 != 0 {
		t.Errorf("changed tail msSinceOutput = %d, want 0", ms3)
	}
	if st3.OutAt != 5000 {
		t.Errorf("outAt should reset to 5000, got %d", st3.OutAt)
	}
}

func TestDecideOutcome_MeaningfulChangePublishes(t *testing.T) {
	out := DecideOutcome(DecideArgs{
		Classification: domain.ClassWaitingForInput,
		Previous:       "still_working",
		Signals:        WatcherSignals{},
	})
	if !out.ShouldPublish {
		t.Error("a meaningful change should publish")
	}
	if out.Severity != domain.SeverityAttention {
		t.Errorf("severity = %s, want attention", out.Severity)
	}
}

func TestDecideOutcome_NoChangeSuppressed(t *testing.T) {
	out := DecideOutcome(DecideArgs{
		Classification: domain.ClassNoChange,
		Previous:       "no_change",
		Signals:        WatcherSignals{},
	})
	if out.ShouldPublish {
		t.Error("no_change with same previous should not publish")
	}
	if out.Stop {
		t.Error("no_change should not stop")
	}
}

func TestDecideOutcome_TerminalStops(t *testing.T) {
	out := DecideOutcome(DecideArgs{
		Classification: domain.ClassCompletedSuccess,
		Previous:       "still_working",
		Signals:        WatcherSignals{},
	})
	if !out.Stop || out.StopReason != StopTerminal {
		t.Errorf("completed_success should stop terminally, got stop=%v reason=%s", out.Stop, out.StopReason)
	}
	// completed_unverified must NOT stop (kept alive).
	out2 := DecideOutcome(DecideArgs{
		Classification: domain.ClassCompletedUnverified,
		Previous:       "still_working",
		Signals:        WatcherSignals{},
	})
	if out2.Stop {
		t.Error("completed_unverified must not stop the watcher")
	}
}

func TestDecideOutcome_StopConditionPromotesSeverity(t *testing.T) {
	stop := cond(t, `{"stateIs":"completed"}`)
	out := DecideOutcome(DecideArgs{
		Classification: domain.ClassTestsPassed, // base severity "done"
		Previous:       "still_working",
		Signals:        WatcherSignals{AgentState: "completed"},
		StopWhen:       &stop,
	})
	if out.Severity != domain.SeverityAttention {
		t.Errorf("a matched stop condition must promote 'done' to attention, got %s", out.Severity)
	}
	if out.StopReason != StopConditionMet {
		t.Errorf("stopReason = %s, want condition_met", out.StopReason)
	}
}

func TestDecideOutcome_TimeoutForcesAttention(t *testing.T) {
	out := DecideOutcome(DecideArgs{
		Classification: domain.ClassStillWorking, // debug
		Previous:       "still_working",
		Signals:        WatcherSignals{},
		TimedOut:       true,
	})
	if !out.ShouldPublish || out.Severity != domain.SeverityAttention {
		t.Errorf("timeout should publish at attention, got publish=%v sev=%s", out.ShouldPublish, out.Severity)
	}
	if out.StopReason != StopTimeout {
		t.Errorf("stopReason = %s, want timeout", out.StopReason)
	}
}

func TestDeriveVerification_Clean(t *testing.T) {
	v := DeriveVerification(map[string]any{"clean": true}, "")
	if v.Verdict != domain.VerdictVerified || v.HasGitChanges {
		t.Errorf("clean flag → verified+no-changes, got %+v", v)
	}
}

func TestDeriveVerification_DirtyWins(t *testing.T) {
	// Self-contradictory: clean flag but a positive count → dirty wins.
	v := DeriveVerification(map[string]any{"isDirty": false, "changedFiles": float64(3)}, "")
	if v.Verdict != domain.VerdictUnknown || !v.HasGitChanges || v.ChangedFiles != 3 {
		t.Errorf("dirty count must override clean flag, got %+v", v)
	}
}

func TestDeriveVerification_TextMarkers(t *testing.T) {
	dirty := DeriveVerification(map[string]any{}, "Changes not staged for commit:\n\tmodified: x.go")
	if !dirty.HasGitChanges {
		t.Error("dirty text markers → hasGitChanges")
	}
	clean := DeriveVerification(map[string]any{}, "nothing to commit, working tree clean")
	if clean.HasGitChanges || clean.Verdict != domain.VerdictVerified {
		t.Errorf("clean text → verified, got %+v", clean)
	}
	// Never returns "failed".
	if dirty.Verdict == domain.VerdictFailed {
		t.Error("deriveVerification must never return failed")
	}
}

func TestDeriveVerification_Unknown(t *testing.T) {
	v := DeriveVerification(map[string]any{}, "")
	if v.Verdict != domain.VerdictUnknown {
		t.Errorf("undeterminable → unknown, got %s", v.Verdict)
	}
}

func TestExtractPrFields_Normalization(t *testing.T) {
	// GitHub-style: state open + merged flag.
	merged := MCPResult{StructuredContent: map[string]any{"state": "open", "merged": true, "title": "X"}}
	f, ok := extractPrFields(merged)
	if !ok || f.state != "merged" {
		t.Errorf("merged flag should normalize to merged, got %+v ok=%v", f, ok)
	}
	// GitLab-style: work_in_progress draft.
	draft := MCPResult{StructuredContent: map[string]any{"state": "opened", "work_in_progress": true}}
	f2, ok2 := extractPrFields(draft)
	if !ok2 || f2.state != "open" || f2.isDraft == nil || !*f2.isDraft {
		t.Errorf("opened+wip should be open+draft, got %+v ok=%v", f2, ok2)
	}
	// Envelope: {state:"ok", pr:{state:"closed"}} → unwrap to closed.
	env := MCPResult{StructuredContent: map[string]any{"state": "ok", "pr": map[string]any{"state": "closed"}}}
	f3, ok3 := extractPrFields(env)
	if !ok3 || f3.state != "closed" {
		t.Errorf("envelope should unwrap to closed PR, got %+v ok=%v", f3, ok3)
	}
	// Unrecognizable → not a PR.
	if _, ok4 := extractPrFields(MCPResult{StructuredContent: map[string]any{"foo": "bar"}}); ok4 {
		t.Error("non-PR payload should return ok=false")
	}
}

func TestAdvanced(t *testing.T) {
	if !advanced("2026-01-01T00:00:00Z", "2026-01-02T00:00:00Z") {
		t.Error("later timestamp should advance")
	}
	if advanced("2026-01-02T00:00:00Z", "2026-01-01T00:00:00Z") {
		t.Error("earlier timestamp should not advance")
	}
	if advanced("", "2026-01-01T00:00:00Z") {
		t.Error("missing prev should not advance")
	}
}
