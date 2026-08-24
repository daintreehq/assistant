package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// sseServer spins an httptest server whose /v1/daintree/respond writes the given
// raw SSE body, and records the last decoded request body.
func sseServer(t *testing.T, sseBody string, lastReq *RespondRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/daintree/respond" {
			if lastReq != nil {
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, lastReq)
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, sseBody)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRespondStream_BasicAnswer(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_1","model":"daintree-assistant","state":"dst1.test"}`,
		``,
		`event: delta`,
		`data: {"content":"Hello"}`,
		``,
		`event: delta`,
		`data: {"content":" world"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{"total_tokens":10}}`,
		``,
	}, "\n")

	var got RespondRequest
	srv := sseServer(t, body, &got)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	var streamed strings.Builder
	var metaState string
	res, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "sess_1", TurnID: "turn_1"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"hi"`)}}},
	}, StreamCallbacks{
		OnMeta:    func(m StreamMeta) { metaState = m.State },
		OnContent: func(s string) { streamed.WriteString(s) },
	})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if res.Meta.State != "dst1.test" {
		t.Errorf("state = %q, want dst1.test", res.Meta.State)
	}
	if metaState != "dst1.test" {
		t.Errorf("OnMeta state = %q", metaState)
	}
	if res.Message.Content != "Hello world" {
		t.Errorf("content = %q, want %q", res.Message.Content, "Hello world")
	}
	if streamed.String() != "Hello world" {
		t.Errorf("streamed = %q", streamed.String())
	}
	if res.HasToolCalls() {
		t.Errorf("unexpected tool calls: %+v", res.Message.ToolCalls)
	}
	if res.Usage.TotalTokens != 10 {
		t.Errorf("usage total = %d, want 10", res.Usage.TotalTokens)
	}
	// The request must force stream=true.
	if got.Generation == nil || !got.Generation.Stream {
		t.Errorf("request did not force stream=true: %+v", got.Generation)
	}
}

func TestRespondStream_RunbookLoadFiresBeforeFirstContent(t *testing.T) {
	releaseContent := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseContent)
		}
	}()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "event: meta\n"+
			`data: {"request_id":"req_runbook","runbooks":{"newly_loaded":[{"id":"multi_agent","title":"Multi-agent orchestration"}]},"state":"dst1.runbook"}`+
			"\n\n")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		// Keep the model side gated after meta. The runbook callback must reach the
		// caller while this handler is still waiting here.
		<-releaseContent
		_, _ = io.WriteString(w, "event: delta\n"+
			`data: {"content":"answer"}`+"\n\n"+
			"event: done\n"+
			`data: {"finish_reason":"stop","usage":{}}`+"\n\n")
	}))
	t.Cleanup(srv.Close)

	type observed struct {
		kind string
		refs []RunbookRef
		meta StreamMeta
	}
	eventsCh := make(chan observed, 3)
	doneCh := make(chan error, 1)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	go func() {
		_, err := c.RespondStream(context.Background(), RespondRequest{}, StreamCallbacks{
			OnRawMeta: func(m StreamMeta) {
				eventsCh <- observed{kind: "raw", meta: m}
			},
			OnRunbookLoaded: func(refs []RunbookRef) {
				eventsCh <- observed{kind: "runbook", refs: refs}
			},
			OnMeta: func(m StreamMeta) {
				eventsCh <- observed{kind: "committed", meta: m}
			},
		})
		doneCh <- err
	}()

	select {
	case ev := <-eventsCh:
		if ev.kind != "raw" || ev.meta.State != "dst1.runbook" {
			t.Fatalf("first callback = %+v, want raw meta", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("raw-meta callback waited for model content")
	}
	select {
	case ev := <-eventsCh:
		if ev.kind != "runbook" || len(ev.refs) != 1 || ev.refs[0].ID != "multi_agent" || ev.refs[0].Title != "Multi-agent orchestration" {
			t.Fatalf("second callback = %+v, want eager runbook cue", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("runbook callback waited for model content")
	}
	select {
	case ev := <-eventsCh:
		t.Fatalf("callback %q fired before first content", ev.kind)
	default:
	}

	close(releaseContent)
	released = true
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("RespondStream: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RespondStream did not finish after content was released")
	}
	select {
	case ev := <-eventsCh:
		if ev.kind != "committed" || ev.meta.State != "dst1.runbook" {
			t.Fatalf("post-content callback = %+v, want committed meta", ev)
		}
	default:
		t.Fatal("committed meta callback did not fire with first content")
	}
}

func TestRespondStream_ToolCallAccumulation(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"protocol_version":2,"request_id":"req_2","model":"daintree-assistant","state":"dst1.x"}`,
		``,
		`event: delta`,
		`data: {"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"git__status","arguments":""}}]}`,
		``,
		`event: delta`,
		`data: {"tool_calls":[{"index":0,"function":{"arguments":"{}"}}]}`,
		``,
		`event: done`,
		`data: {"finish_reason":"tool_calls","usage":{}}`,
		``,
	}, "\n")

	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	res, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"status?"`)}}},
	}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if !res.HasToolCalls() {
		t.Fatalf("expected a tool call, got none")
	}
	tc := res.Message.ToolCalls[0]
	if tc.ID != "call_1" || tc.Function.Name != "git__status" || tc.Function.Arguments != "{}" {
		t.Errorf("tool call = %+v", tc)
	}
	if res.FinishReason != "tool_calls" {
		t.Errorf("finish = %q", res.FinishReason)
	}
}

func TestRespondStream_SyntheticToolID(t *testing.T) {
	// A tool-call fragment with no id must still produce a usable, stable id.
	body := strings.Join([]string{
		`event: meta`,
		`data: {"request_id":"r","model":"m","state":"s"}`,
		``,
		`event: delta`,
		`data: {"tool_calls":[{"index":0,"function":{"name":"fs__read","arguments":"{\"path\":\"x\"}"}}]}`,
		``,
		`event: done`,
		`data: {"finish_reason":"tool_calls","usage":{}}`,
		``,
	}, "\n")
	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	res, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}},
	}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if len(res.Message.ToolCalls) != 1 || res.Message.ToolCalls[0].ID == "" {
		t.Fatalf("expected a synthesized id, got %+v", res.Message.ToolCalls)
	}
}

func TestRespondStream_ErrorEvent(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"request_id":"r","model":"m","state":"s"}`,
		``,
		`event: error`,
		`data: {"error":{"type":"api_error","code":"upstream_failed","message":"model failed"},"retry_after":"5"}`,
		``,
	}, "\n")
	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	res, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}},
	}, StreamCallbacks{})
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var be *Error
	if !errorsAs(err, &be) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if be.Code != "upstream_failed" || !be.Stream {
		t.Errorf("error = %+v", be)
	}
	if be.RetryAfter != 5*time.Second {
		t.Errorf("retry after = %v, want 5s", be.RetryAfter)
	}
	// A failed stream must NOT yield a completed assistant message with content.
	if res.Message.Content != "" {
		t.Errorf("error stream produced content: %q", res.Message.Content)
	}
}

