package e2e

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// recordingSink captures the ordered event stream the session emits so a test can
// assert the lifecycle (phases, assistant start/token/end, tool call/result).
type recordingSink struct {
	mu      sync.Mutex
	order   []string // human-readable event log, in emission order
	phases  []domain.RunPhase
	tokens  []string
	endText string
	calls   []agent.ToolCallEvent
	results []agent.ToolResultEvent
}

func (r *recordingSink) log(s string) {
	r.mu.Lock()
	r.order = append(r.order, s)
	r.mu.Unlock()
}

func (r *recordingSink) Phase(p domain.RunPhase) {
	r.mu.Lock()
	r.phases = append(r.phases, p)
	r.mu.Unlock()
	r.log("phase:" + p.String())
}
func (r *recordingSink) AssistantStart() { r.log("assistant:start") }
func (r *recordingSink) AssistantToken(t string) {
	r.mu.Lock()
	r.tokens = append(r.tokens, t)
	r.mu.Unlock()
	r.log("assistant:token")
}
func (r *recordingSink) AssistantEnd(c, _ string) {
	r.mu.Lock()
	r.endText = c
	r.mu.Unlock()
	r.log("assistant:end")
}
func (r *recordingSink) AssistantCancelled(string)         { r.log("assistant:cancelled") }
func (r *recordingSink) ToolBatch([]agent.BatchedToolCall) { r.log("tool:batch") }
func (r *recordingSink) ToolState(string, agent.ToolState) {}
func (r *recordingSink) ToolProgress(string, string)       {}
func (r *recordingSink) ToolCall(ev agent.ToolCallEvent) {
	r.mu.Lock()
	r.calls = append(r.calls, ev)
	r.mu.Unlock()
	r.log("tool:call:" + ev.Name)
}
func (r *recordingSink) ToolResult(ev agent.ToolResultEvent) {
	r.mu.Lock()
	r.results = append(r.results, ev)
	r.mu.Unlock()
	r.log("tool:result:" + ev.Name)
}
func (r *recordingSink) Error(m string)         { r.log("error:" + m) }
func (r *recordingSink) Info(string)            {}
func (r *recordingSink) Usage(agent.UsageEvent) { r.log("usage") }
func (r *recordingSink) TurnPrompt(string)      {}

func (r *recordingSink) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.order))
	copy(out, r.order)
	return out
}

// indexOf returns the first index of s in the event log, or -1.
func indexOf(log []string, s string) int {
	for i, v := range log {
		if v == s {
			return i
		}
	}
	return -1
}

