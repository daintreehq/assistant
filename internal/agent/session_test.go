package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// --- fakes ---

type fakeRouter struct {
	// results is the queued sequence of stream replies (one per round).
	results []models.ChatResult
	round   int
	streams [][]string // visible tokens to emit per round (parallel to results)
}

func (r *fakeRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	i := r.round
	r.round++
	if i < len(r.streams) && onToken != nil {
		for _, tok := range r.streams[i] {
			onToken(tok)
		}
	}
	if i >= len(r.results) {
		return models.ChatResult{Content: "done"}, nil
	}
	return r.results[i], nil
}
func (r *fakeRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "summary"}, nil
}
func (r *fakeRouter) ModelFor(tier domain.ModelTier) string { return "minimax-m3" }
func (r *fakeRouter) FlushMeter() []models.TierUsage        { return nil }

type fakeTools struct {
	// dispatch returns this result for every call (the repeat-failure scenario).
	result       domain.ToolResult
	dispatched   int
	readOnly     []string
	lastTurn     TurnContext
	dispatchSeen []string
	// emitProgress, when set, is invoked inside Dispatch to mimic a tool reporting an
	// in-tool substep — the registry's ReportProgress path (carried via turn.Progress).
	emitProgress []string
}

func (t *fakeTools) OpenAITools(filter []string) ([]models.ChatTool, error) { return nil, nil }
func (t *fakeTools) ResolveWireName(w string) string {
	return strings.ReplaceAll(w, "__", ".")
}
func (t *fakeTools) ReadOnlyToolNames() []string { return t.readOnly }
func (t *fakeTools) Dispatch(ctx context.Context, name, args string, turn TurnContext) domain.ToolResult {
	t.dispatched++
	t.lastTurn = turn
	t.dispatchSeen = append(t.dispatchSeen, name)
	if turn.Progress != nil {
		for _, msg := range t.emitProgress {
			turn.Progress(turn.CallID, msg)
		}
	}
	return t.result
}

type fakeSelector struct{ sel skills.SkillSelection }

func (s fakeSelector) Select(ctx context.Context, c []skills.SkillMetadata, q string) (skills.SkillSelection, error) {
	return s.sel, nil
}

type fakeCatalog struct{}

func (fakeCatalog) MetadataForSelection() []skills.SkillMetadata { return nil }
func (fakeCatalog) GetMany([]string) []skills.Skill              { return nil }
func (fakeCatalog) Has(string) bool                              { return false }

func baseDeps(r Router, tr ToolRunner) SessionDeps {
	return SessionDeps{
		Router:        r,
		Tools:         tr,
		SkillSelector: fakeSelector{},
		SkillCatalog:  fakeCatalog{},
		Store:         nil, // best-effort persistence; nil exercises the nil-guard
		SessionID:     "sess_test",
		Events:        NoopEventSink{},
	}
}

// captureSink records think separation + tool-state promotions for assertions.
type captureSink struct {
	NoopEventSink
	endContent   string
	endReasoning string
	states       []string
	batched      int
	progress     []progressBeat
}

// progressBeat records one ToolProgress(callID, msg) the session emitted.
type progressBeat struct {
	id  string
	msg string
}

func (c *captureSink) AssistantEnd(content, reasoning string) {
	c.endContent = content
	c.endReasoning = reasoning
}
func (c *captureSink) ToolBatch(calls []BatchedToolCall) { c.batched = len(calls) }
func (c *captureSink) ToolState(id string, st ToolState) { c.states = append(c.states, string(st)) }
func (c *captureSink) ToolProgress(id, msg string) {
	c.progress = append(c.progress, progressBeat{id: id, msg: msg})
}

func toolCall(id, wireName, args string) models.ToolCallRequest {
	return models.ToolCallRequest{ID: id, Type: "function",
		Function: models.ToolCallFunction{Name: wireName, Arguments: args}}
}

// --- tests ---

func TestThinkSeparationOnFinalAnswer(t *testing.T) {
	sink := &captureSink{}
	// Final round: visible content + reasoning (the router already split <think>).
	r := &fakeRouter{
		results: []models.ChatResult{{Content: "the visible answer", Reasoning: "the hidden plan"}},
	}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "the visible answer" {
		t.Fatalf("reply = %q", reply)
	}
	if sink.endContent != "the visible answer" || sink.endReasoning != "the hidden plan" {
		t.Fatalf("think not separated: content=%q reasoning=%q", sink.endContent, sink.endReasoning)
	}
}