func TestRespondStream_EOFBeforeDone(t *testing.T) {
	body := strings.Join([]string{
		`event: meta`,
		`data: {"request_id":"r","model":"m","state":"s"}`,
		``,
		`event: delta`,
		`data: {"content":"partial"}`,
		``,
	}, "\n")
	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	_, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}},
	}, StreamCallbacks{})
	if err == nil {
		t.Fatalf("expected interrupted error, got nil")
	}
	var be *Error
	if !errorsAs(err, &be) || be.Code != "stream_interrupted" {
		t.Fatalf("expected stream_interrupted, got %v", err)
	}
}

func TestRespondStream_PreStreamHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"system_messages_not_allowed","message":"server owns system text","param":"input.messages[0].role"}}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	_, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}},
	}, StreamCallbacks{})
	var be *Error
	if !errorsAs(err, &be) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if !be.IsContract() || be.Code != "system_messages_not_allowed" || be.Param != "input.messages[0].role" {
		t.Errorf("error = %+v", be)
	}
}

func TestRespondStream_StopsAtDone(t *testing.T) {
	// A `done` event is terminal: any events AFTER it must be ignored (and the parser
	// must not block waiting for them). The trailing delta below must NOT appear.
	body := strings.Join([]string{
		`event: meta`,
		`data: {"request_id":"r","model":"m","state":"s"}`,
		``,
		`event: delta`,
		`data: {"content":"Hello"}`,
		``,
		`event: done`,
		`data: {"finish_reason":"stop","usage":{}}`,
		``,
		`event: delta`,
		`data: {"content":"TRAILING-IGNORED"}`,
		``,
	}, "\n")
	srv := sseServer(t, body, nil)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	res, err := c.RespondStream(context.Background(), RespondRequest{
		Session: RespondSession{ID: "s", TurnID: "t"},
		Input:   RespondInput{Messages: []Message{{Role: "user", Content: json.RawMessage(`"x"`)}}},
	}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("RespondStream: %v", err)
	}
	if res.Message.Content != "Hello" {
		t.Errorf("content = %q, want %q (trailing post-done delta must be ignored)", res.Message.Content, "Hello")
	}
}

