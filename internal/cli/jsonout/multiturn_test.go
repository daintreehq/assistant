package jsonout

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// mtLines parses a sink's output into decoded JSONL lines.
func mtLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatalf("non-JSON line %q: %v", raw, err)
		}
		out = append(out, m)
	}
	return out
}

// mtTypes is the ordered `type` sequence, the shape most of these tests assert on.
func mtTypes(lines []map[string]any) []string {
	types := make([]string, 0, len(lines))
	for _, l := range lines {
		t, _ := l["type"].(string)
		types = append(types, t)
	}
	return types
}

func mtFind(lines []map[string]any, typ string) []map[string]any {
	var out []map[string]any
	for _, l := range lines {
		if s, _ := l["type"].(string); s == typ {
			out = append(out, l)
		}
	}
	return out
}

// mtFindN is mtFind with the count asserted up front. Every caller here indexes the
// result, so a wrong count must FAIL the test rather than panic three lines later — and
// a test that silently iterated zero matches would pass while proving nothing.
func mtFindN(t *testing.T, lines []map[string]any, typ string, want int) []map[string]any {
	t.Helper()
	got := mtFind(lines, typ)
	if len(got) != want {
		t.Fatalf("%s lines = %d, want %d", typ, len(got), want)
	}
	return got
}

// mtClock is a monotonically advancing fake clock.
func mtClock() Clock {
	n := int64(1000)
	return func() int64 { n += 10; return n }
}

// runTurnOK drives one successful turn through the sink.
func runTurnOK(s *Sink, prompt, answer string) {
	s.BeginTurn(prompt)
	s.AssistantStart()
	s.AssistantEnd(answer, "")
	s.SettleTurn()
}

// TestMultiTurnBracketsEachTurnAndKeepsOneSessionAndOneResult pins the central shape of
// the feature: one header, one terminal envelope, and a turn:prompt/turn:end bracket per
// prompt — with seq monotonic across the WHOLE process rather than restarting per turn.
func TestMultiTurnBracketsEachTurnAndKeepsOneSessionAndOneResult(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.Session(SessionInfo{SessionID: "ses_1"})
	runTurnOK(s, "first", "one")
	runTurnOK(s, "second", "two")
	if code := s.Finish(); code != domain.OneShotExitCode.Success {
		t.Fatalf("exit code = %d, want success", code)
	}

	lines := mtLines(t, &buf)
	want := []string{
		"session",
		"turn:prompt", "assistant:start", "assistant:end", "turn:end",
		"turn:prompt", "assistant:start", "assistant:end", "turn:end",
		"result",
	}
	if got := mtTypes(lines); !equalStrings(got, want) {
		t.Fatalf("line types =\n %v\nwant\n %v", got, want)
	}
	mtFindN(t, lines, "session", 1)
	mtFindN(t, lines, "result", 1)
	// seq is one ordered transcript, not per-turn counters.
	for i, l := range lines {
		seq, ok := l["seq"].(float64)
		if !ok || int(seq) != i {
			t.Fatalf("line %d has seq %v, want %d (monotonic across the process)", i, l["seq"], i)
		}
	}
	// Turn numbers are zero-based and advance only for model turns.
	prompts := mtFindN(t, lines, "turn:prompt", 2)
	for i, p := range prompts {
		if got, _ := p["turn"].(float64); int(got) != i {
			t.Errorf("turn:prompt[%d].turn = %v, want %d", i, p["turn"], i)
		}
	}
	if got, _ := prompts[0]["prompt"].(string); got != "first" {
		t.Errorf("turn:prompt[0].prompt = %q, want %q", got, "first")
	}
	if got, _ := prompts[1]["prompt"].(string); got != "second" {
		t.Errorf("turn:prompt[1].prompt = %q, want %q", got, "second")
	}
}

// TestTurnBracketSpansEveryRoundOfOneSend is the invariant borrowed from the embedded
// host's Bridge: a turn is one whole Session.Send, so several assistant:start rounds —
// and the tool calls between them — live inside ONE bracket. A consumer that treated
// assistant:start as a turn marker would mis-slice every tool-using turn.
func TestTurnBracketSpansEveryRoundOfOneSend(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("do a thing")
	s.AssistantStart()
	s.ToolCall(agent.ToolCallEvent{ID: "t1", Name: "memory.list", Args: `{}`})
	s.ToolResult(agent.ToolResultEvent{ID: "t1", Name: "memory.list", Result: domain.Ok("done", nil)})
	s.AssistantStart()
	s.AssistantEnd("finished", "")
	s.SettleTurn()
	s.Finish()

	types := mtTypes(mtLines(t, &buf))
	want := []string{
		"turn:prompt", "assistant:start", "tool:call", "tool:result",
		"assistant:start", "assistant:end", "turn:end", "result",
	}
	if !equalStrings(types, want) {
		t.Fatalf("line types =\n %v\nwant\n %v", types, want)
	}
}

