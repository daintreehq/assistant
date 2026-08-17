package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
)

// orderSink records the ordered vocabulary of high-level events the loop emits so
// the ordering + wire-name translation can be asserted end-to-end (agentEvents).
type orderSink struct {
	NoopEventSink
	log []string
}

func (s *orderSink) TurnPrompt(p string)         { s.log = append(s.log, "prompt:"+p) }
func (s *orderSink) AssistantStart()             { s.log = append(s.log, "start") }
func (s *orderSink) AssistantToken(t string)     { s.log = append(s.log, "tok:"+t) }
func (s *orderSink) AssistantEnd(c, _ string)    { s.log = append(s.log, "end:"+c) }
func (s *orderSink) AssistantCancelled(c string) { s.log = append(s.log, "cancelled:"+c) }
func (s *orderSink) Interjection(t string)       { s.log = append(s.log, "interject:"+t) }
func (s *orderSink) SkillLoaded(titles []string) {
	s.log = append(s.log, "skill:"+strings.Join(titles, ","))
}
func (s *orderSink) ToolCall(ev ToolCallEvent) { s.log = append(s.log, "call:"+ev.Name+":"+ev.ID) }
func (s *orderSink) ToolResult(ev ToolResultEvent) {
	ok := "false"
	if ev.Result.Ok {
		ok = "true"
	}
	s.log = append(s.log, "result:"+ev.Name+":"+ok+":"+ev.ID)
}
func (s *orderSink) Error(m string) { s.log = append(s.log, "error:"+m) }

func contains(log []string, want string) bool {
	for _, e := range log {
		if e == want {
			return true
		}
	}
	return false
}

func TestEmitStreamsTokensThenEnds(t *testing.T) {
	sink := &orderSink{}
	r := &fakeRouter{
		results: []models.ChatResult{{Content: "Hello"}},
		streams: [][]string{{"Hel", "lo"}},
	}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	out, err := s.Send(context.Background(), "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "Hello" {
		t.Fatalf("reply = %q", out)
	}
	// turn:prompt is emitted FIRST (before the assistant round) so /explain can
	// label the run by what prompted it.
	want := []string{"prompt:hi", "start", "tok:Hel", "tok:lo", "end:Hello"}
	if !equalStrings(sink.log, want) {
		t.Fatalf("event log = %v want %v", sink.log, want)
	}
}

// eagerSkillBackend mirrors the production callback order: raw SSE meta is observed,
// then the newly-loaded skill notification arrives before committed meta and content.
type eagerSkillBackend struct{}

func (eagerSkillBackend) RespondStream(_ context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	refs := []backend.SkillRef{
		{ID: "multi_agent", Title: "Multi-agent orchestration"},
		{ID: "fallback_skill"}, // no title: the label falls back to the id
		{},                     // malformed refs never produce a blank label
	}
	meta := backend.StreamMeta{
		Model:  "daintree-assistant",
		State:  "dst1.skill",
		Skills: backend.SkillsBlock{NewlyLoaded: refs},
	}
	if cb.OnRawMeta != nil {
		cb.OnRawMeta(meta)
	}
	if cb.OnSkillLoaded != nil {
		cb.OnSkillLoaded(refs)
	}
	if cb.OnMeta != nil {
		cb.OnMeta(meta)
	}
	if cb.OnContent != nil {
		cb.OnContent("answer")
	}
	return backend.RespondResult{
		Meta:    meta,
		Message: backend.RespondMessage{Role: "assistant", Content: "answer"},
	}, nil
}

func (eagerSkillBackend) RunTask(context.Context, backend.TaskRequest) (backend.TaskResult, error) {
	return backend.TaskResult{}, nil
}

