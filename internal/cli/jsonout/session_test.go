package jsonout

import (
	"bytes"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

// session_test.go pins the two additions that make the --json stream self-sufficient
// for a scripted consumer: the one-time `session` header (how to FIND this run) and the
// terminal envelope's `stats` block (what the run cost).

func sampleSession() SessionInfo {
	return SessionInfo{
		SessionID:    "ses_abc123",
		Project:      "/repo",
		Tier:         "operator",
		BackendURL:   "http://127.0.0.1:8473",
		LogPath:      "/logs/2026-08-21-ses_abc123.log",
		Version:      "test-version",
		AutoApprove:  true,
		MCPConnected: true,
		MCPTransport: "streamable-http",
	}
}

func TestSessionLineCarriesTheFactsNeededToFindTheRun(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Session(sampleSession())
	s.AssistantStart()
	s.AssistantEnd("done", "")
	s.Finish()

	lines := decodeLines(t, &buf)
	first := lines[0]
	if first["type"] != "session" || first["seq"] != float64(0) {
		t.Fatalf("session must be the first line at seq 0, got %v", first)
	}
	for key, want := range map[string]any{
		"sessionId":    "ses_abc123",
		"project":      "/repo",
		"tier":         "operator",
		"backendUrl":   "http://127.0.0.1:8473",
		"logPath":      "/logs/2026-08-21-ses_abc123.log",
		"version":      "test-version",
		"autoApprove":  true,
		"mcpConnected": true,
		"mcpTransport": "streamable-http",
	} {
		if first[key] != want {
			t.Errorf("session.%s = %v, want %v", key, first[key], want)
		}
	}
	// logPath must be PRESENT-but-empty when logging is off, so a consumer can tell
	// "disabled" from "this schema version has no such field".
	var off bytes.Buffer
	s2 := New(&off, fixedClock)
	info := sampleSession()
	info.LogPath = ""
	s2.Session(info)
	if _, ok := decodeLines(t, &off)[0]["logPath"]; !ok {
		t.Error("logPath must be emitted even when empty")
	}
}

// TestSessionLineCarriesNoCredential: the header names the endpoint, never the key. A
// --json stream is routinely captured into CI logs.
//
// It asserts on the raw SERIALIZED line, not on the field names. Checking only the keys
// was false confidence: the leak that mattered came through backendUrl's VALUE, which a
// name-only check passes happily.
func TestSessionLineCarriesNoCredential(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	info := sampleSession()
	// The caller is responsible for sanitizing (RunOneShot applies mcp.SanitizeURL), so
	// this fixture proves the SINK does not invent a place to hide one — the endpoint
	// field is the only URL on the line, and there is no second field echoing it.
	info.BackendURL = "https://backend.example/api"
	s.Session(info)
	s.Finish()

	raw := buf.String()
	line := decodeLines(t, bytes.NewBufferString(raw))[0]
	for k := range line {
		for _, banned := range []string{"apiKey", "api_key", "token", "key", "secret", "credential", "password"} {
			if strings.EqualFold(k, banned) {
				t.Errorf("session line exposes a credential-shaped field %q", k)
			}
		}
	}
	// The line must contain no userinfo-shaped substring at all.
	if strings.Contains(raw, "@") {
		t.Errorf("session line contains an '@' — check for URL userinfo: %s", raw)
	}
}

// TestSessionKeysMatchTheContract: the sink marshals domain.JsonSessionPayload rather
// than a hand-written map, so the emitted keys ARE the contract's json tags. This pins
// that they stay the documented set — a renamed tag or a dropped field breaks every
// consumer silently, and `omitempty` on a field a consumer reads unconditionally is the
// same bug in slower motion.
func TestSessionKeysMatchTheContract(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	// Zero value on purpose: every key must appear even when empty/false, so a consumer
	// can distinguish "off" from "absent".
	s.Session(SessionInfo{})
	s.Finish()

	line := decodeLines(t, &buf)[0]
	want := []string{
		"type", "ts", "seq",
		// schemaVersion is on the FIRST frame as well as the terminal result, so a
		// streaming consumer can reject an unknown schema before it parses anything
		// else rather than after it has parsed everything.
		"schemaVersion",
		"sessionId", "project", "tier", "backendUrl", "logPath", "version",
		"autoApprove", "mcpConnected", "mcpTransport",
	}
	for _, k := range want {
		if _, ok := line[k]; !ok {
			t.Errorf("session line is missing %q (omitempty on a zero value?): %v", k, line)
		}
	}
	if len(line) != len(want) {
		t.Errorf("session line has %d keys, want exactly %d: %v", len(line), len(want), line)
	}
	// It must be the REAL version, not a zero left by the caller: the sink stamps it so
	// a consumer can always rely on it.
	if got := line["schemaVersion"]; got != float64(domain.JSONOutputSchemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", got, domain.JSONOutputSchemaVersion)
	}
}

// TestSchemaVersionAgreesAcrossFirstAndLastFrame: two declarations of the same fact are
// a drift hazard, so pin that they cannot disagree.
func TestSchemaVersionAgreesAcrossFirstAndLastFrame(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Session(SessionInfo{})
	s.Finish()

	lines := decodeLines(t, &buf)
	first := lines[0]
	last := lines[len(lines)-1]
	if last["type"] != "result" {
		t.Fatalf("last frame = %v, want result", last["type"])
	}
	if first["schemaVersion"] != last["schemaVersion"] {
		t.Errorf("session says schemaVersion %v but result says %v", first["schemaVersion"], last["schemaVersion"])
	}
}

// TestSessionIsEmittedOnce: a second call must be dropped rather than produce two
// conflicting headers a consumer would have to reconcile.
func TestSessionIsEmittedOnce(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Session(sampleSession())
	second := sampleSession()
	second.SessionID = "ses_different"
	s.Session(second)
	s.Finish()

	lines := decodeLines(t, &buf)
	count := 0
	for _, l := range lines {
		if l["type"] == "session" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("session line count = %d, want 1", count)
	}
	if lines[0]["sessionId"] != "ses_abc123" {
		t.Errorf("the FIRST header must win, got %v", lines[0]["sessionId"])
	}
}

func TestResultStatsCountWorkAndTokens(t *testing.T) {
	var buf bytes.Buffer
	// An explicitly CONTROLLED clock, not one that advances per read: coupling the
	// assertion to how many times the sink happens to read the clock makes the test
	// break on unrelated changes and, worse, makes an off-by-one look like a real bug.
	now := int64(1000)
	s := New(&buf, func() int64 { return now })
	s.AssistantStart()
	s.Usage(agent.UsageEvent{PromptTokens: 100, CompletionTokens: 10, TotalTokens: 110, ContextTokens: 100})
	s.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "fs.read"})
	s.ToolResult(agent.ToolResultEvent{ID: "c1", Name: "fs.read", Result: domain.Ok("ok", nil)})
	s.ToolCall(agent.ToolCallEvent{ID: "c2", Name: "git.push"})
	s.ToolResult(agent.ToolResultEvent{ID: "c2", Name: "git.push", Result: domain.Fail("rejected", "non-fast-forward")})
	s.AssistantStart()
	s.Usage(agent.UsageEvent{PromptTokens: 300, CompletionTokens: 20, TotalTokens: 320, ContextTokens: 300})
	s.AssistantEnd("done", "")
	now = 1450 // 450ms of wall clock between New and Finish
	code := s.Finish()

	if code != domain.OneShotExitCode.Success {
		t.Fatalf("exit = %d, want 0 — a failed TOOL is recoverable context, not a failed run", code)
	}
	lines := decodeLines(t, &buf)
	stats, ok := lines[len(lines)-1]["stats"].(map[string]any)
	if !ok {
		t.Fatalf("terminal envelope has no stats block: %v", lines[len(lines)-1])
	}
	for key, want := range map[string]float64{
		"rounds":     2,
		"toolCalls":  2,
		"toolErrors": 1,
		// Tokens sum across rounds...
		"promptTokens":     400,
		"completionTokens": 30,
		"totalTokens":      430,
		// ...but contextTokens is the LAST round's prompt size, not a sum: it is the
		// compaction-pressure figure, and summing it would be meaningless.
		"contextTokens": 300,
	} {
		if stats[key] != want {
			t.Errorf("stats.%s = %v, want %v", key, stats[key], want)
		}
	}
	// Deliberately NOT a `> 0` check, which would pass for almost any formula.
	if d, ok := stats["durationMs"].(float64); !ok || int(d) != 450 {
		t.Errorf("stats.durationMs = %v, want 450 (Finish's clock minus New's)", stats["durationMs"])
	}
}