// TestLaterSuccessNeverForgivesAnEarlierFailedTurn is the issue's stated requirement:
// "a run where turn two failed is a failed run". AssistantEnd resets the TURN's status
// to success and clears its error, which is right within a turn and would be
// catastrophic across turns without the run-level latch.
func TestLaterSuccessNeverForgivesAnEarlierFailedTurn(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	runTurnOK(s, "fine", "ok")

	s.BeginTurn("breaks")
	s.AssistantStart()
	s.Error("backend exploded")
	s.SettleTurn()

	runTurnOK(s, "fine again", "still ok")

	code := s.Finish()
	if code != domain.OneShotExitCode.Error {
		t.Fatalf("exit code = %d, want %d — a run whose turn 2 failed is a failed run",
			code, domain.OneShotExitCode.Error)
	}
	lines := mtLines(t, &buf)
	result := mtFindN(t, lines, "result", 1)[0]
	if got, _ := result["status"].(string); got != string(domain.JSONStatusError) {
		t.Errorf("result.status = %q, want %q", got, domain.JSONStatusError)
	}
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "backend exploded" {
		t.Errorf("result.error = %v, want the failed turn's message", result["error"])
	}
	// The failure is reported per-turn too, and only on the turn that failed.
	wantStatuses := []string{"success", "error", "success"}
	ends := mtFindN(t, lines, "turn:end", len(wantStatuses))
	for i, e := range ends {
		if got, _ := e["status"].(string); got != wantStatuses[i] {
			t.Errorf("turn:end[%d].status = %q, want %q", i, got, wantStatuses[i])
		}
	}
	// The final answer is still the LAST turn's, so a consumer reading only `result`
	// gets the conversation's conclusion alongside the run's failure.
	if got, _ := result["content"].(string); got != "still ok" {
		t.Errorf("result.content = %q, want the last turn's answer", got)
	}
}

// TestCancelledTurnNeverDowngradesAnEarlierError mirrors CancelRun's existing rule at
// the run level: an error carries a message and outranks a later cancellation.
func TestCancelledTurnNeverDowngradesAnEarlierError(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("breaks")
	s.Error("boom")
	s.SettleTurn()

	s.BeginTurn("cut short")
	s.AssistantStart()
	s.AssistantCancelled("partial")
	s.SettleTurn()

	if code := s.Finish(); code != domain.OneShotExitCode.Error {
		t.Fatalf("exit code = %d, want error to outrank the later cancellation", code)
	}
}

// TestCancelledTurnFailsTheRunWhenNothingElseFailed covers the other precedence edge:
// cancelled outranks success.
func TestCancelledTurnFailsTheRunWhenNothingElseFailed(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	runTurnOK(s, "fine", "ok")
	s.BeginTurn("cut short")
	s.AssistantStart()
	s.AssistantCancelled("partial")
	s.SettleTurn()

	if code := s.Finish(); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit code = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
}

// TestStatsAccumulateAcrossTurns pins the issue's "natural read": stats already sum
// across rounds, so they sum across turns too. There is exactly one accounting block,
// on the terminal line, for the whole process.
func TestStatsAccumulateAcrossTurns(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())

	s.BeginTurn("one")
	s.AssistantStart()
	s.ToolCall(agent.ToolCallEvent{ID: "a", Name: "x"})
	s.ToolResult(agent.ToolResultEvent{ID: "a", Name: "x", Result: domain.Ok("ok", nil)})
	s.Usage(agent.UsageEvent{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12, ContextTokens: 100})
	s.AssistantEnd("done", "")
	s.SettleTurn()

	s.BeginTurn("two")
	s.AssistantStart()
	s.ToolCall(agent.ToolCallEvent{ID: "b", Name: "y"})
	s.ToolResult(agent.ToolResultEvent{ID: "b", Name: "y", Result: domain.Fail("NOPE", "no")})
	s.Usage(agent.UsageEvent{PromptTokens: 30, CompletionTokens: 4, TotalTokens: 34, ContextTokens: 250})
	s.AssistantEnd("done", "")
	s.SettleTurn()

	s.Finish()
	result := mtFindN(t, mtLines(t, &buf), "result", 1)[0]
	stats, _ := result["stats"].(map[string]any)
	for key, want := range map[string]int{
		"rounds": 2, "toolCalls": 2, "toolErrors": 1,
		"promptTokens": 40, "completionTokens": 6, "totalTokens": 46,
		// Not a sum: the LAST round's prompt size, the compaction-pressure figure.
		"contextTokens": 250,
	} {
		if got, _ := stats[key].(float64); int(got) != want {
			t.Errorf("stats.%s = %v, want %d", key, stats[key], want)
		}
	}
}

