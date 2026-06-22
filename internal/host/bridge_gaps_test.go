package host

import (
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Additional bridge behaviors: an empty final response closes the turn as
// "unknown", a tool reports its real duration in ms with the danger hint, and the
// full result→audit {result, severity} map holds.

func TestBridgeEmptyFinalResponseIsUnknown(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post})
	b.AssistantStart()
	b.AssistantEnd("   ", "") // all-whitespace → unknown outcome
	snap := c.snapshot()
	end := snap[len(snap)-1].(EvTurnEnd)
	if end.Outcome != OutcomeUnknown {
		t.Fatalf("empty final response outcome = %q, want unknown", end.Outcome)
	}
}

func TestBridgeToolDurationAndDanger(t *testing.T) {
	c := &collector{}
	clock := int64(1000)
	b := NewBridge(BridgeOptions{
		SessionID: "s", Post: c.post,
		Now: func() int64 { return clock },
		RiskOf: func(n string) (domain.RiskClass, bool) {
			if n == "git.commit" {
				return domain.RiskGit, true
			}
			return domain.RiskRead, true
		},
	})
	b.AssistantStart()
	b.ToolCall(agent.ToolCallEvent{ID: "tc1", Name: "git.commit", StartedAt: 1000})
	b.ToolResult(agent.ToolResultEvent{ID: "tc1", Name: "git.commit", Result: domain.Ok("done", nil), EndedAt: 1042})

	var started *EvToolStarted
	var settled *EvToolSettled
	for _, e := range c.snapshot() {
		switch ev := e.(type) {
		case EvToolStarted:
			s := ev
			started = &s
		case EvToolSettled:
			s := ev
			settled = &s
		}
	}
	if started == nil || settled == nil {
		t.Fatal("missing tool:started/settled")
	}
	if !started.Danger {
		t.Error("git.commit must be danger")
	}
	if started.ToolCallID != "tc1" {
		t.Errorf("toolCallId = %q", started.ToolCallID)
	}
	if settled.DurationMs != 42 {
		t.Errorf("durationMs = %d, want 42", settled.DurationMs)
	}
	if settled.Result != AuditSuccess || settled.Severity != SeverityInfo {
		t.Errorf("settled result/severity = %v/%v", settled.Result, settled.Severity)
	}
}

func TestBridgeResultToAuditMap(t *testing.T) {
	cases := []struct {
		res      domain.ToolResult
		result   AuditResult
		severity AuditSeverity
	}{
		{domain.Ok("", nil), AuditSuccess, SeverityInfo},
		{domain.Fail("CONFIRMATION_REQUIRED", ""), AuditConfirmationPending, SeverityNotice},
		{domain.Fail("UNAUTHORIZED", ""), AuditUnauthorized, SeverityWarning},
		{domain.Fail("RATE_LIMITED", ""), AuditRateLimited, SeverityWarning},
		{domain.Fail("SOMETHING_ELSE", ""), AuditError, SeverityErrorSev},
	}
	for _, c := range cases {
		gotResult, gotSev, _ := resultToAudit(c.res)
		if gotResult != c.result || gotSev != c.severity {
			t.Errorf("resultToAudit(%+v) = %v/%v, want %v/%v",
				c.res, gotResult, gotSev, c.result, c.severity)
		}
	}
}