func TestTaskTranscriptAndTailClamped(t *testing.T) {
	var gotCheckpoint, gotSummarize TaskRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req TaskRequest
		_ = json.Unmarshal(body, &req)
		switch req.Task {
		case TaskCheckpoint:
			gotCheckpoint = req
		case TaskTerminalSummarize:
			gotSummarize = req
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"t","object":"daintree.task.result","task":"`+req.Task+`","model":"m","output":{},"finish_reason":"stop","usage":{},"prompt_version":"v"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	hugeTranscript := strings.Repeat("a", maxTaskTranscriptRunes+50_000)
	if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: hugeTranscript}); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if got := len([]rune(gotCheckpoint.Input["transcript"].(string))); got != maxTaskTranscriptRunes {
		t.Errorf("checkpoint transcript clamped to %d runes, want %d", got, maxTaskTranscriptRunes)
	}

	hugeTail := strings.Repeat("b", maxTaskTailRunes+10_000)
	if _, err := RunTerminalSummarize(context.Background(), c, TerminalSummarizeInput{Tail: hugeTail}); err != nil {
		t.Fatalf("RunTerminalSummarize: %v", err)
	}
	if got := len([]rune(gotSummarize.Input["tail"].(string))); got != maxTaskTailRunes {
		t.Errorf("summarize tail clamped to %d runes, want %d", got, maxTaskTailRunes)
	}
}

func TestRunTask(t *testing.T) {
	var gotReq TaskRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daintree/tasks" {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotReq)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"task_1","object":"daintree.task.result","task":"checkpoint","model":"m","output":{"goal":"ship it","next_actions":["merge"]},"finish_reason":"stop","usage":{"total_tokens":5},"prompt_version":"checkpoint"}`)
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{BaseURL: srv.URL})

	out, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "long transcript"})
	if err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if out.Goal != "ship it" || len(out.NextActions) != 1 || out.NextActions[0] != "merge" {
		t.Errorf("checkpoint output = %+v", out)
	}
	if gotReq.Task != "checkpoint" {
		t.Errorf("task = %q", gotReq.Task)
	}
	if _, ok := gotReq.Input["transcript"]; !ok {
		t.Errorf("input missing transcript: %+v", gotReq.Input)
	}
	// Tasks must never carry prompt-like fields.
	for _, banned := range []string{"system", "developer", "messages", "system_prompt"} {
		if _, ok := gotReq.Input[banned]; ok {
			t.Errorf("task input smuggled %q", banned)
		}
	}
}

