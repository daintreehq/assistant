package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/domain"
)

// dueMessageJSON extracts the delivered JSON array from a timer-message wake prompt.
func dueMessageJSON(t *testing.T, prompt string) []map[string]any {
	t.Helper()
	i := strings.Index(prompt, "\n[")
	if i < 0 {
		t.Fatalf("no JSON array in prompt:\n%s", prompt)
	}
	var out []map[string]any
	if err := json.Unmarshal([]byte(prompt[i+1:]), &out); err != nil {
		t.Fatalf("delivered JSON does not parse: %v\n%s", err, prompt[i+1:])
	}
	return out
}

// A check-in tick must be able to tell where its loop stands — cadence, bounds, runs
// left, the ledger row it drives, and whether this is the LAST tick — so it can end or
// pace the loop honestly instead of promising a check that is not scheduled.
func TestTimerMessageWakePromptCarriesTheLoopPosition(t *testing.T) {
	until := int64(1_790_021_600_000)
	e := timerMessageEvent("evt_1", "tmr_loop", "check the queue; start the next job when one has a PR")
	e.Target.TimerOccurrence = 28
	e.Target.TimerEveryMs = 600_000
	e.Target.TimerMaxRuns = 30
	e.Target.TimerRepeatUntil = until
	e.Target.WorkflowRunID = "wfr_queue"

	got := dueMessageJSON(t, BuildWakePrompt([]domain.QueueEvent{e}, nil))
	if len(got) != 1 {
		t.Fatalf("want one due message, got %v", got)
	}
	m := got[0]
	want := map[string]any{
		"timerId": "tmr_loop", "occurrence": float64(28), "everyMs": float64(600_000),
		"maxRuns": float64(30), "remainingRuns": float64(2), "repeatUntil": "2026-09-21T20:13:20Z",
		"workflowRunId": "wfr_queue",
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %#v, want %#v", k, m[k], v)
		}
	}
	if _, ok := m["final"]; ok {
		t.Errorf("a tick with runs left must not be marked final: %v", m)
	}

	// The last tick says so, and its remaining count is an honest zero rather than absent.
	e.Target.TimerOccurrence = 30
	e.Target.TimerFinal = true
	m = dueMessageJSON(t, BuildWakePrompt([]domain.QueueEvent{e}, nil))[0]
	if m["final"] != true || m["remainingRuns"] != float64(0) {
		t.Fatalf("last tick: final=%v remainingRuns=%v, want true/0", m["final"], m["remainingRuns"])
	}
	// An until-only loop ending early is final even with the cap not reached.
	e.Target.TimerOccurrence = 12
	m = dueMessageJSON(t, BuildWakePrompt([]domain.QueueEvent{e}, nil))[0]
	if m["final"] != true || m["remainingRuns"] != float64(0) {
		t.Fatalf("deadline-ended tick: final=%v remainingRuns=%v, want true/0", m["final"], m["remainingRuns"])
	}
}

// The framing names the one metadata field the model must act on.
func TestTimerMessageWakePromptExplainsFinal(t *testing.T) {
	p := BuildWakePrompt([]domain.QueueEvent{timerMessageEvent("evt_1", "tmr_1", "run the tests")}, nil)
	if !strings.Contains(p, `"final": true means no further occurrence will fire`) {
		t.Fatalf("framing must explain final:\n%s", p)
	}
}

// Loop metadata is never meaning: an event carrying all of it but NOT the marker is not
// a scheduled message, and ClearTimerMessage removes all of it together.
func TestLoopMetadataDoesNotMakeAMessage(t *testing.T) {
	e := timerMessageEvent("evt_1", "tmr_1", "run the tests")
	e.Target.TimerEveryMs, e.Target.TimerMaxRuns, e.Target.TimerRepeatUntil, e.Target.TimerFinal = 60_000, 5, 1, true
	if !IsTimerMessageEvent(e) {
		t.Fatal("precondition: marked event is a message")
	}
	cleared := e.Target.ClearTimerMessage()
	e.Target = &cleared
	if IsTimerMessageEvent(e) {
		t.Fatal("a cleared target must not be a message")
	}
	if cleared.TimerEveryMs != 0 || cleared.TimerMaxRuns != 0 || cleared.TimerRepeatUntil != 0 || cleared.TimerFinal || cleared.TimerOccurrence != 0 {
		t.Fatalf("ClearTimerMessage left loop metadata behind: %+v", cleared)
	}
	if cleared.TimerID != "tmr_1" {
		t.Fatalf("ClearTimerMessage must keep the structural timer link, got %+v", cleared)
	}
}
