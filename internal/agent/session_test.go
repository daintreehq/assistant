package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
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

// backendFromRouter adapts a legacy *Router fake to the AssistantBackend seam so the
// existing Router-style fakes drive the new backend-based turn loop unchanged.
// RespondStream delegates to Router.Stream; RunTask translates the checkpoint and
// memory-distill utility tasks back onto Router.Chat (the small-model call the fakes
// script). The agent loop no longer talks to a Router at all — this adapter is the
// test-only bridge that keeps the per-fake scripting (Stream for the turn, Chat for the
// compaction summary / distill reply) meaningful against the new seam.
type backendFromRouter struct {
	r Router
}

// toBackendToolCalls is the inverse of production's backendToolCalls: it lifts the
// local ToolCallRequest shape the fakes emit into the backend wire shape the loop reads
// off RespondResult.Message.
func toBackendToolCalls(calls []models.ToolCallRequest) []backend.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]backend.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, backend.ToolCall{
			ID:       c.ID,
			Type:     "function",
			Function: backend.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments},
		})
	}
	return out
}

// usageFromChat maps the Router's pointer-field usage to the backend's flat Usage so a
// fake that reports usage on its ChatResult flows through emitBackendUsage.
func usageFromChat(u *models.Usage) backend.Usage {
	if u == nil {
		return backend.Usage{}
	}
	return backend.Usage{
		PromptTokens:     derefp(u.PromptTokens),
		CompletionTokens: derefp(u.CompletionTokens),
		TotalTokens:      derefp(u.TotalTokens),
		CachedTokens:     derefp(u.CachedTokens),
	}
}

// backendMessagesToModel is the inverse of production's toBackendMessages: it decodes the
// backend wire conversation back into the local ChatMessage shape and forwards it to the
// wrapped Router as opts.Messages — so a message-snapshot fake (injectRouter.seen) still
// observes the exact per-round visible history the turn loop shipped, even though the loop
// now talks to a backend and not a Router.
func backendMessagesToModel(bms []backend.Message) []models.ChatMessage {
	out := make([]models.ChatMessage, 0, len(bms))
	for _, bm := range bms {
		m := models.ChatMessage{Role: bm.Role, ToolCallID: bm.ToolCallID, Name: bm.Name}
		switch {
		case len(bm.Content) == 0:
			// no content field
		case string(bm.Content) == "null":
			m.ContentNull = true
		default:
			var s string
			if err := json.Unmarshal(bm.Content, &s); err == nil {
				m.StringContent = s
			} else {
				// A multimodal parts array (rare in these tests) — keep the raw JSON so the
				// snapshot is non-empty rather than dropping it.
				m.StringContent = string(bm.Content)
			}
		}
		for _, tc := range bm.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, models.ToolCallRequest{
				ID: tc.ID, Type: tc.Type,
				Function: models.ToolCallFunction{Name: tc.Function.Name, Arguments: tc.Function.Arguments},
			})
		}
		out = append(out, m)
	}
	return out
}

func (b backendFromRouter) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	model := b.r.ModelFor(domain.ModelLarge)
	meta := backend.StreamMeta{Model: model, State: "dst1.test"}
	if cb.OnRawMeta != nil {
		cb.OnRawMeta(meta)
	}
	if cb.OnMeta != nil {
		cb.OnMeta(meta)
	}
	res, err := b.r.Stream(ctx, domain.ModelLarge,
		models.ChatOptions{Messages: backendMessagesToModel(req.Input.Messages)}, cb.OnContent)
	if err != nil {
		// A cancellation (explicit or via the ctx) reads as a clean stop. Everything
		// else — including a *backend.Error a fake router wants classified as a rate
		// limit — passes through untouched, so classifyBackendError sees exactly what
		// the real backend client would hand it.
		if errors.Is(err, context.Canceled) || ctx.Err() != nil {
			return backend.RespondResult{}, context.Canceled
		}
		return backend.RespondResult{}, err
	}
	return backend.RespondResult{
		Meta:         meta,
		Message:      backend.RespondMessage{Content: res.Content, ToolCalls: toBackendToolCalls(res.ToolCalls)},
		FinishReason: res.FinishReason,
		Usage:        usageFromChat(res.Usage),
	}, nil
}

// RunTask routes the two utility tasks the agent loop runs (checkpoint +
// memory_distill) back onto the Router's Chat call, so a fake scripting Chat to
// return a summary / distill reply keeps working. The transcript travels in
// req.Input["transcript"]; it is replayed as a single plain user message so a fake that
// inspects the summarizer's opts.Messages sees the flattened, role-clean input.
func (b backendFromRouter) RunTask(ctx context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	transcript, _ := req.Input["transcript"].(string)
	switch req.Task {
	case backend.TaskCheckpoint:
		res, err := b.r.Chat(ctx, domain.ModelSmall, models.ChatOptions{
			Messages: []models.ChatMessage{models.TextMessage("user", transcript)},
		})
		if err != nil {
			return backend.TaskResult{}, err
		}
		// A JSON-object reply is the structured checkpoint; a prose reply degrades to an
		// empty object so validateCheckpoint still mines the transcript's IDs.
		out := json.RawMessage("{}")
		content := strings.TrimSpace(res.Content)
		var probe map[string]json.RawMessage
		if content != "" && json.Unmarshal([]byte(content), &probe) == nil {
			out = json.RawMessage(content)
		}
		return backend.TaskResult{Task: backend.TaskCheckpoint, Output: out, Model: "daintree-assistant"}, nil
	case backend.TaskMemoryDistill:
		res, err := b.r.Chat(ctx, domain.ModelSmall, models.ChatOptions{
			Messages: []models.ChatMessage{models.TextMessage("user", transcript)},
		})
		if err != nil {
			return backend.TaskResult{}, err
		}
		out := backend.MemoryDistillOutput{Facts: parseDistillFacts(res.Content)}
		raw, _ := json.Marshal(out)
		return backend.TaskResult{Task: backend.TaskMemoryDistill, Output: raw, Model: "daintree-assistant"}, nil
	default:
		return backend.TaskResult{Task: req.Task, Output: json.RawMessage("{}"), Model: "daintree-assistant"}, nil
	}
}