// TestRunTaskOnTaskHook pins the task-observability seam: every RunTask round trip
// (success AND failure) invokes OnTask with the task name, outcome, and bounded
// sizes — the hook the app routes to the debug log's backend.task event. Without it
// utility tasks (a /compact's checkpoint + memory_distill included) are invisible
// in session logs.
func TestRunTaskOnTaskHook(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/daintree/tasks" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"task_1","object":"daintree.task.result","task":"checkpoint","model":"m","output":{"goal":"g"},"finish_reason":"stop","usage":{"total_tokens":5},"prompt_version":"checkpoint"}`)
	}))
	t.Cleanup(srv.Close)

	var got []TaskTraceInfo
	// Retries disabled: the failure leg below points at a CLOSED server, and the
	// production policy would patiently replay that refused socket for ~a minute.
	// OnTask fires once per whole RunTask call regardless of attempts — that
	// once-per-call property is pinned by TestDoJSONRetriesTransientFailure.
	c := NewClient(ClientConfig{
		BaseURL: srv.URL,
		Retry:   RetryPolicy{MaxAttempts: 1},
		OnTask:  func(info TaskTraceInfo) { got = append(got, info) },
	})

	if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "long transcript"}); err != nil {
		t.Fatalf("RunCheckpoint: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("OnTask calls = %d, want 1", len(got))
	}
	if got[0].Task != "checkpoint" || got[0].Err != nil {
		t.Errorf("OnTask info = %+v, want task=checkpoint err=nil", got[0])
	}
	if got[0].InputBytes <= 0 || got[0].OutputBytes <= 0 {
		t.Errorf("OnTask sizes = in %d out %d, want both > 0", got[0].InputBytes, got[0].OutputBytes)
	}

	// Failure path: a dead endpoint still reports the attempt, with Err set.
	srv.Close()
	got = nil
	if _, err := RunCheckpoint(context.Background(), c, CheckpointInput{Transcript: "t"}); err == nil {
		t.Fatalf("RunCheckpoint after close: want error")
	}
	if len(got) != 1 || got[0].Err == nil {
		t.Fatalf("OnTask on failure = %+v, want one call with Err set", got)
	}
}

func TestCapabilitiesAndHealth(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/health":
			_, _ = io.WriteString(w, `{"status":"ok"}`)
		case "/readyz":
			_, _ = io.WriteString(w, `{"status":"ready","catalog_revision":"sha256:x","runbooks":4}`)
		case "/v1/daintree/capabilities":
			_, _ = io.WriteString(w, `{"server_version":"1.0.0","protocol":{"min":2,"max":2},"respond":{"endpoint":"/v1/daintree/respond","model":"daintree-assistant","streaming":true,"stream_events":["meta","delta","done","error"],"system_messages_accepted":false,"max_active_runbooks":3,"display_context":true},"tasks":["checkpoint","memory_distill"],"limits":{"request_bytes":8388608,"tools":128}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClient(ClientConfig{BaseURL: srv.URL})
	if err := c.Health(context.Background()); err != nil {
		t.Fatalf("Health: %v", err)
	}
	if err := c.Ready(context.Background()); err != nil {
		t.Fatalf("Ready: %v", err)
	}
	caps, err := c.Capabilities(context.Background())
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if caps.Protocol.Min != 2 || caps.Protocol.Max != 2 {
		t.Errorf("protocol = %+v", caps.Protocol)
	}
	if len(caps.Tasks) != 2 {
		t.Errorf("tasks = %+v", caps.Tasks)
	}
	if caps.Respond.SystemMessagesAccepted {
		t.Errorf("system messages should not be accepted")
	}
	// The gate that decides whether the CLI may attach runtime.display is only as good
	// as this decode: a mistyped key would silently read as "unsupported" forever, and
	// the symptom (replies shaped for a default width) never looks like a JSON bug.
	if !caps.Respond.DisplayContext {
		t.Errorf("display_context did not decode: %+v", caps.Respond)
	}
}

// errorsAs is a tiny local errors.As wrapper to avoid importing errors in the
// many small assertions above.
func errorsAs(err error, target **Error) bool {
	if e, ok := err.(*Error); ok {
		*target = e
		return true
	}
	return false
}
