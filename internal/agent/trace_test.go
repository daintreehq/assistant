package agent

import (
	"context"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// traceEvent is one captured (event, fields) pair from the Trace seam.
type traceEvent struct {
	event  string
	fields map[string]any
}

// traceCapture is a non-concurrent recorder for the Trace seam. Send runs the turn
// synchronously on the caller's goroutine, so the slice needs no lock.
type traceCapture struct{ events []traceEvent }

func (c *traceCapture) record(event string, fields map[string]any) {
	c.events = append(c.events, traceEvent{event: event, fields: fields})
}

// only returns every captured event with the given name, in order.
func (c *traceCapture) only(name string) []traceEvent {
	var out []traceEvent
	for _, e := range c.events {
		if e.event == name {
			out = append(out, e)
		}
	}
	return out
}

// first returns the first captured event with the given name (or false).
func (c *traceCapture) first(name string) (traceEvent, bool) {
	for _, e := range c.events {
		if e.event == name {
			return e, true
		}
	}
	return traceEvent{}, false
}

// TestTraceTurnAndBackendRoundsEmitted verifies a normal tool-using turn produces the
// turn lifecycle bracket plus one backend.respond.{request,meta,done} per round — the
// trace coverage the backend migration removed (acceptance criterion #1: a one-turn
// answer yields one turn.start, the round events, and one turn.end).
func TestTraceTurnAndBackendRoundsEmitted(t *testing.T) {
	cap := &traceCapture{}
	// Round 0 requests a tool; round 1 returns the final answer.
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("call_1", "fs__read", `{"path":"x"}`)}},
		{Content: "all done"},
	}}
	tools := &fakeTools{result: domain.Ok("read it", nil)}
	deps := baseDeps(r, tools)
	deps.Trace = cap.record
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "do the thing", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "all done" {
		t.Fatalf("reply = %q want %q", reply, "all done")
	}

	// Exactly one turn.start and one turn.end.
	if got := len(cap.only("turn.start")); got != 1 {
		t.Fatalf("turn.start count = %d want 1", got)
	}
	ends := cap.only("turn.end")
	if len(ends) != 1 {
		t.Fatalf("turn.end count = %d want 1", len(ends))
	}
	end := ends[0]
	if end.fields["status"] != "complete" {
		t.Errorf("turn.end status = %v want complete", end.fields["status"])
	}
	if end.fields["rounds"] != 2 {
		t.Errorf("turn.end rounds = %v want 2", end.fields["rounds"])
	}

	// turnId is shared across start, end, and every round event.
	start, _ := cap.first("turn.start")
	turnID, _ := start.fields["turnId"].(string)
	if turnID == "" {
		t.Fatal("turn.start carried no turnId")
	}
	if start.fields["sessionId"] != "sess_test" {
		t.Errorf("turn.start sessionId = %v want sess_test", start.fields["sessionId"])
	}
	if end.fields["turnId"] != turnID {
		t.Errorf("turn.end turnId = %v want %v", end.fields["turnId"], turnID)
	}

	// Two backend rounds, numbered 0 and 1, each with a request/meta/done.
	reqs := cap.only("backend.respond.request")
	if len(reqs) != 2 {
		t.Fatalf("backend.respond.request count = %d want 2", len(reqs))
	}
	for i, ev := range reqs {
		if ev.fields["round"] != i {
			t.Errorf("request[%d] round = %v want %d", i, ev.fields["round"], i)
		}
		if ev.fields["turnId"] != turnID {
			t.Errorf("request[%d] turnId = %v want %v", i, ev.fields["turnId"], turnID)
		}
		input, ok := ev.fields["input"].(map[string]any)
		if !ok {
			t.Fatalf("request[%d] input not a map: %T", i, ev.fields["input"])
		}
		if _, ok := input["messageRoles"].([]string); !ok {
			t.Errorf("request[%d] input.messageRoles missing/wrong type", i)
		}
	}
	if got := len(cap.only("backend.respond.meta")); got != 2 {
		t.Errorf("backend.respond.meta count = %d want 2", got)
	}
	dones := cap.only("backend.respond.done")
	if len(dones) != 2 {
		t.Fatalf("backend.respond.done count = %d want 2", len(dones))
	}
	// Round 0 asked for one tool call; round 1 produced the final content.
	if dones[0].fields["toolCallCount"] != 1 {
		t.Errorf("done[0] toolCallCount = %v want 1", dones[0].fields["toolCallCount"])
	}
	if dones[1].fields["contentPreview"] != "all done" {
		t.Errorf("done[1] contentPreview = %v want %q", dones[1].fields["contentPreview"], "all done")
	}
}

// TestTraceInvalidToolArgsEmitted verifies a tool call whose arguments are not valid
// JSON — a rejection that never reaches the registry's Dispatch and so produces no
// tool.call audit event — is still surfaced via tool.args.invalid.
func TestTraceInvalidToolArgsEmitted(t *testing.T) {
	cap := &traceCapture{}
	r := &fakeRouter{results: []models.ChatResult{
		{ToolCalls: []models.ToolCallRequest{toolCall("call_bad", "fs__read", `{not json`)}},
		{Content: "stopped"},
	}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("unused", nil)})
	deps.Trace = cap.record
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	ev, ok := cap.first("tool.args.invalid")
	if !ok {
		t.Fatal("expected a tool.args.invalid trace event")
	}
	if ev.fields["tool"] != "fs.read" {
		t.Errorf("tool = %v want fs.read", ev.fields["tool"])
	}
	if ev.fields["toolCallId"] != "call_bad" {
		t.Errorf("toolCallId = %v want call_bad", ev.fields["toolCallId"])
	}
}

// TestTraceNilSeamIsNoop verifies a session with no Trace seam wired (the default,
// and every test that doesn't opt in) runs a turn without error — the seam is purely
// additive and a nil seam must be a silent no-op.
func TestTraceNilSeamIsNoop(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "hi"}}}
	deps := baseDeps(r, &fakeTools{result: domain.Ok("x", nil)})
	deps.Trace = nil
	s := NewSession(deps)
	reply, err := s.Send(context.Background(), "hello", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hi" {
		t.Fatalf("reply = %q want hi", reply)
	}
}

// TestTurnStatusClassification pins the reply→status mapping the turn.end event uses.
func TestTurnStatusClassification(t *testing.T) {
	cases := []struct {
		reply string
		want  string
	}{
		{"a normal answer", "complete"},
		{domain.CancelledReply, "cancelled"},
		{"Model unavailable: backend down", "failed"},
		{"Tool projection failed: boom", "failed"},
		{"Stopped: called fs.read 3 times", "failed"},
	}
	for _, c := range cases {
		if got := turnStatus(c.reply); got != c.want {
			t.Errorf("turnStatus(%q) = %q want %q", c.reply, got, c.want)
		}
	}
}