// TestCommandResultIsRecordedWithoutOpeningATurn pins that a slash command between
// prompts appears in the transcript, does not move the turn number, and cannot change
// the run's outcome.
func TestCommandResultIsRecordedWithoutOpeningATurn(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	runTurnOK(s, "first", "one")
	s.CommandResult(domain.JsonCommandResultPayload{
		Command: "/clear", Handled: true, Title: "Clear",
		Content: "Conversation cleared.", ConversationCleared: true,
	})
	runTurnOK(s, "second", "two")
	if code := s.Finish(); code != domain.OneShotExitCode.Success {
		t.Fatalf("exit code = %d, want success — a command never changes the run outcome", code)
	}

	lines := mtLines(t, &buf)
	c := mtFindN(t, lines, "command:result", 1)[0]
	for key, want := range map[string]any{
		"command": "/clear", "handled": true, "title": "Clear",
		"content": "Conversation cleared.", "quit": false, "conversationCleared": true,
	} {
		if c[key] != want {
			t.Errorf("command:result.%s = %v, want %v", key, c[key], want)
		}
	}
	// The command did NOT consume a turn number: the second bracket is still turn 1.
	prompts := mtFindN(t, lines, "turn:prompt", 2)
	if got, _ := prompts[1]["turn"].(float64); int(got) != 1 {
		t.Errorf("turn after a command = %v, want 1 (a command is not a turn)", prompts[1]["turn"])
	}
}

// TestClearResetsTheConversationNotTheTranscript: /clear wipes session state, but the
// transcript is an append-only record. seq keeps climbing, stats keep accumulating, and
// an earlier failed turn stays failed — otherwise a script could launder its own failure.
func TestClearResetsTheConversationNotTheTranscript(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("breaks")
	s.AssistantStart()
	s.Error("boom")
	s.SettleTurn()

	s.CommandResult(domain.JsonCommandResultPayload{Command: "/clear", Handled: true, ConversationCleared: true})

	runTurnOK(s, "fresh start", "fine")

	if code := s.Finish(); code != domain.OneShotExitCode.Error {
		t.Fatalf("exit code = %d, want error — /clear must not forgive an earlier failed turn", code)
	}
	lines := mtLines(t, &buf)
	for i, l := range lines {
		if seq, _ := l["seq"].(float64); int(seq) != i {
			t.Fatalf("seq restarted at line %d (%v); /clear must not reset the transcript", i, l["seq"])
		}
	}
	stats, _ := mtFindN(t, lines, "result", 1)[0]["stats"].(map[string]any)
	if got, _ := stats["rounds"].(float64); int(got) != 2 {
		t.Errorf("stats.rounds = %v, want 2 — accounting spans the whole process", stats["rounds"])
	}
}

// TestFinishClosesADanglingTurn: a turn cut short by the deadline still gets its
// turn:end, so a consumer is never left waiting for a boundary that never comes.
func TestFinishClosesADanglingTurn(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("never finishes")
	s.AssistantStart()
	if code := s.Finish(); code != domain.OneShotExitCode.Error {
		t.Fatalf("exit code = %d, want error — a turn with no terminal event is a failed turn", code)
	}
	types := mtTypes(mtLines(t, &buf))
	want := []string{"turn:prompt", "assistant:start", "turn:end", "result"}
	if !equalStrings(types, want) {
		t.Fatalf("line types = %v, want %v", types, want)
	}
}

// TestPostTurnCancelRunStillReachesTheTerminalLine covers the --run-scheduler async
// barrier expiring AFTER the last turn already succeeded: the answer survives, the run
// reports cancelled, and no second assistant terminal event is invented.
func TestPostTurnCancelRunStillReachesTheTerminalLine(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	runTurnOK(s, "spawn something", "started")
	s.CancelRun()

	if code := s.Finish(); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit code = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
	result := mtFindN(t, mtLines(t, &buf), "result", 1)[0]
	if got, _ := result["content"].(string); got != "started" {
		t.Errorf("result.content = %q, want the answer to survive the run-level cancel", got)
	}
	if got, _ := result["status"].(string); got != string(domain.JSONStatusCancelled) {
		t.Errorf("result.status = %q, want cancelled", got)
	}
}

