package host

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
)

func TestRedactArgs(t *testing.T) {
	long := strings.Repeat("a", 100)
	cases := []struct{ in, want string }{
		{"", ""},
		{`null`, ""},
		{`"short"`, `"short"`},
		{`"` + long + `"`, "<string: 100 chars>"},
		{`42`, "42"},
		{`true`, "true"},
		{`{"k":"v","big":"` + long + `","arr":[1],"obj":{"x":1},"n":3}`,
			"OBJECT"}, // checked structurally below
		{`[1,2]`, `{"0":1,"1":2}`}, // array → string-keyed object quirk
		{`not json`, `"not json"`},
	}
	for _, c := range cases {
		got := redactArgs(c.in)
		if c.want == "OBJECT" {
			// encoding/json HTML-escapes '<' to <; assert on the escaped forms.
			if !strings.Contains(got, `"big":"<string: 100 chars>"`) ||
				!strings.Contains(got, `"arr":"<array>"`) ||
				!strings.Contains(got, `"obj":"<object>"`) ||
				!strings.Contains(got, `"k":"v"`) ||
				!strings.Contains(got, `"n":3`) {
				t.Errorf("object redaction wrong: %s", got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("redactArgs(%q)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestResultToAudit(t *testing.T) {
	r, sev, code := resultToAudit(domain.Ok("done", nil))
	if r != AuditSuccess || sev != SeverityInfo || code != "" {
		t.Fatalf("ok result mapped wrong: %v %v %q", r, sev, code)
	}
	r, sev, code = resultToAudit(domain.Fail("UNAUTHORIZED", "no"))
	if r != AuditUnauthorized || sev != SeverityWarning || code != "UNAUTHORIZED" {
		t.Fatalf("unauthorized mapped wrong: %v %v %q", r, sev, code)
	}
	r, _, _ = resultToAudit(domain.Fail("WEIRD", "x"))
	if r != AuditError {
		t.Fatalf("unknown code want error, got %v", r)
	}
}

// collectPost captures emitted events for assertions (timer/goroutine-safe).
type collector struct {
	mu  sync.Mutex
	evs []HostEvent
}

func (c *collector) post(ev HostEvent) {
	c.mu.Lock()
	c.evs = append(c.evs, ev)
	c.mu.Unlock()
}
func (c *collector) snapshot() []HostEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]HostEvent, len(c.evs))
	copy(out, c.evs)
	return out
}
func (c *collector) types() []string {
	var out []string
	for _, e := range c.snapshot() {
		out = append(out, eventType(e))
	}
	return out
}

func eventType(e HostEvent) string {
	switch e.(type) {
	case EvReady:
		return "host:ready"
	case EvTurnStart:
		return "turn:start"
	case EvTurnToken:
		return "turn:token"
	case EvTurnEnd:
		return "turn:end"
	case EvToolStarted:
		return "tool:started"
	case EvToolSettled:
		return "tool:settled"
	case EvApprovalRequested:
		return "approval:requested"
	case EvApprovalDecided:
		return "approval:decided"
	case EvError:
		return "host:error"
	case EvShutdown:
		return "host:shutdown"
	}
	return "?"
}

func TestBridgeTurnLifecycle(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post})

	b.StartExchange() // user turn: start+end
	b.AssistantStart()
	b.AssistantStart() // second start is a no-op (single open turn)
	b.AssistantToken("hi")
	b.AssistantEnd("answer", "")

	got := c.types()
	want := []string{"turn:start", "turn:end", "turn:start", "turn:token", "turn:end"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("lifecycle events=%v want %v", got, want)
	}
	// First turn:start is the user role, third is assistant.
	evs := c.snapshot()
	if evs[0].(EvTurnStart).Role != RoleUser {
		t.Errorf("first turn not user")
	}
	if evs[2].(EvTurnStart).Role != RoleAssistant {
		t.Errorf("assistant turn not role assistant")
	}
	if evs[4].(EvTurnEnd).Outcome != OutcomeAnswered {
		t.Errorf("end outcome=%q want answered", evs[4].(EvTurnEnd).Outcome)
	}
}

// Neither backend-skill event has a host-protocol channel. Pinned alongside the
// turn-lifecycle assertions because the decision event carries the richest payload of the
// two, and "just forward it" is the reflex this guards against — a host that wants it
// reads the --json stream or the run transcript.
func TestBridgeSkillEventsPostNothing(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post})

	b.StartExchange()
	b.AssistantStart()
	b.SkillLoaded([]string{"Multi-agent orchestration"})
	b.SkillDecision(agent.SkillDecisionEvent{
		Active:   []agent.SkillRef{{ID: "multi_agent", Title: "Multi-agent orchestration"}},
		Selector: agent.SkillSelectorOutcome{Ran: true, Degraded: true},
	})
	b.AssistantToken("hi")
	b.AssistantEnd("answer", "")

	got := c.types()
	want := []string{"turn:start", "turn:end", "turn:start", "turn:token", "turn:end"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("skill events reached the host protocol: got %v want %v", got, want)
	}
}