// parseDistillFacts decodes the legacy distill-reply shapes the fakes script — either a
// bare string array (["fact A", "fact B"]) or an object array ([{"fact":...,"kind":...}])
// — into the backend's typed fact list. A malformed reply yields nil (no facts saved).
func parseDistillFacts(content string) []backend.DistilledFact {
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}
	// Object array first: [{"fact":"x","kind":"semantic"}, ...].
	var objs []struct {
		Fact string `json:"fact"`
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(content), &objs); err == nil && len(objs) > 0 {
		out := make([]backend.DistilledFact, 0, len(objs))
		for _, o := range objs {
			out = append(out, backend.DistilledFact{Fact: o.Fact, Kind: o.Kind})
		}
		return out
	}
	// Fall back to a bare string array: ["fact A", "fact B"].
	var strs []string
	if err := json.Unmarshal([]byte(content), &strs); err == nil {
		out := make([]backend.DistilledFact, 0, len(strs))
		for _, s := range strs {
			out = append(out, backend.DistilledFact{Fact: s})
		}
		return out
	}
	return nil
}

func baseDeps(r Router, tr ToolRunner) SessionDeps {
	return SessionDeps{
		Backend:   backendFromRouter{r: r},
		Tools:     tr,
		Store:     nil, // best-effort persistence; nil exercises the nil-guard
		SessionID: "sess_test",
		Events:    NoopEventSink{},
	}
}

// recordingBackend wraps backendFromRouter and records every RespondRequest the turn loop
// sent. The per-turn footer (goal anchor, memories, worktree, open workflow runs, session
// note) moved off the prose tail of the model request into STRUCTURED data (req.Turn /
// req.Runtime) that the backend renders — so session-level footer tests assert on those
// fields here, not on a system message in the conversation. It still inherits the message
// forwarding of the embedded adapter, so a wrapped injectRouter.seen also observes the
// per-round visible history.
type recordingBackend struct {
	backendFromRouter
	mu   sync.Mutex
	reqs []backend.RespondRequest
}

func (b *recordingBackend) RespondStream(ctx context.Context, req backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	b.mu.Lock()
	b.reqs = append(b.reqs, req)
	b.mu.Unlock()
	return b.backendFromRouter.RespondStream(ctx, req, cb)
}

// requests returns a copy of every recorded RespondRequest, in round order.
func (b *recordingBackend) requests() []backend.RespondRequest {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]backend.RespondRequest(nil), b.reqs...)
}

// turnAt returns the structured per-turn context the loop sent on round i (empty when the
// round wasn't recorded or carried no turn block).
func (b *recordingBackend) turnAt(i int) backend.TurnContext {
	reqs := b.requests()
	if i < 0 || i >= len(reqs) || reqs[i].Turn == nil {
		return backend.TurnContext{}
	}
	return *reqs[i].Turn
}

// runtimeAt returns the structured runtime context the loop sent on round i (empty when
// the round wasn't recorded or carried no runtime block).
func (b *recordingBackend) runtimeAt(i int) backend.RuntimeContext {
	reqs := b.requests()
	if i < 0 || i >= len(reqs) || reqs[i].Runtime == nil {
		return backend.RuntimeContext{}
	}
	return *reqs[i].Runtime
}

// recordingDeps builds session deps whose Backend is a recordingBackend over r, returning
// both so a test can configure deps further before NewSession and then assert on the
// recorded requests. The wrapped router still drives streaming/utility behavior.
func recordingDeps(r Router, tr ToolRunner) (SessionDeps, *recordingBackend) {
	be := &recordingBackend{backendFromRouter: backendFromRouter{r: r}}
	deps := baseDeps(r, tr)
	deps.Backend = be
	return deps, be
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

func TestFinalAnswerSurfacedThroughAssistantEnd(t *testing.T) {
	sink := &captureSink{}
	// Final round: visible content reaches AssistantEnd as the turn's reply. Reasoning is
	// no longer separated by the session — the backend owns think handling, so the loop
	// always passes an empty reasoning string to AssistantEnd.
	r := &fakeRouter{
		results: []models.ChatResult{{Content: "the visible answer"}},
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
	if sink.endContent != "the visible answer" {
		t.Fatalf("final content not surfaced: content=%q", sink.endContent)
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
	r := &errRouter{err: &backend.Error{HTTPStatus: 429, Type: "rate_limit", Code: "upstream_rate_limited", Message: "provider quota/throughput exceeded"}}
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