// TestFullTurnAgainstFakes drives a complete agent turn end-to-end against the fake
// Fireworks SSE server: round 1 streams prose + a memory.list tool call; round 2
// streams the final answer. It asserts the streamed event order, that the tool was
// dispatched and its result fed back into round 2, that the conversation was
// persisted, the run-event log written, and the RunPhase lifecycle reached a
// terminal phase.
func TestFullTurnAgainstFakes(t *testing.T) {
	fake := newFakeFireworks(t,
		// Round 1: a little prose, then a tool call to the local read-only memory.list.
		sseRound{
			contentTokens: []string{"Let me ", "check."},
			toolName:      "memory__list",
			toolArgs:      `{"limit":5}`,
			usage:         &fakeUsage{prompt: 100, completion: 12, total: 112, cached: 40},
		},
		// Round 2: the final answer after the tool result is fed back.
		sseRound{
			contentTokens: []string{"All ", "done."},
			usage:         &fakeUsage{prompt: 130, completion: 8, total: 138, cached: 80},
		},
	)

	// Point the model client at the fake via the FIREWORKS_BASE_URL env override
	// (the config trusted-env boundary reads it). A non-empty API key clears the
	// offline/missing-key guard.
	t.Setenv("FIREWORKS_BASE_URL", fake.baseURL())
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	key := "test-key"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:        &dir,
			ProjectPath:     &dir,
			Tier:            strPtr("operator"),
			FireworksAPIKey: &key,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	sink := &recordingSink{}
	a.SetHooks(app.AppHooks{AgentEvents: sink})

	reply, serr := a.Session.Send(context.Background(), "what's in memory?", agent.SendOptions{})
	if serr != nil {
		t.Fatalf("Send returned error: %v", serr)
	}
	if !strings.Contains(reply, "All done.") {
		t.Errorf("final reply = %q, want it to contain the round-2 answer", reply)
	}

	// Two model rounds: the tool-call round and the final-answer round.
	if got := fake.callCount(); got != 2 {
		t.Fatalf("model called %d times, want 2 (tool round + final round)", got)
	}

	// --- streamed event order ---
	log := sink.snapshot()
	startIdx := indexOf(log, "assistant:start")
	callIdx := indexOf(log, "tool:call:memory.list")
	resultIdx := indexOf(log, "tool:result:memory.list")
	endIdx := indexOf(log, "assistant:end")
	if startIdx < 0 || callIdx < 0 || resultIdx < 0 || endIdx < 0 {
		t.Fatalf("missing core events in order log: %v", log)
	}
	if !(startIdx < callIdx && callIdx < resultIdx && resultIdx < endIdx) {
		t.Errorf("event order wrong: start=%d call=%d result=%d end=%d\n%v",
			startIdx, callIdx, resultIdx, endIdx, log)
	}

	// --- a tool call was dispatched and settled ok ---
	if len(sink.calls) != 1 || sink.calls[0].Name != "memory.list" {
		t.Fatalf("tool calls = %+v, want one memory.list", sink.calls)
	}
	if len(sink.results) != 1 || !sink.results[0].Result.Ok {
		t.Fatalf("tool results = %+v, want one ok result", sink.results)
	}

	// --- the tool result was fed back into round 2 ---
	round2 := fake.requestMessages(1)
	if !containsToolMessage(round2, sink.results[0].Result.Summary) {
		// The summary text should appear in the tool-role feedback message.
		hasToolRole := false
		for _, m := range round2 {
			if role, _ := m["role"].(string); role == "tool" {
				hasToolRole = true
			}
		}
		if !hasToolRole {
			t.Errorf("round-2 request carried no tool-role feedback message: %v", round2)
		}
	}

	// --- RunPhase lifecycle reached a terminal phase ---
	sawTerminal := false
	for _, p := range sink.phases {
		if p.IsTerminal() {
			sawTerminal = true
		}
	}
	if !sawTerminal {
		t.Errorf("no terminal RunPhase emitted; phases=%v", sink.phases)
	}

	// --- conversation persisted (controls + user + assistant tool-call turn + tool
	// result + final assistant) ---
	rows, lerr := a.Store.ListMessages(a.SessionID)
	if lerr != nil {
		t.Fatalf("ListMessages: %v", lerr)
	}
	var sawUser, sawTool, sawAssistant bool
	for _, row := range rows {
		switch row.Role {
		case "user":
			if strings.Contains(row.Content, "what's in memory?") {
				sawUser = true
			}
		case "tool":
			sawTool = true
		case "assistant":
			sawAssistant = true
		}
	}
	if !sawUser || !sawTool || !sawAssistant {
		t.Errorf("conversation not fully persisted: user=%v tool=%v assistant=%v (rows=%d)",
			sawUser, sawTool, sawAssistant, len(rows))
	}

	// --- run-event log written under a single run id ---
	runs, rerr := a.Store.ListRuns(10)
	if rerr != nil {
		t.Fatalf("ListRuns: %v", rerr)
	}
	if len(runs) != 1 {
		t.Fatalf("ListRuns = %d runs, want 1", len(runs))
	}
	events, eerr := a.Store.ListRunEvents(runs[0].RunID)
	if eerr != nil {
		t.Fatalf("ListRunEvents: %v", eerr)
	}
	wantTypes := map[string]bool{"tool:call": false, "tool:result": false, "assistant:end": false}
	lastSeq := -1
	for _, ev := range events {
		if ev.Seq <= lastSeq {
			t.Errorf("run-event seq not monotonic: %d after %d", ev.Seq, lastSeq)
		}
		lastSeq = ev.Seq
		if _, ok := wantTypes[ev.Type]; ok {
			wantTypes[ev.Type] = true
		}
	}
	for typ, seen := range wantTypes {
		if !seen {
			t.Errorf("run-event log missing %q row", typ)
		}
	}
}

func strPtr(s string) *string { return &s }