func TestBridgeInterruptSuppresses(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post})
	b.StartExchange()
	b.AssistantStart()
	b.Interrupt() // latches interrupted, closes the turn as cancelled
	b.AssistantToken("late")
	b.ToolCall(agent.ToolCallEvent{ID: "t1", Name: "fs.read", StartedAt: 1})

	// No turn:token or tool:started after interrupt.
	for _, ty := range c.types() {
		if ty == "turn:token" || ty == "tool:started" {
			t.Fatalf("interrupt failed to suppress: %v", c.types())
		}
	}
	// The interrupt closes the assistant turn as CANCELLED, not agent-stuck. The user
	// pressed Stop; nothing hung. Recording a deliberate interruption as a fault
	// misreports it in the transcript and in every tally built from outcomes.
	snap := c.snapshot()
	last := snap[len(snap)-1].(EvTurnEnd)
	if last.Outcome != OutcomeCancelled {
		t.Fatalf("interrupt close outcome=%q want cancelled", last.Outcome)
	}
}

// An interrupt must terminalize every outstanding call. Without it the host was told
// each call started and never told anything else, so a stopped turn left rows
// rendering as "Running" permanently — describing work that is not happening.
func TestBridgeInterruptTerminalizesOutstandingCalls(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post})
	b.StartExchange()
	b.AssistantStart()
	b.ToolBatch([]agent.BatchedToolCall{{ID: "t1", Name: "fs.read"}, {ID: "t2", Name: "git.status"}})
	b.ToolState("t1", agent.ToolState("active"))

	b.Interrupt()

	states := map[string]string{}
	for _, ev := range c.snapshot() {
		if ts, ok := ev.(EvToolState); ok {
			states[ts.ToolCallID] = ts.State
		}
	}
	// The one that was running was cancelled; the one that never started was not run.
	// The distinction is what tells a reader what the stop actually interrupted.
	if states["t1"] != toolStateCancelled {
		t.Errorf("running call state=%q want cancelled", states["t1"])
	}
	if states["t2"] != toolStateNotRun {
		t.Errorf("queued call state=%q want not-run", states["t2"])
	}
}

func TestBridgeApprovalDecide(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post, ApprovalTimeoutMs: 0})

	done := make(chan bool, 1)
	go func() {
		done <- b.Confirm(context.Background(), ConfirmRequest{ToolName: "git.push", Summary: "push?"})
	}()

	// Wait for the approval:requested to land, then decide approved.
	var approvalID string
	deadline := time.After(time.Second)
	for approvalID == "" {
		select {
		case <-deadline:
			t.Fatal("no approval:requested emitted")
		default:
		}
		for _, e := range c.snapshot() {
			if ar, ok := e.(EvApprovalRequested); ok {
				approvalID = ar.ApprovalID
			}
		}
		time.Sleep(time.Millisecond)
	}
	b.ResolveApproval(approvalID, DecisionApproved)
	if got := <-done; !got {
		t.Fatal("approved confirm returned false")
	}
}

