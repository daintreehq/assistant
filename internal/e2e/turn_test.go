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
func (r *recordingSink) Interjection(string)               { r.log("user:interjection") }
func (r *recordingSink) SkillLoaded([]string)              { r.log("skill:loaded") }
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
func (r *recordingSink) Warn(m string)          { r.log("warn:" + m) }
func (r *recordingSink) Info(string)            {}
func (r *recordingSink) Usage(agent.UsageEvent) { r.log("usage") }
func (r *recordingSink) TurnPrompt(string)      {}
func (r *recordingSink) ModelRateLimited()      { r.log("model-rate-limited") }

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
// Daintree backend SSE server: round 1 streams prose + a memory.list tool call; round 2
// streams the final answer. It asserts the streamed event order, that the tool was
// dispatched and its result fed back into round 2, that the conversation was
// persisted, the run-event log written, and the RunPhase lifecycle reached a
// terminal phase.
func TestFullTurnAgainstFakes(t *testing.T) {
	fake := newFakeBackend(t,
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

	// Point the backend client at the fake via the DAINTREE_BACKEND_URL env override
	// (app.Create reads it directly as the dev/test endpoint hook). app.Create builds a
	// real backend.Client to the fake, so this exercises the live SSE parse path too.
	t.Setenv("DAINTREE_BACKEND_URL", fake.baseURL())
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	key := "test-key"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:       &dir,
			ProjectPath:    &dir,
			Tier:           strPtr("operator"),
			DeepSeekAPIKey: &key,
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

// flashModelID is the validated orchestration model id all tiers default to. The
// wiring test below pins it so the large tier can never silently revert to a heavier
// model: flash is the validated main-thread model (see config.DEFAULTS).
const flashModelID = "deepseek-v4-flash"

// countToolRoleMessages returns how many role:"tool" feedback messages a request
// carried — i.e. how many prior tool results were folded back into that round.
func countToolRoleMessages(msgs []map[string]any) int {
	n := 0
	for _, m := range msgs {
		if role, _ := m["role"].(string); role == "tool" {
			n++
		}
	}
	return n
}

// TestOrchestratorMultiStep validates that the orchestrator (the main thread) drives a
// genuinely multi-step load against the backend — not a single relay. The fake backend
// is scripted and deterministic, so it cannot test reasoning *quality*; what it proves
// is the wiring of the orchestration deduction loop: a three-round chained-tool scenario
// — list memories, then recall by query (a second lookup conceptually conditioned on the
// first), then conclude — drives to completion with each tool result fed back into the
// next backend round. That feedback chain is exactly the orchestration loop: the model
// must integrate prior tool output before its next move, not echo it.
//
// NOTE: the migration moved model routing and the think-on/off decision INTO the backend
// — the CLI no longer chooses the model id or sets a `thinking`/`reasoning_effort` field
// on the wire (the request carries only the visible conversation + structured context).
// So the old per-round wire assertions on model id / reasoning_effort / thinking are gone
// (they exercised removed client-side behavior). The config.DEFAULTS.LargeModel guard
// survives as a cheap check that the legacy default is still the validated flash model.
func TestOrchestratorMultiStep(t *testing.T) {
	// Guard the legacy default at its source: the configured large-tier default IS flash
	// (used now only by the transitional Router seam, never the turn loop).
	if config.DEFAULTS.LargeModel != flashModelID {
		t.Fatalf("config.DEFAULTS.LargeModel = %q, want the validated orchestration model %q",
			config.DEFAULTS.LargeModel, flashModelID)
	}

	fake := newFakeBackend(t,
		// Round 1: orchestrator opens the deduction — list what's in memory.
		sseRound{
			contentTokens: []string{"Step 1: ", "list memory."},
			toolName:      "memory__list",
			toolArgs:      `{"limit":3}`,
			usage:         &fakeUsage{prompt: 100, completion: 10, total: 110, cached: 0},
		},
		// Round 2: conditioned on round 1, a second dependent lookup — recall by query.
		sseRound{
			contentTokens: []string{"Step 2: ", "recall by query."},
			toolName:      "memory__recall",
			toolArgs:      `{"query":"test"}`,
			usage:         &fakeUsage{prompt: 130, completion: 10, total: 140, cached: 40},
		},
		// Round 3: both results integrated, the orchestrator concludes.
		sseRound{
			contentTokens: []string{"Conclusion: ", "deduced."},
			usage:         &fakeUsage{prompt: 160, completion: 8, total: 168, cached: 80},
		},
	)

	t.Setenv("DAINTREE_BACKEND_URL", fake.baseURL())
	t.Setenv("DAINTREE_ASSISTANT_DEBUG_LOG", "0")

	dir := t.TempDir()
	key := "test-key"
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			StateDir:       &dir,
			ProjectPath:    &dir,
			Tier:           strPtr("operator"),
			DeepSeekAPIKey: &key,
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	defer a.Shutdown()

	sink := &recordingSink{}
	a.SetHooks(app.AppHooks{AgentEvents: sink})

	reply, serr := a.Session.Send(context.Background(), "deduce what's worth remembering", agent.SendOptions{})
	if serr != nil {
		t.Fatalf("Send returned error: %v", serr)
	}
	if !strings.Contains(reply, "deduced") {
		t.Errorf("final reply = %q, want it to contain the round-3 conclusion", reply)
	}

	// Three backend rounds: two chained tool rounds + the final-answer round.
	if got := fake.callCount(); got != 3 {
		t.Fatalf("backend called %d times, want 3 (two chained tool rounds + final)", got)
	}

	// --- the local tool inventory reached the backend on every round ---
	// The CLI ships its function-tool specs as input.tools; the backend never invents
	// them. A non-empty inventory each round proves the projection was wired through.
	for i := 0; i < 3; i++ {
		if got := fake.requestTools(i); len(got) == 0 {
			t.Errorf("round %d carried no tools in the backend request (input.tools empty)", i+1)
		}
	}

	// --- two distinct tools were dispatched in order, both settled ok ---
	if len(sink.calls) != 2 ||
		sink.calls[0].Name != "memory.list" || sink.calls[1].Name != "memory.recall" {
		t.Fatalf("tool calls = %+v, want [memory.list, memory.recall] in order", sink.calls)
	}
	if len(sink.results) != 2 {
		t.Fatalf("tool results = %+v, want 2 (one per dispatched tool)", sink.results)
	}
	for i, res := range sink.results {
		if !res.Result.Ok {
			t.Errorf("tool result %d (%s) not ok: %+v", i, res.Name, res.Result)
		}
	}

	// --- the deduction chain: each round folds back the PRIOR round's tool result ---
	// Round 2 sees round-1's list result (1 tool message); round 3 sees both (>= 2).
	if got := countToolRoleMessages(fake.requestMessages(1)); got < 1 {
		t.Errorf("round-2 request carried %d tool-role messages, want >= 1 (round-1 result fed back)", got)
	}
	if got := countToolRoleMessages(fake.requestMessages(2)); got < 2 {
		t.Errorf("round-3 request carried %d tool-role messages, want >= 2 (both results fed back)", got)
	}
	// Content check: round 3 must carry the round-2 recall result specifically, proving
	// the second (dependent) lookup's output was integrated — not just the first echoed.
	if !containsToolMessage(fake.requestMessages(2), sink.results[1].Result.Summary) {
		t.Errorf("round-3 request did not fold back the memory.recall result %q: %v",
			sink.results[1].Result.Summary, fake.requestMessages(2))
	}
}

func strPtr(s string) *string { return &s }