// TestDurationNeverGoesNegative: the Clock is wall time, so an NTP step between New and
// Finish can move it backwards. A negative duration reads as corruption downstream, so
// it clamps to 0 — an understated duration is a small lie, a negative one breaks
// arithmetic.
func TestDurationNeverGoesNegative(t *testing.T) {
	var buf bytes.Buffer
	now := int64(5000)
	s := New(&buf, func() int64 { return now })
	s.AssistantEnd("done", "")
	now = 1000 // the clock stepped backwards mid-run
	s.Finish()

	lines := decodeLines(t, &buf)
	stats := lines[len(lines)-1]["stats"].(map[string]any)
	if d, _ := stats["durationMs"].(float64); d < 0 {
		t.Errorf("stats.durationMs = %v, want >= 0 after a clock rollback", d)
	}
}

// TestStatsPresentOnAFailedRun: a consumer gating on stats must not have to special-case
// the failure path — the block is always there, even when nothing ran.
func TestStatsPresentOnAFailedRun(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Error("could not reach the backend")
	s.Finish()

	last := decodeLines(t, &buf)[1]
	stats, ok := last["stats"].(map[string]any)
	if !ok {
		t.Fatalf("failed run has no stats block: %v", last)
	}
	if stats["rounds"] != float64(0) || stats["toolCalls"] != float64(0) {
		t.Errorf("stats should be zeroed, got %v", stats)
	}
}