func TestRepeatedFailureBreakerAborts(t *testing.T) {
	// Every round returns the SAME failing tool call with byte-identical args; the
	// breaker should abort at the 3rd identical failure with a "Stopped: called " reply.
	failing := domain.Fail("BOOM", "it broke", domain.Unrecoverable())
	tools := &fakeTools{result: failing}
	// Provide enough rounds (each emits the identical failing call).
	rounds := make([]models.ChatResult, 6)
	for i := range rounds {
		rounds[i] = models.ChatResult{ToolCalls: []models.ToolCallRequest{toolCall("c", "fs__read", `{"path":"x"}`)}}
	}
	r := &fakeRouter{results: rounds}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Stopped: called ") {
		t.Fatalf("expected circuit-breaker abort, got %q", reply)
	}
	if !IsWakeFailureReply(reply) {
		t.Fatal("breaker reply must be a wake-failure sentinel")
	}
	// 3 identical failures abort: dispatched 3 times (one per round before abort).
	if tools.dispatched != 3 {
		t.Fatalf("dispatched %d times, want 3 (abort at 3rd identical failure)", tools.dispatched)
	}
}

func TestWakeReadOnlyRefusesDisallowedTool(t *testing.T) {
	// Read-only turn: allowedSet = {fs.read}. A call to terminal.write (not in the
	// set) must be refused with READ_ONLY_TURN and NEVER reach Dispatch.
	tools := &fakeTools{readOnly: []string{"fs.read"}, result: domain.Ok("ok", nil)}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c1", "terminal__write", `{}`)}},
			{Content: "done after refusal"},
		},
	}
	s := NewSession(baseDeps(r, tools))

	reply, err := s.Send(context.Background(), "wake", SendOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done after refusal" {
		t.Fatalf("reply = %q", reply)
	}
	if tools.dispatched != 0 {
		t.Fatalf("disallowed tool reached Dispatch %d times, want 0", tools.dispatched)
	}
	// The tool reply pushed to history must carry the READ_ONLY_TURN refusal.
	var refusalSeen bool
	for _, m := range s.Messages() {
		if m.Role == "tool" && strings.Contains(m.StringContent, "READ_ONLY_TURN") {
			refusalSeen = true
		}
	}
	if !refusalSeen {
		t.Fatal("expected a READ_ONLY_TURN refusal tool result in history")
	}
}

func TestToolBatchAnnouncedBeforeDispatch(t *testing.T) {
	sink := &captureSink{}
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{
				toolCall("a", "fs__read", `{}`),
				toolCall("b", "fs__list", `{}`),
			}},
			{Content: "final"},
		},
	}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if sink.batched != 2 {
		t.Fatalf("ToolBatch announced %d calls, want 2", sink.batched)
	}
	// Each call promotes active then done → at least 4 state transitions.
	active, done := 0, 0
	for _, st := range sink.states {
		switch st {
		case "active":
			active++
		case "done":
			done++
		}
	}
	if active != 2 || done != 2 {
		t.Fatalf("state promotions active=%d done=%d, want 2/2", active, done)
	}
}

func TestToolProgressForwardedToSink(t *testing.T) {
	// A tool reporting an in-tool substep (via the registry's ReportProgress, carried
	// through turn.Progress) must surface as a ToolProgress(callID, msg) event tagged
	// with the active call's id.
	sink := &captureSink{}
	tools := &fakeTools{result: domain.Ok("ok", nil), emitProgress: []string{"launching terminal", ""}}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("call-1", "agentTask__spawnForEdits", `{}`)}},
			{Content: "final"},
		},
	}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	// The empty-message beat is dropped by the session forwarder; only the real
	// substep reaches the sink, tagged with the dispatched call's id.
	if len(sink.progress) != 1 {
		t.Fatalf("ToolProgress emitted %d times, want 1 (empty beats dropped): %+v", len(sink.progress), sink.progress)
	}
	if sink.progress[0].id != "call-1" || sink.progress[0].msg != "launching terminal" {
		t.Fatalf("progress beat = %+v, want {id:call-1 msg:launching terminal}", sink.progress[0])
	}
}

func TestSingleFlightConcurrentSend(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	// Force inFlight to simulate a concurrent turn.
	s.inFlight = true
	if _, err := s.Send(context.Background(), "x", SendOptions{}); err != ErrTurnInProgress {
		t.Fatalf("expected ErrTurnInProgress, got %v", err)
	}
}

func TestCancelBeforeWorkLeavesNoOrphan(t *testing.T) {
	r := &fakeRouter{results: []models.ChatResult{{Content: "should not run"}}}
	s := NewSession(baseDeps(r, &fakeTools{}))
	before := len(s.Messages())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reply, err := s.Send(ctx, "cancelled input", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != domain.CancelledReply {
		t.Fatalf("reply = %q want %q", reply, domain.CancelledReply)
	}
	if len(s.Messages()) != before {
		t.Fatal("cancelled turn must not push the user message (no orphan turn)")
	}
}
