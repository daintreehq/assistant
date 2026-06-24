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
	rateLimited  bool
	warnings     []string
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
func (c *captureSink) ModelRateLimited() { c.rateLimited = true }
func (c *captureSink) Warn(msg string)   { c.warnings = append(c.warnings, msg) }

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

func TestLoadedSkillNeverLimitsCallableTools(t *testing.T) {
	// A loaded skill must NEVER limit which tools the model can call: skills are
	// guidance, not a capability gate. With a skill active, a call to a tool that is
	// NOT in that skill's requiredTools (terminal.summarize) must still reach Dispatch
	// and succeed — no TOOL_NOT_OFFERED refusal. The full registry is offered on every
	// turn regardless of loaded skills.
	tools := &fakeTools{result: domain.Ok("ok", nil)}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c1", "terminal__summarize", `{}`)}},
			{Content: "done"},
		},
	}
	s := skillSession(t, realRegistry(t), r, tools)
	// Load a skill whose requiredTools does NOT include terminal.summarize.
	s.mu.Lock()
	s.activeSkills = []string{skills.IDSpawnAgentForEdits}
	s.mu.Unlock()

	reply, err := s.Send(context.Background(), "go", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reply != "done" {
		t.Fatalf("reply = %q", reply)
	}
	if tools.dispatched != 1 {
		t.Fatalf("a tool outside the skill's requiredTools must still dispatch; got %d dispatches", tools.dispatched)
	}
	// No TOOL_NOT_OFFERED refusal may appear — a skill never makes a tool uncallable.
	for _, m := range s.Messages() {
		if m.Role == "tool" && strings.Contains(m.StringContent, "TOOL_NOT_OFFERED") {
			t.Fatal("a loaded skill must NOT refuse a tool with TOOL_NOT_OFFERED")
		}
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

func TestClassifyRateLimitedStreamError(t *testing.T) {
	// A streamed RateLimitedError (retry budget exhausted on a 429) classifies to a
	// byte-stable "Model rate-limited:" reply, fires the ModelRateLimited health cue,
	// and is a wake-failure sentinel so a background wake won't record it as a result.
	sink := &captureSink{}
	r := &errRouter{err: &models.RateLimitedError{Message: "provider quota/throughput exceeded", RetryAfterMs: 1500}}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)

	reply, err := s.Send(context.Background(), "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(reply, "Model rate-limited:") {
		t.Fatalf("reply = %q, want a \"Model rate-limited:\" prefix", reply)
	}
	if !IsWakeFailureReply(reply) {
		t.Fatalf("reply %q must be a wake-failure sentinel", reply)
	}
	if !sink.rateLimited {
		t.Fatal("ModelRateLimited health cue was not fired")
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

// TestRehydrateDropNoteEmitsOnceOnFirstTurn proves the resume-corruption note:
// when NewSession is handed a non-zero DroppedRehydrateRows, the session emits
// exactly one Info event — on the first turn, after TurnPrompt — and never again
// on subsequent turns.
func TestRehydrateDropNoteEmitsOnceOnFirstTurn(t *testing.T) {
	sink := &recordingSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}, {Content: "done"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	deps.DroppedRehydrateRows = 2
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "first", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Send(context.Background(), "second", SendOptions{}); err != nil {
		t.Fatal(err)
	}

	var infos []string
	for _, e := range sink.log {
		if strings.HasPrefix(e, "info:") {
			infos = append(infos, e)
		}
	}
	if len(infos) != 1 {
		t.Fatalf("info events = %v want exactly 1 (emit once across two turns)", infos)
	}
	if !strings.Contains(infos[0], "2 malformed or orphan") {
		t.Fatalf("info note = %q want it to report the 2 dropped rows", infos[0])
	}
	// The note must follow the first turn's prompt, not precede it.
	promptIdx, infoIdx := -1, -1
	for i, e := range sink.log {
		if promptIdx == -1 && strings.HasPrefix(e, "prompt:") {
			promptIdx = i
		}
		if infoIdx == -1 && strings.HasPrefix(e, "info:") {
			infoIdx = i
		}
	}
	if !(promptIdx >= 0 && infoIdx > promptIdx) {
		t.Fatalf("info note (idx %d) must come after the first TurnPrompt (idx %d); log=%v", infoIdx, promptIdx, sink.log)
	}
}

// TestNoRehydrateDropNoteWhenZero confirms a clean resume (zero dropped rows)
// emits no info note at all.
func TestNoRehydrateDropNoteWhenZero(t *testing.T) {
	sink := &recordingSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "done"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	// DroppedRehydrateRows left at 0.
	s := NewSession(deps)

	if _, err := s.Send(context.Background(), "go", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, e := range sink.log {
		if strings.HasPrefix(e, "info:") {
			t.Fatalf("unexpected info note on a clean resume: %q", e)
		}
	}
}