// TestWarnDoesNotPoisonTheTerminalEnvelope is the regression guard for the auto-approve
// bug: routing a non-fatal notice through Error stamped errorMessage, and
// AssistantCancelled never clears it — so a cancelled run reported the warning as its
// cause of failure.
func TestWarnDoesNotPoisonTheTerminalEnvelope(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Warn("AUTO-APPROVE is ON")
	s.AssistantStart()
	s.AssistantCancelled("")
	code := s.Finish()

	if code != domain.OneShotExitCode.Cancelled {
		t.Fatalf("exit = %d, want %d", code, domain.OneShotExitCode.Cancelled)
	}
	lines := decodeLines(t, &buf)
	last := lines[len(lines)-1]
	if last["status"] != string(domain.JSONStatusCancelled) {
		t.Errorf("status = %v, want cancelled", last["status"])
	}
	// The whole point: a warning must leave `error` null. Routed through Error it would
	// arrive here as {"message":"AUTO-APPROVE is ON"}, blaming the notice for the cancel.
	if last["error"] != nil {
		t.Errorf("a warning must not become the run's error, got %v", last["error"])
	}
	if types(lines)[0] != "warning" {
		t.Errorf("Warn must emit a warning line, got %v", types(lines))
	}
}

// TestCancelRunPreservesTheAnswerAndEmitsNoSecondAssistantEvent: a --run-scheduler
// one-shot whose --timeout fires while it waits for async work to settle has a real
// answer already streamed. The run is cancelled, but the answer must survive into the
// terminal `result` line — and the turn must not sprout a second terminal assistant
// event, which is why this is not AssistantCancelled.
func TestCancelRunPreservesTheAnswerAndEmitsNoSecondAssistantEvent(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Session(sampleSession())
	s.AssistantStart()
	s.AssistantEnd("spawned two agents", "")
	s.Warn("timed out after 5m waiting for async work to settle")
	s.CancelRun()
	code := s.Finish()

	if code != domain.OneShotExitCode.Cancelled {
		t.Errorf("exit code = %d, want %d (cancelled)", code, domain.OneShotExitCode.Cancelled)
	}
	lines := decodeLines(t, &buf)
	last := lines[len(lines)-1]
	if last["type"] != "result" {
		t.Fatalf("last line type = %v, want result", last["type"])
	}
	if last["status"] != string(domain.JSONStatusCancelled) {
		t.Errorf("status = %v, want cancelled", last["status"])
	}
	if last["content"] != "spawned two agents" {
		t.Errorf("content = %v, want the completed answer preserved", last["content"])
	}
	if last["error"] != nil {
		t.Errorf("error = %v, want null (a cancelled wait is not a turn error)", last["error"])
	}
	for _, l := range lines {
		if l["type"] == "assistant:cancelled" {
			t.Error("CancelRun emitted assistant:cancelled; the turn already ended with assistant:end")
		}
	}
}

// TestCancelRunNeverDowngradesAFailedRun: an error status carries a message and a
// non-zero code that say strictly more than "cancelled" does, so the late wait timeout
// must not overwrite it.
func TestCancelRunNeverDowngradesAFailedRun(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	s.Error("backend unreachable")
	s.CancelRun()
	code := s.Finish()

	if code != domain.OneShotExitCode.Error {
		t.Errorf("exit code = %d, want %d (error must survive CancelRun)", code, domain.OneShotExitCode.Error)
	}
	lines := decodeLines(t, &buf)
	last := lines[len(lines)-1]
	if last["status"] != string(domain.JSONStatusError) {
		t.Errorf("status = %v, want error", last["status"])
	}
}

// The documented guarantee is that a streaming consumer can reject an incompatible
// schema on the FIRST line it sees. That was false for the case that needs it most: a
// setup failure emits `error` before any session frame exists, so the version has to
// live on the common envelope rather than only on the session header.
func TestEverySchemaVersionIsOnEveryFrameIncludingAnErrorBeforeTheSession(t *testing.T) {
	var buf bytes.Buffer
	s := New(&buf, fixedClock)
	// No Session() call at all — exactly the setup-failure shape.
	s.Error("could not acquire the project lease")
	s.Finish()

	lines := decodeLines(t, &buf)
	if len(lines) == 0 {
		t.Fatal("no frames emitted")
	}
	for i, line := range lines {
		got, ok := line["schemaVersion"]
		if !ok {
			t.Fatalf("frame %d (%v) carries no schemaVersion: %v", i, line["type"], line)
		}
		if got != float64(domain.JSONOutputSchemaVersion) {
			t.Errorf("frame %d schemaVersion = %v, want %d", i, got, domain.JSONOutputSchemaVersion)
		}
	}
	if lines[0]["type"] == "session" {
		t.Fatal("this test must exercise the no-session path to be meaningful")
	}
}