// The skill event still fires BEFORE the first token, even though nothing renders it.
// The contract is now DIAGNOSTIC rather than visual: it is what lets a debug log time
// selection separately from generation (backend.respond.skill_cue landing ahead of the
// content stream), which is the trace a selector regression is read from. Emitting it
// late would collapse those two costs into one indistinguishable span.
func TestEmitSkillLoadBeforeFirstToken(t *testing.T) {
	sink := &orderSink{}
	r := &fakeRouter{results: []models.ChatResult{{Content: "unused"}}}
	deps := baseDeps(r, &fakeTools{})
	deps.Backend = eagerSkillBackend{}
	deps.Events = sink
	cap := &traceCapture{}
	deps.Trace = cap.record
	s := NewSession(deps)

	out, err := s.Send(context.Background(), "use agents", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "answer" {
		t.Fatalf("reply = %q", out)
	}
	want := []string{
		"prompt:use agents",
		"start",
		"skill:Multi-agent orchestration,fallback_skill",
		"tok:answer",
		"end:answer",
	}
	if !equalStrings(sink.log, want) {
		t.Fatalf("event log = %v want %v", sink.log, want)
	}
	traceIndex := func(name string) int {
		for i, ev := range cap.events {
			if ev.event == name {
				return i
			}
		}
		return -1
	}
	raw, cue, committed := traceIndex("backend.respond.raw_meta"), traceIndex("backend.respond.skill_cue"), traceIndex("backend.respond.meta")
	if raw < 0 || cue <= raw || committed <= cue {
		t.Fatalf("trace callback order raw=%d cue=%d committed=%d; events=%+v", raw, cue, committed, cap.events)
	}
}

func TestEmitTurnPromptPrecedesAssistantStart(t *testing.T) {
	sink := &orderSink{}
	r := &fakeRouter{
		results: []models.ChatResult{{Content: "ok"}},
		streams: [][]string{{"ok"}},
	}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	if _, err := s.Send(context.Background(), "label me", SendOptions{}); err != nil {
		t.Fatal(err)
	}
	pi, si := indexOfLog(sink.log, "prompt:label me"), indexOfLog(sink.log, "start")
	if pi < 0 || si < 0 || pi >= si {
		t.Fatalf("turn:prompt must precede assistant:start: %v", sink.log)
	}
}

// indexOfLog returns the first index of want in log, or -1.
func indexOfLog(log []string, want string) int {
	for i, e := range log {
		if e == want {
			return i
		}
	}
	return -1
}

func TestEmitTranslatesWireNameInToolEvents(t *testing.T) {
	// The model returns the OpenAI-legal wire name (fs__search); the loop must
	// translate it back to the internal dotted name (fs.search) before the tool
	// events fire, and the call id must flow through both events.
	sink := &orderSink{}
	tools := &fakeTools{result: domain.Ok("found 2 files", nil)}
	r := &fakeRouter{
		results: []models.ChatResult{
			{ToolCalls: []models.ToolCallRequest{toolCall("c1", "fs__search", "{}")}},
			{Content: "done"},
		},
	}
	deps := baseDeps(r, tools)
	deps.Events = sink
	s := NewSession(deps)
	out, err := s.Send(context.Background(), "search", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if out != "done" {
		t.Fatalf("reply = %q", out)
	}
	if !contains(sink.log, "call:fs.search:c1") {
		t.Fatalf("missing translated tool call event: %v", sink.log)
	}
	if !contains(sink.log, "result:fs.search:true:c1") {
		t.Fatalf("missing translated tool result event: %v", sink.log)
	}
	if sink.log[len(sink.log)-1] != "end:done" {
		t.Fatalf("last event = %q want end:done", sink.log[len(sink.log)-1])
	}
}

func TestEmitReportsModelErrorThroughSink(t *testing.T) {
	sink := &orderSink{}
	r := &errRouter{err: errors.New("boom")}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	out, err := s.Send(context.Background(), "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Model error: boom") {
		t.Fatalf("reply = %q want 'Model error: boom'", out)
	}
	var sawError bool
	for _, e := range sink.log {
		if len(e) >= 6 && e[:6] == "error:" {
			sawError = true
		}
	}
	if !sawError {
		t.Fatalf("expected an error event: %v", sink.log)
	}
}

// TestEmitReportsBackendUnreachableAsConnectivity: a connection-level backend failure
// (Code "connect") is relabeled as a connectivity problem with a /doctor next step —
// NOT "Model error:" — and the reply is still a registered wake failure so a
// timer/watcher wake that fails this way is treated as a non-result.
func TestEmitReportsBackendUnreachableAsConnectivity(t *testing.T) {
	sink := &orderSink{}
	r := &errRouter{err: &backend.Error{Code: "connect", Message: "could not reach assistant backend: dial tcp 127.0.0.1:8473: connect: connection refused"}}
	deps := baseDeps(r, &fakeTools{})
	deps.Events = sink
	s := NewSession(deps)
	out, err := s.Send(context.Background(), "hi", SendOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "Model error:") {
		t.Fatalf("a connect failure must not read as a model error: %q", out)
	}
	if !strings.Contains(out, "/doctor") {
		t.Fatalf("expected a /doctor next-step hint: %q", out)
	}
	if !IsWakeFailureReply(out) {
		t.Fatalf("connectivity reply must be a registered wake failure: %q", out)
	}
}

// errRouter fails its stream with a configurable error (model-error path).
type errRouter struct{ err error }

func (r *errRouter) Stream(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions, onToken func(string)) (models.ChatResult, error) {
	return models.ChatResult{}, r.err
}
func (r *errRouter) Chat(ctx context.Context, tier domain.ModelTier, opts models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "S"}, nil
}
func (r *errRouter) ModelFor(domain.ModelTier) string { return "minimax-m3" }