// TestExitCodeReportsTheRunNotTheLiveTurn: mid-conversation, ExitCode must already
// carry an earlier turn's failure rather than the current turn's optimism.
func TestExitCodeReportsTheRunNotTheLiveTurn(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("breaks")
	s.Error("boom")
	s.SettleTurn()

	s.BeginTurn("recovers")
	s.AssistantStart()
	s.AssistantEnd("fine", "")
	if got := s.ExitCode(); got != domain.OneShotExitCode.Error {
		t.Fatalf("ExitCode() = %d mid-conversation, want the run's %d", got, domain.OneShotExitCode.Error)
	}
}

// TestSingleTurnSinkEmitsNoTurnLines is the backward-compatibility pin. The plain
// constructor must be untouched by any of this: the multi-turn calls are inert, so an
// ordinary `--json "prompt"` stream is what it has always been.
func TestSingleTurnSinkEmitsNoTurnLines(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, mtClock())
	s.Session(SessionInfo{SessionID: "ses_1"})
	// Every multi-turn entry point, called on a plain sink.
	s.BeginTurn("should not appear")
	s.AssistantStart()
	s.AssistantEnd("answer", "")
	s.SettleTurn()
	s.CommandResult(domain.JsonCommandResultPayload{Command: "/clear", Handled: true})
	s.Finish()

	types := mtTypes(mtLines(t, &buf))
	want := []string{"session", "assistant:start", "assistant:end", "result"}
	if !equalStrings(types, want) {
		t.Fatalf("single-turn line types = %v, want %v (no turn/command lines)", types, want)
	}
}

// TestCancelBeforeAnyTurnReportsCancelledNotError is the bug the default sentinel hid.
// A fresh sink's status is already "error" — meaning "nothing terminal has happened
// yet", not "something failed" — so a guard that looked only at the status made
// CancelRun a no-op in precisely the case it exists for: a --timeout expiring while the
// loop waited for the first line of stdin. That reported a bare error/exit-1 with a null
// message, sending the reader hunting a failure that never occurred.
func TestCancelBeforeAnyTurnReportsCancelledNotError(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.Session(SessionInfo{SessionID: "ses_1"})
	s.CancelRun() // the bound expired before a single prompt was read

	if code := s.Finish(); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit code = %d, want %d — nothing failed, the run was cut short",
			code, domain.OneShotExitCode.Cancelled)
	}
	result := mtFindN(t, mtLines(t, &buf), "result", 1)[0]
	if got, _ := result["status"].(string); got != string(domain.JSONStatusCancelled) {
		t.Errorf("result.status = %q, want cancelled", got)
	}
	// And no assistant event was invented for a turn that never opened.
	for _, typ := range []string{"assistant:start", "assistant:cancelled", "turn:prompt"} {
		if n := len(mtFind(mtLines(t, &buf), typ)); n != 0 {
			t.Errorf("%s lines = %d, want 0 — no turn ever started", typ, n)
		}
	}
}

// TestCancelBetweenTurnsReachesTheTerminalLine: cancellation between turns has no live
// turn to record it, and the next BeginTurn would reset the turn-level fields anyway —
// so it has to reach the run aggregate directly or it would vanish.
func TestCancelBetweenTurnsReachesTheTerminalLine(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	runTurnOK(s, "first", "one")
	s.CancelRun() // deadline expired while waiting for the next line

	if code := s.Finish(); code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit code = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
	result := mtFindN(t, mtLines(t, &buf), "result", 1)[0]
	if got, _ := result["content"].(string); got != "one" {
		t.Errorf("result.content = %q, want the completed turn's answer to survive", got)
	}
}

// TestCancelRunStillRefusesToDowngradeARealError guards the other side of the same
// change: an error that carries a MESSAGE says more than "cancelled" and must win.
func TestCancelRunStillRefusesToDowngradeARealError(t *testing.T) {
	var buf bytes.Buffer
	s := NewMultiTurn(&buf, mtClock())
	s.BeginTurn("breaks")
	s.Error("boom")
	s.SettleTurn()
	s.CancelRun()

	if code := s.Finish(); code != domain.OneShotExitCode.Error {
		t.Fatalf("exit code = %d, want error to outrank a later cancellation", code)
	}
	result := mtFindN(t, mtLines(t, &buf), "result", 1)[0]
	errObj, _ := result["error"].(map[string]any)
	if errObj == nil || errObj["message"] != "boom" {
		t.Errorf("result.error = %v, want the real failure's message preserved", result["error"])
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