// TestBridgeApprovalEnrichment asserts the approval:requested event carries the
// request's display context: the risk class is passed through verbatim (so a
// per-confirm override survives — it is NOT re-derived from riskOf), the
// consequence is forwarded, and the raw args are redacted via redactArgs.
func TestBridgeApprovalEnrichment(t *testing.T) {
	c := &collector{}
	// riskOf returns a DIFFERENT class than the request carries, to prove the
	// emitted event uses the passed-through RiskClass, not the registry lookup.
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post, ApprovalTimeoutMs: 0,
		RiskOf: func(string) (domain.RiskClass, bool) { return domain.RiskLocal, true }})

	// A long value must collapse to a "<string: N chars>" marker (proves redaction
	// genuinely runs); a short value passes through verbatim — deliberate parity
	// with tool:started's wire redaction (redactArgs is length-only, not
	// credential-aware; the credential-masking redactor lives in the host path).
	longVal := strings.Repeat("a", 100)
	done := make(chan bool, 1)
	go func() {
		done <- b.Confirm(context.Background(), ConfirmRequest{
			ToolName:    "grant.create",
			Summary:     "create automation grant?",
			RiskClass:   domain.RiskSystem,
			Consequence: "grants unattended actor authority",
			RawArgs:     `{"scope":"git","payload":"` + longVal + `"}`,
		})
	}()

	var req *EvApprovalRequested
	deadline := time.After(time.Second)
	for req == nil {
		select {
		case <-deadline:
			t.Fatal("no approval:requested emitted")
		default:
		}
		for _, e := range c.snapshot() {
			if ar, ok := e.(EvApprovalRequested); ok {
				ar := ar
				req = &ar
			}
		}
		time.Sleep(time.Millisecond)
	}
	b.ResolveApproval(req.ApprovalID, DecisionApproved)
	<-done

	if req.RiskClass != domain.RiskSystem {
		t.Errorf("riskClass=%q want %q (passed through, not re-derived)", req.RiskClass, domain.RiskSystem)
	}
	if req.Consequence != "grants unattended actor authority" {
		t.Errorf("consequence=%q not forwarded", req.Consequence)
	}
	// redactArgs produces a single-level object: keys survive, the short value
	// passes through verbatim, and the long value is collapsed to a length marker
	// (never crossing the wire raw).
	if !strings.Contains(req.ArgsSummary, "scope") {
		t.Errorf("argsSummary=%q dropped the scope key", req.ArgsSummary)
	}
	if !strings.Contains(req.ArgsSummary, "<string: 100 chars>") {
		t.Errorf("argsSummary=%q want long value collapsed to a length marker", req.ArgsSummary)
	}
	if strings.Contains(req.ArgsSummary, longVal) {
		t.Errorf("argsSummary leaked the long value verbatim: %q", req.ArgsSummary)
	}

	// Encoded wire object must surface the three optional keys.
	raw, err := req.encode("s", 1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if obj["riskClass"] != "system" {
		t.Errorf("wire riskClass=%v want system", obj["riskClass"])
	}
	if obj["consequence"] != "grants unattended actor authority" {
		t.Errorf("wire consequence=%v", obj["consequence"])
	}
	if _, ok := obj["argsSummary"]; !ok {
		t.Error("wire argsSummary missing")
	}
}

// TestApprovalRequestedEncodeOmitsEmpty asserts the optional display fields are
// omitted from the wire object when empty (no empty strings / nulls leak).
func TestApprovalRequestedEncodeOmitsEmpty(t *testing.T) {
	raw, err := EvApprovalRequested{ApprovalID: "apr_1", ToolID: "git.push", Summary: "push?", RequestedAt: 5}.encode("s", 1)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"riskClass", "consequence", "argsSummary", "turnId"} {
		if _, ok := obj[k]; ok {
			t.Errorf("empty field %q must be omitted, got %v", k, obj[k])
		}
	}
}

func TestBridgeApprovalTimeout(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post, ApprovalTimeoutMs: 10})
	got := b.Confirm(context.Background(), ConfirmRequest{ToolName: "git.push", Summary: "push?"})
	if got {
		t.Fatal("timed-out confirm returned true")
	}
	// approval:decided with timeout must have been emitted.
	found := false
	for _, e := range c.snapshot() {
		if ad, ok := e.(EvApprovalDecided); ok && ad.Decision == DecisionTimeout {
			found = true
		}
	}
	if !found {
		t.Fatal("no timeout decision emitted")
	}
}

func TestBridgeSettlePendingApprovals(t *testing.T) {
	c := &collector{}
	b := NewBridge(BridgeOptions{SessionID: "s", Post: c.post, ApprovalTimeoutMs: 0})
	done := make(chan bool, 1)
	go func() {
		done <- b.Confirm(context.Background(), ConfirmRequest{ToolName: "git.push", Summary: "p"})
	}()
	// Let the approval register.
	time.Sleep(10 * time.Millisecond)
	b.SettlePendingApprovals(DecisionRejected)
	select {
	case got := <-done:
		if got {
			t.Fatal("rejected confirm returned true")
		}
	case <-time.After(time.Second):
		t.Fatal("settlePendingApprovals did not unblock confirm")
	}
}

func TestIsDanger(t *testing.T) {
	b := NewBridge(BridgeOptions{SessionID: "s", Post: func(HostEvent) {},
		RiskOf: func(name string) (domain.RiskClass, bool) {
			switch name {
			case "fs.read":
				return domain.RiskRead, true
			case "git.push":
				return domain.RiskGit, true
			}
			return "", false
		}})
	if b.isDanger("fs.read") {
		t.Error("read risk must not be danger")
	}
	if !b.isDanger("git.push") {
		t.Error("git risk must be danger")
	}
	if b.isDanger("unknown.tool") {
		t.Error("unknown tool must not be danger")
	}
}
