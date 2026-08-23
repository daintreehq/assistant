package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// server_test.go drives the REAL MCP server through a REAL MCP client over the SDK's
// in-memory transport, against a fake runtime. That combination is the point: the
// handshake, the generated schemas, argument validation and the structured results are
// all exercised, while no database, backend, project lease or network is involved.

// fakeRuntime is a cooperative assistant: Send blocks until the test releases it, so a
// test can observe a run mid-flight — which is the state most of this surface exists to
// manage.
type fakeRuntime struct {
	id        string
	facts     RuntimeFacts
	approvals *Approvals

	// script runs inside Send with the REAL sink the session installed. Driving events
	// through it is deliberate: recording used to be broken in production precisely
	// because tests reached for NewRecorder directly and never exercised the wiring.
	script func(sink agent.EventSink)
	// confirmInSend, when set, raises a confirmation ON THE SEND GOROUTINE, the way a
	// real dispatch does. Detaching it to a goroutine would let turns.Wait() return
	// while a dispatch was still parked — the exact ordering these tests exist to pin.
	confirmInSend *ApprovalRequest
	confirmResult chan bool

	// silent makes Send return cleanly WITHOUT emitting any terminal event — the shape a
	// runtime with an unwired sink produces, and the one the server must refuse rather
	// than report as an empty success.
	silent bool
	// closeErr makes Close fail, modelling a teardown that leaves the project lease held.
	closeErr error
	// closeBlock, when set, holds Close open until it is closed — a teardown that hangs,
	// which is the case the whole closing-state machine exists for.
	closeBlock chan struct{}

	mu           sync.Mutex
	release      chan struct{}
	sends        int
	closeCount   int
	sendInFlight bool
	lastRunID    string
	discards     int
	// closedDuringSend records the bug this design exists to prevent: tearing the
	// runtime down while Send is still unwinding would close the store underneath it.
	closedDuringSend bool
	injected         []string
	closed           bool
	attention        []domain.QueueEvent
	acked            bool
	ackedIDs         []string
	attErr           error
}

func newFakeRuntime(id string) *fakeRuntime {
	return &fakeRuntime{
		id:            id,
		facts:         RuntimeFacts{Project: "/repo", Tier: "operator", BackendURL: "http://127.0.0.1:8473", LogPath: "/logs/x.log", MCPConnected: true, MCPTransport: "streamable-http", ApprovalMode: string(ApprovalDecline)},
		release:       make(chan struct{}),
		approvals:     NewApprovals(ApprovalDecline, 0),
		confirmResult: make(chan bool, 4),
	}
}

func (f *fakeRuntime) Approvals() *Approvals { return f.approvals }
func (f *fakeRuntime) SessionID() string     { return f.id }
func (f *fakeRuntime) Facts() RuntimeFacts   { return f.facts }

func (f *fakeRuntime) DiscardPendingInjections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discards++
}

func (f *fakeRuntime) Send(ctx context.Context, prompt, runID string, sink agent.EventSink) (string, error) {
	f.mu.Lock()
	f.sends++
	f.sendInFlight = true
	f.lastRunID = runID
	release := f.release
	script := f.script
	confirmReq := f.confirmInSend
	f.mu.Unlock()
	// Wrap the sink so the fake can tell whether the script already produced a terminal
	// event. A real runtime always emits exactly one; the fake must too, and must not
	// emit a SECOND one over a script that did its own.
	tracked := &terminalTrackingSink{EventSink: sink}
	sink = tracked
	if script != nil {
		script(sink)
	}
	if confirmReq != nil {
		req := *confirmReq
		req.RunID = runID
		// Blocks HERE, on the turn goroutine, exactly as a parked dispatch does.
		f.confirmResult <- f.approvals.Confirm(ctx, req)
	}
	defer func() {
		f.mu.Lock()
		f.sendInFlight = false
		f.mu.Unlock()
	}()
	select {
	case <-release:
		reply := "answered: " + prompt
		// Emit the terminal event a real runtime emits. Returning a reply with no
		// terminal event is the BROKEN-SINK shape, which the server now refuses as
		// RUN_EVENT_STREAM_INCOMPLETE — so a fake that did that would be modelling the
		// bug rather than the runtime. See f.silent for a fake that models it on purpose.
		f.mu.Lock()
		silent := f.silent
		f.mu.Unlock()
		if !silent && !tracked.sawTerminal {
			tracked.EventSink.AssistantEnd(reply, "")
		}
		return reply, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (f *fakeRuntime) InjectPrompt(text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.injected = append(f.injected, text)
}

func (f *fakeRuntime) Attention(_ context.Context, acknowledge bool) ([]domain.QueueEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attErr != nil {
		return nil, f.attErr
	}
	f.acked = acknowledge
	return f.attention, nil
}

func (f *fakeRuntime) AcknowledgeAttention(_ context.Context, ids []string) (int, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.attErr != nil {
		return 0, nil, f.attErr
	}
	live := map[string]bool{}
	for _, e := range f.attention {
		live[e.ID] = true
	}
	acked := 0
	unknown := []string{}
	for _, id := range ids {
		if live[id] {
			acked++
			f.ackedIDs = append(f.ackedIDs, id)
			continue
		}
		unknown = append(unknown, id)
	}
	return acked, unknown, nil
}

func (f *fakeRuntime) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closeCount
}

func (f *fakeRuntime) Close() error {
	if f.closeBlock != nil {
		<-f.closeBlock
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeCount++
	if f.sendInFlight {
		f.closedDuringSend = true
	}
	f.closed = true
	// closeErr models a teardown that fails — a lease that will not release, a store
	// that will not close. The registry must keep that session VISIBLE rather than
	// deleting it, because the project is then stuck and nobody can see why.
	return f.closeErr
}

func (f *fakeRuntime) letFinish() {
	f.mu.Lock()
	defer f.mu.Unlock()
	select {
	case <-f.release:
	default:
		close(f.release)
	}
}

func (f *fakeRuntime) discardCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.discards
}

func (f *fakeRuntime) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// connect stands up the real server + client over an in-memory pipe.
func connect(t *testing.T, factory RuntimeFactory) (*mcp.ClientSession, *Registry) {
	t.Helper()
	ctx := context.Background()
	reg := NewUnconfinedRegistry(ctx, factory)
	srv := mcp.NewServer(&mcp.Implementation{Name: ServerName, Version: "test"}, nil)
	Register(srv, reg, NewBinaryInfo("test"), ctx)

	st, ct := mcp.NewInMemoryTransports()
	if _, err := srv.Connect(ctx, st, nil); err != nil {
		t.Fatalf("server connect: %v", err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.Close()
		reg.CloseAll()
	})
	return cs, reg
}

// call invokes a tool and decodes its structured result into out. A tool that reported
// an error returns it, so a test can assert on failures too.
func call(t *testing.T, cs *mcp.ClientSession, name string, args any, out any) error {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		return err
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return errors.New(sb.String())
	}
	if out == nil {
		return nil
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structured content: %v", err)
	}
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("decode %s result: %v (raw %s)", name, err, b)
	}
	return nil
}

func openSession(t *testing.T, cs *mcp.ClientSession) SessionOutput {
	t.Helper()
	var out SessionOutput
	if err := call(t, cs, "daintree.session.open", OpenInput{Project: "/repo"}, &out); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	return out
}

// TestToolsAreDiscoverableWithSchemas: the whole reason for the typed surface is that a
// caller can discover the exact argument shape. If a tool ever lost its input schema, a
// model would be guessing — which is the single biggest cause of tool misuse in this
// system's own logs.
func TestToolsAreDiscoverableWithSchemas(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_test"), nil
	})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range res.Tools {
		byName[tool.Name] = tool
	}
	for _, want := range []string{
		"daintree.session.open", "daintree.session.list", "daintree.session.close",
		"daintree.ask", "daintree.poll", "daintree.inject", "daintree.interrupt",
		"daintree.attention",
	} {
		tool, ok := byName[want]
		if !ok {
			t.Errorf("tool %q is not offered", want)
			continue
		}
		if tool.InputSchema == nil {
			t.Errorf("tool %q has no input schema — a caller would have to guess its arguments", want)
		}
		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %q has no description", want)
		}
	}
}

// TestAskReturnsImmediatelyWhileTheTurnRuns is the core contract. A Daintree turn takes
// minutes; if ask blocked, every MCP client would time out before the work it exists to
// do had finished.
func TestAskReturnsImmediatelyWhileTheTurnRuns(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	started := time.Now()
	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "spawn a cohort"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// The fake's Send is still blocked, so returning at all proves the handle came back
	// without waiting for the turn.
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("ask took %v — it must return a handle immediately", elapsed)
	}
	if run.Status != string(RunRunning) {
		t.Errorf("status = %q, want running", run.Status)
	}
	if run.RunID == "" {
		t.Fatal("ask returned no runId to poll")
	}
	if run.Content != "" {
		t.Errorf("content = %q, want empty while running", run.Content)
	}

	// Now let it finish and poll it to completion.
	fake.letFinish()
	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID, WaitMs: 5000}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Status != string(RunSucceeded) {
		t.Fatalf("status = %q, want success", polled.Status)
	}
	if polled.Content != "answered: spawn a cohort" {
		t.Errorf("content = %q", polled.Content)
	}
	if polled.DurationMs < 0 {
		t.Errorf("durationMs = %d", polled.DurationMs)
	}
}

// TestSecondAskIsRejectedWhileBusy: agent.Session.Send is single-flight, and a second
// concurrent turn would corrupt the conversation. The rejection must name the remedy.
func TestSecondAskIsRejectedWhileBusy(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "first"}, &RunOutput{}); err != nil {
		t.Fatalf("first ask: %v", err)
	}
	err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "second"}, &RunOutput{})
	if err == nil {
		t.Fatal("a second concurrent ask must be rejected")
	}
	if !strings.Contains(err.Error(), "in flight") {
		t.Errorf("rejection should explain the conflict, got: %v", err)
	}
	fake.letFinish()
}

// TestInjectSteersTheRunningTurn: inject is the documented alternative to a second ask.
func TestInjectSteersTheRunningTurn(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	// With nothing running it must say so rather than silently swallow the message.
	var idle ActedOutput
	if err := call(t, cs, "daintree.inject", InjectInput{SessionID: sess.SessionID, Text: "hi"}, &idle); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if idle.Acted {
		t.Error("inject with no running turn must report acted=false")
	}
	if !strings.Contains(idle.Message, "ask") {
		t.Errorf("the message should point at daintree.ask, got %q", idle.Message)
	}

	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "work"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	var acted ActedOutput
	if err := call(t, cs, "daintree.inject", InjectInput{SessionID: sess.SessionID, Text: "also check the tests"}, &acted); err != nil {
		t.Fatalf("inject: %v", err)
	}
	if !acted.Acted {
		t.Error("inject during a running turn must report acted=true")
	}
	fake.mu.Lock()
	got := append([]string(nil), fake.injected...)
	fake.mu.Unlock()
	if len(got) != 1 || got[0] != "also check the tests" {
		t.Errorf("injected = %v", got)
	}
	fake.letFinish()
}

// TestInterruptCancelsTheTurnAndKeepsTheSession: a cancelled turn must leave the session
// usable, or a caller would have to rebuild the whole conversation to recover.
func TestInterruptCancelsTheTurnAndKeepsTheSession(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "long job"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	var acted ActedOutput
	if err := call(t, cs, "daintree.interrupt", SessionRefInput{SessionID: sess.SessionID}, &acted); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if !acted.Acted {
		t.Fatal("interrupt during a running turn must report acted=true")
	}
	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID, WaitMs: 5000}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if polled.Status != string(RunCancelled) {
		t.Errorf("status = %q, want cancelled", polled.Status)
	}
	// The session survives: a fresh ask is accepted.
	fake.letFinish()
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "again"}, &RunOutput{}); err != nil {
		t.Errorf("the session must stay usable after an interrupt: %v", err)
	}
}

// TestPollIsIncrementalAndReportsWhatItWithheld: poll returns a WINDOW because the
// caller is a model paying context per event. Silent truncation would read as a complete
// timeline, so the count of withheld events is part of the contract.
func TestPollIsIncrementalAndReportsWhatItWithheld(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	// Ten events, emitted through the sink the SESSION installed — the production path.
	fake.script = func(sink agent.EventSink) {
		for i := 0; i < 10; i++ {
			sink.Info("step")
		}
	}
	cs, reg := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	live, err := reg.Get(sess.SessionID)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "work"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// ask is ASYNC: it answers the moment the turn is admitted, which can be before the
	// turn goroutine has entered Send and run the script at all. Wait for the ten events
	// to be recorded, or the first window is legitimately — and misleadingly — empty.
	waitForRecordedEvents(t, live, run.RunID, 10)
	var first RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID, MaxEvents: 4}, &first); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(first.Events) != 4 {
		t.Fatalf("events = %d, want the 4 requested", len(first.Events))
	}
	if first.WithheldEvents != 6 {
		t.Errorf("withheldEvents = %d, want 6 — silent truncation reads as a complete timeline", first.WithheldEvents)
	}
	if first.NextSeq != first.Events[3].Seq+1 {
		t.Errorf("nextSeq = %d, want one past the last returned event", first.NextSeq)
	}

	// Reading from nextSeq must return the remainder with no gap and no repeat.
	var second RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID, SinceSeq: first.NextSeq, MaxEvents: 100}, &second); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(second.Events) != 6 {
		t.Fatalf("second window = %d events, want 6", len(second.Events))
	}
	if second.Events[0].Seq != first.NextSeq {
		t.Errorf("second window starts at seq %d, want %d (a gap or a repeat)", second.Events[0].Seq, first.NextSeq)
	}
	if second.WithheldEvents != 0 {
		t.Errorf("withheldEvents = %d, want 0", second.WithheldEvents)
	}
	fake.letFinish()
}

// TestAsyncHandlesSurfaceAsPendingNotAsResults: async work settles OUTSIDE the run. If a
// caller believed a pending handle was a result it would report finished work that had
// not started.
func TestAsyncHandlesSurfaceAsPendingNotAsResults(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.script = func(sink agent.EventSink) {
		sink.ToolResult(agent.ToolResultEvent{
			ID:   "call-1",
			Name: "terminal.run.async",
			Result: domain.ToolResult{
				Ok:      true,
				Summary: "accepted",
				Async:   &domain.AsyncHandle{ID: "asy_abc123", ToolName: "terminal.run.async"},
			},
		})
	}
	cs, reg := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	live, err := reg.Get(sess.SessionID)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "spawn"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	// Same async-ask race as the poll-window test: the tool result the script emits has
	// to be recorded before polling can see its pending handle.
	waitForRecordedEvents(t, live, run.RunID, 1)

	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(polled.AsyncOperations) != 1 || polled.AsyncOperations[0].ID != "asy_abc123" {
		t.Fatalf("asyncOperations = %+v, want one entry for asy_abc123", polled.AsyncOperations)
	}
	if got := polled.AsyncOperations[0].Status; got != "accepted" {
		t.Fatalf("asyncOperations[0].status = %q, want accepted", got)
	}
	// The ledger must survive the caller advancing past the accepting event — deriving
	// it from the poll window was the bug.
	var advanced RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID, SinceSeq: polled.NextSeq}, &advanced); err != nil {
		t.Fatalf("poll (advanced): %v", err)
	}
	if len(advanced.AsyncOperations) != 1 {
		t.Fatalf("asyncOperations vanished once sinceSeq advanced: %+v", advanced.AsyncOperations)
	}
	fake.letFinish()
}

// TestAttentionPeeksByDefaultAndAcksExplicitly: acknowledging inside the read makes
// delivery at-most-once — the rows are stamped before the response is known to have
// arrived — and an attention row is the ONLY report background work ever makes. Peeking
// by default plus an explicit ack turns a dropped response into a duplicate instead.
func TestAttentionPeeksByDefaultAndAcksExplicitly(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.attention = []domain.QueueEvent{{
		ID: "evt_1", Severity: domain.SeverityDone, Source: domain.SourceAsyncTool,
		Title: "cohort finished", Summary: "3 agents completed", Count: 3,
		Target: &domain.EventTarget{TerminalID: "terminal-abc", AsyncInvocationID: "asy_abc123"},
	}}
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	var out AttentionOutput
	if err := call(t, cs, "daintree.attention", AttentionInput{SessionID: sess.SessionID}, &out); err != nil {
		t.Fatalf("attention: %v", err)
	}
	if out.Count != 1 || len(out.Items) != 1 {
		t.Fatalf("items = %+v", out.Items)
	}
	item := out.Items[0]
	if item.Title != "cohort finished" || item.Count != 3 {
		t.Errorf("item = %+v", item)
	}
	// The async id is what lets a caller match background work back to the run that
	// started it.
	if item.AsyncID != "asy_abc123" {
		t.Errorf("asyncId = %q, want asy_abc123", item.AsyncID)
	}
	if !strings.Contains(item.Target, "terminal-abc") {
		t.Errorf("target = %q", item.Target)
	}
	fake.mu.Lock()
	acked := fake.acked
	fake.mu.Unlock()
	if acked {
		t.Error("attention must PEEK by default; acknowledging inside the read loses items whose response never arrives")
	}
	if !strings.Contains(out.Note, "daintree.attention.ack") {
		t.Errorf("a peek must say what the caller still owes, got note %q", out.Note)
	}

	// acknowledge:true is still available for a caller that accepts the risk.
	yes := true
	if err := call(t, cs, "daintree.attention", AttentionInput{SessionID: sess.SessionID, Acknowledge: &yes}, &out); err != nil {
		t.Fatalf("attention: %v", err)
	}
	fake.mu.Lock()
	acked = fake.acked
	fake.mu.Unlock()
	if !acked {
		t.Error("acknowledge:true must consume")
	}

	// The explicit ack is the supported path, and it is idempotent: a retry after an
	// ambiguous transport failure reports the id as unknown rather than failing.
	var ackOut AttentionAckOutput
	if err := call(t, cs, "daintree.attention.ack", AttentionAckInput{SessionID: sess.SessionID, EventIDs: []string{"evt_1", "evt_missing"}}, &ackOut); err != nil {
		t.Fatalf("attention.ack: %v", err)
	}
	if ackOut.Acknowledged != 1 {
		t.Errorf("acknowledged = %d, want 1", ackOut.Acknowledged)
	}
	if len(ackOut.Unknown) != 1 || ackOut.Unknown[0] != "evt_missing" {
		t.Errorf("unknown = %v, want [evt_missing]", ackOut.Unknown)
	}

	// An acknowledge-everything call would re-introduce exactly the loss this split
	// prevents, for rows the caller never read.
	if err := call(t, cs, "daintree.attention.ack", AttentionAckInput{SessionID: sess.SessionID}, &ackOut); err == nil {
		t.Error("attention.ack with no ids must be rejected, not treated as ack-everything")
	}
	fake.letFinish()
}

// TestCloseReleasesTheRuntime: an open session holds the project's owner lease, so a
// leaked one blocks every other process from opening that project.
func TestCloseReleasesTheRuntime(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	// Close with a turn STILL RUNNING: the turn must be cancelled and awaited before
	// the runtime is torn down, or Send would keep using a closed store.
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "long"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if err := call(t, cs, "daintree.session.close", SessionRefInput{SessionID: sess.SessionID}, &CloseOutput{}); err != nil {
		t.Fatalf("close: %v", err)
	}
	if !fake.isClosed() {
		t.Error("close must tear the runtime down and release the lease")
	}
	// The subtle half: close waits for the turn GOROUTINE, not merely for the run to
	// settle. The recorder settles a run on assistant:end while Send is still unwinding,
	// so waiting on the run would tear the App down under a live Send.
	fake.mu.Lock()
	early := fake.closedDuringSend
	fake.mu.Unlock()
	if early {
		t.Error("the runtime was closed while Send was still in flight")
	}
	// A second close, and any use of the id, must fail cleanly rather than panic.
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "x"}, &RunOutput{}); err == nil {
		t.Error("a closed session id must be rejected")
	}
}

// TestCloseAllReleasesEverySession: the server process must never exit still holding a
// project lease.
func TestCloseAllReleasesEverySession(t *testing.T) {
	var runtimes []*fakeRuntime
	var mu sync.Mutex
	n := 0
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		f := newFakeRuntime("ses_" + string(rune('a'+n)))
		runtimes = append(runtimes, f)
		return f, nil
	})
	for i := 0; i < 3; i++ {
		if _, err := reg.Open(context.Background(), OpenParams{}); err != nil {
			t.Fatalf("open: %v", err)
		}
	}
	for _, f := range runtimes {
		f.letFinish()
	}
	reg.CloseAll()
	for i, f := range runtimes {
		if !f.isClosed() {
			t.Errorf("runtime %d was not closed on shutdown", i)
		}
	}
}

// TestOpenFailureIsReportedNotSwallowed: a factory failure (busy project, signed out,
// unreadable key file) must reach the caller as a tool error with its message intact.
func TestOpenFailureIsReportedNotSwallowed(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return nil, errors.New("this project is already open in another assistant")
	})
	err := call(t, cs, "daintree.session.open", OpenInput{Project: "/repo"}, &SessionOutput{})
	if err == nil {
		t.Fatal("a factory failure must surface")
	}
	if !strings.Contains(err.Error(), "already open") {
		t.Errorf("the underlying reason must survive, got: %v", err)
	}
}

// TestDegradedMCPIsWarnedAbout: running without MCP is invisible in the answer text and
// is the commonest cause of a confusing run, so open says so explicitly.
func TestDegradedMCPIsWarnedAbout(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.facts.MCPConnected = false
	fake.facts.LogPath = ""
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	joined := strings.Join(sess.Warnings, " | ")
	if !strings.Contains(joined, "degraded local mode") {
		t.Errorf("a disconnected MCP must be warned about, got %q", joined)
	}
	if !strings.Contains(joined, "debugLog") {
		t.Errorf("logging-off must be warned about, got %q", joined)
	}
	fake.letFinish()
}

// TestSessionListReportsServerState: session.list is how a caller that lost its ids
// recovers, and how it learns the binary was rebuilt underneath it.
func TestSessionListReportsServerState(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	var out ListOutput
	if err := call(t, cs, "daintree.session.list", struct{}{}, &out); err != nil {
		t.Fatalf("session.list: %v", err)
	}
	if len(out.Sessions) != 1 || out.Sessions[0].SessionID != sess.SessionID {
		t.Fatalf("sessions = %+v", out.Sessions)
	}
	if out.Server.Version != "test" {
		t.Errorf("server.version = %q, want test", out.Server.Version)
	}
	fake.letFinish()
}

// TestUnknownSessionAndRunFailCleanly.
func TestUnknownSessionAndRunFailCleanly(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	if err := call(t, cs, "daintree.ask", AskInput{SessionID: "ses_nope", Prompt: "x"}, &RunOutput{}); err == nil {
		t.Error("an unknown session must be rejected")
	}
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: "mrun_nope"}, &RunOutput{}); err == nil {
		t.Error("an unknown run must be rejected")
	}
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "   "}, &RunOutput{}); err == nil {
		t.Error("an empty prompt must be rejected")
	}
	fake.letFinish()
}

// --- regressions for bugs the surface shipped with before review ---

// TestRunEventsReachTheCaller covers the Recorder→Run→poll projection: events emitted
// during a turn must arrive in what a CALLER sees, with correct stats.
//
// It does NOT prove the production adapter installs the sink — the fake receives it as
// an argument, so it would pass either way. That wiring is pinned separately by
// TestAdapterInstallsTheSinkBeforeSending, which is the gap that let the real bug ship.
func TestRunEventsReachTheCaller(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.script = func(sink agent.EventSink) {
		sink.AssistantStart()
		sink.SkillLoaded([]string{"Orchestration"})
		sink.SkillDecision(agent.SkillDecisionEvent{
			Active: []agent.SkillRef{
				{ID: "orchestration", Title: "Orchestration"},
				{ID: "bare_id"}, // no title: falls back to the id, never a blank row
			},
			Selector: agent.SkillSelectorOutcome{Ran: true, Degraded: true},
		})
		sink.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "terminal.list"})
		sink.ToolResult(agent.ToolResultEvent{ID: "c1", Name: "terminal.list", Result: domain.Ok("2 terminals", nil)})
		sink.ToolCall(agent.ToolCallEvent{ID: "c2", Name: "artifact.read"})
		sink.ToolResult(agent.ToolResultEvent{ID: "c2", Name: "artifact.read", Result: domain.Fail("ARTIFACT_NOT_FOUND", "gone")})
		sink.Usage(agent.UsageEvent{PromptTokens: 900, CompletionTokens: 40, TotalTokens: 940, ContextTokens: 900})
		sink.AssistantEnd("two terminals are idle", "")
	}
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "status?", Wait: true, WaitMs: 5000}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if len(run.Events) == 0 {
		t.Fatal("the run recorded NO events — the sink is not reaching the run")
	}
	seen := map[string]int{}
	for _, e := range run.Events {
		seen[e.Type]++
	}
	for _, want := range []string{"assistant:start", "skill:loaded", "skill:decision", "tool:call", "tool:result", "assistant:end"} {
		if seen[want] == 0 {
			t.Errorf("no %q event in the recorded timeline: %+v", want, run.Events)
		}
	}
	// The decision's projection: the ACTIVE set (not the delta) plus the one selector
	// fact a driving agent can act on. Ids and the rest of the telemetry stay on the
	// --json stream — this transcript is a digest, which is also why it drops tool args.
	var decision *Event
	for i := range run.Events {
		if run.Events[i].Type == "skill:decision" {
			decision = &run.Events[i]
		}
	}
	if decision == nil {
		t.Fatalf("no skill:decision event recorded: %+v", run.Events)
	}
	if len(decision.Skills) != 2 || decision.Skills[0] != "Orchestration" || decision.Skills[1] != "bare_id" {
		t.Errorf("decision skills = %v, want the active titles with an id fallback", decision.Skills)
	}
	if !decision.SkillsDegraded {
		t.Error("skillsDegraded lost; a fail-open round would read as a clean one")
	}
	// The marker must be OMITTED from unrelated events, not merely false — a decoded
	// bool cannot tell those apart, so assert on the raw JSON keys.
	for _, e := range run.Events {
		if e.Type == "skill:decision" {
			continue
		}
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("marshal %q event: %v", e.Type, err)
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(raw, &keys); err != nil {
			t.Fatalf("decode %q event: %v", e.Type, err)
		}
		if _, present := keys["skillsDegraded"]; present {
			t.Errorf("skillsDegraded present on a %q event: %s", e.Type, raw)
		}
	}

	if run.Stats.Rounds != 1 || run.Stats.ToolCalls != 2 || run.Stats.ToolErrors != 1 {
		t.Errorf("stats = %+v, want rounds=1 toolCalls=2 toolErrors=1", run.Stats)
	}
	if run.Stats.TotalTokens != 940 || run.Stats.ContextTokens != 900 {
		t.Errorf("token stats = %+v", run.Stats)
	}
	if run.Content != "two terminals are idle" {
		t.Errorf("content = %q", run.Content)
	}
}

// TestSentinelTurnFailureIsReportedAsError: agent.Session.Send returns a turn FAILURE
// as an ordinary string reply with a nil error, and reports the failure through the
// sink. Without the sink installed, such a run settled as `success` carrying an
// error-shaped sentinel as its answer — a backend outage reported as a good result.
func TestSentinelTurnFailureIsReportedAsError(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.script = func(sink agent.EventSink) {
		sink.AssistantStart()
		sink.Error("Can't reach the Daintree assistant backend")
	}
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	fake.letFinish()

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "x", Wait: true, WaitMs: 5000}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if run.Status != string(RunFailed) {
		t.Fatalf("status = %q, want error — a failed turn must not report success", run.Status)
	}
	if !strings.Contains(run.Error, "backend") {
		t.Errorf("error = %q, want the underlying reason", run.Error)
	}
}

// TestStaleInjectionsAreDiscarded: InjectPrompt only BUFFERS, and a turn interrupted
// past its final fold check leaves the message sitting there. Without a discard it would
// surface inside an unrelated LATER turn, after that turn's own prompt, as an
// instruction nobody issued for that work.
func TestStaleInjectionsAreDiscarded(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, reg := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	live, err := reg.Get(sess.SessionID)
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}

	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "first"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if err := call(t, cs, "daintree.inject", InjectInput{SessionID: sess.SessionID, Text: "late steer"}, &ActedOutput{}); err != nil {
		t.Fatalf("inject: %v", err)
	}
	before := fake.discardCount()
	if err := call(t, cs, "daintree.interrupt", SessionRefInput{SessionID: sess.SessionID}, &ActedOutput{}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if fake.discardCount() <= before {
		t.Error("interrupt must discard buffered injections, or the next turn inherits them")
	}

	// And the next ask discards again, covering an injection that missed its window
	// without an interrupt (a turn that simply finished first).
	//
	// interrupt is ASYNCHRONOUS by design: it cancels the turn's context and returns,
	// while the turn GOROUTINE is still unwinding Send, and only that goroutine clears
	// s.current. Until it does, Ask correctly reports ErrBusy — Send is single-flight in
	// the underlying assistant, so admitting a second turn early would be the bug. So
	// wait on the session's own liveness state before asking again, rather than assuming
	// the interrupt landed synchronously.
	waitIdle(t, live)
	before = fake.discardCount()
	fake.letFinish()
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "second"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	if fake.discardCount() <= before {
		t.Error("a new turn must start from a clean injection buffer")
	}
}

// waitForRecordedEvents blocks until a run has recorded at least n events. The recorder
// writes them on the turn goroutine, so a test that asserts on a window straight after
// an async ask is racing that goroutine's first scheduling.
func waitForRecordedEvents(t *testing.T, s *Session, runID string, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		if run, err := s.Run(runID); err == nil {
			evs, _, _, _, _, _, _, _ := run.Snapshot(0, 0)
			if got = len(evs); got >= n {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d recorded events; got %d", n, got)
}

// waitIdle blocks until the session has no turn in flight, i.e. the turn goroutine has
// finished unwinding and released the single-flight slot. Busy() is the same state Ask
// gates on, so this waits on the real signal rather than on elapsed time.
func waitIdle(t *testing.T, s *Session) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !s.Busy() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("timed out waiting for the session's turn to release; a cancelled turn is not unwinding")
}

// TestBlockingWaitHonoursCallerCancellation: the SDK waits for in-flight handlers before
// its Run returns, so a wait that ignored cancellation would hold the server — and every
// session's project lease — open for the whole budget after the client had gone.
func TestBlockingWaitHonoursCallerCancellation(t *testing.T) {
	run := NewRun("mrun_x", "ses_x", "prompt", func() {})
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		defer close(done)
		waitForSettle(ctx, run, int(maxBlockWait/time.Millisecond))
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForSettle ignored caller cancellation; it would pin the server open for its whole budget")
	}

	// The long POLL wait must honour it too, and for the same reason.
	pctx, pcancel := context.WithCancel(context.Background())
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		waitForChange(pctx, run, 0, run.Revision(), int(maxBlockWait/time.Millisecond))
	}()
	pcancel()
	select {
	case <-polled:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForChange ignored caller cancellation")
	}
}

// TestPollWaitWakesOnProgressNotOnlyCompletion: waiting for the run to FINISH made a
// 60s poll sit through arriving content, tools starting and finishing, and — worst — the
// turn becoming blocked on an approval, reporting none of it until the budget expired.
func TestPollWaitWakesOnProgressNotOnlyCompletion(t *testing.T) {
	run := NewRun("mrun_p", "ses_p", "prompt", func() {})

	rev := run.Revision()
	woke := make(chan struct{})
	go func() {
		defer close(woke)
		run.WaitForChange(context.Background(), 0, rev, 10*time.Second)
	}()
	// An event, not a settlement. The run stays firmly in `running`.
	run.append(Event{Type: "assistant:content", Text: "working on it"})
	select {
	case <-woke:
	case <-time.After(5 * time.Second):
		t.Fatal("a long poll slept through a new event; waitMs still means 'wait for finish'")
	}
	if run.Status() != RunRunning {
		t.Fatalf("status = %q, want the run still running", run.Status())
	}

	// A run PARKED on an approval emits no events of its own, so the change signal is
	// the only thing that can report it before the budget expires.
	// Revision captured before the goroutine starts, exactly as the poll handler does:
	// the Touch below may well land before the waiter reaches its select, and a design
	// that lost that wakeup would sleep out the whole budget.
	parkedRev := run.Revision()
	parked := make(chan struct{})
	go func() {
		defer close(parked)
		run.WaitForChange(context.Background(), 99, parkedRev, 10*time.Second)
	}()
	run.Touch()
	select {
	case <-parked:
	case <-time.After(5 * time.Second):
		t.Fatal("a long poll slept through a parked approval")
	}
}

// TestPollWaitReturnsImmediatelyWhenAlreadyFresh: a caller polling with a stale sinceSeq
// must not be parked for its whole budget over events it has not read yet.
func TestPollWaitReturnsImmediatelyWhenAlreadyFresh(t *testing.T) {
	run := NewRun("mrun_f", "ses_f", "prompt", func() {})
	run.append(Event{Type: "assistant:content", Text: "already said this"})

	done := make(chan struct{})
	go func() {
		defer close(done)
		run.WaitForChange(context.Background(), 0, run.Revision(), 10*time.Second)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waited for new events when unread ones were already buffered")
	}
}

// TestDuplicateSessionIDIsRejected: a map overwrite would lose the only reference to a
// live runtime while it still held the project's exclusive lease — permanently, since
// neither Close nor CloseAll could then reach it.
func TestDuplicateSessionIDIsRejected(t *testing.T) {
	var built []*fakeRuntime
	var mu sync.Mutex
	reg := NewUnconfinedRegistry(context.Background(), func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		f := newFakeRuntime("ses_same")
		f.letFinish()
		mu.Lock()
		built = append(built, f)
		mu.Unlock()
		return f, nil
	})
	t.Cleanup(reg.CloseAll)

	if _, err := reg.Open(context.Background(), OpenParams{}); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := reg.Open(context.Background(), OpenParams{}); err == nil {
		t.Fatal("a colliding session id must be rejected, not silently overwrite the live one")
	}
	// The rejected runtime must be closed, or its lease leaks exactly as an overwrite
	// would have leaked the first one's.
	mu.Lock()
	second := built[1]
	mu.Unlock()
	if !second.isClosed() {
		t.Error("the rejected runtime must be torn down so its lease is released")
	}
}

// TestServerInfoRidesEverySessionResponse: a caller that only ever opens a session must
// still learn its binary went stale, without parsing prose.
func TestServerInfoRidesEverySessionResponse(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)
	if sess.Server.Version != "test" {
		t.Errorf("session.open must carry structured server state, got %+v", sess.Server)
	}
	fake.letFinish()
}

// TestEveryRunResponseSaysWhatToDoNext: the caller is a language model, and the two
// pathologies a polling surface invites — hammering poll, and treating a running turn as
// a finished one — are both prevented by stating the next step rather than leaving it to
// be inferred from a status string.
func TestEveryRunResponseSaysWhatToDoNext(t *testing.T) {
	cases := map[RunStatus][]string{
		RunRunning:   {"daintree.poll", "waitMs"},
		RunSucceeded: {"Finished"},
		RunCancelled: {"still usable"},
		RunFailed:    {"Read `error` before retrying"},
	}
	for status, wants := range cases {
		t.Run(string(status), func(t *testing.T) {
			got := nextAction(RunOutput{Status: string(status)})
			if got == "" {
				t.Fatalf("no nextAction for status %q", status)
			}
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("nextAction(%s) = %q, want it to mention %q", status, got, want)
				}
			}
		})
	}
	// A finished run that left background work behind must point at attention, or the
	// caller reports the job done while agents are still running.
	withAsync := nextAction(RunOutput{Status: string(RunSucceeded), AsyncOperations: []AsyncOperation{{ID: "asy_1", Status: "accepted"}}})
	if !strings.Contains(withAsync, "daintree.attention") {
		t.Errorf("a run with pending async work must point at attention, got %q", withAsync)
	}
}

// TestErrorsNameTheRemedy: an error a model cannot act on becomes a retry loop.
func TestErrorsNameTheRemedy(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
	sess := openSession(t, cs)

	if err := call(t, cs, "daintree.poll", PollInput{SessionID: "ses_nope", RunID: "x"}, &RunOutput{}); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "daintree.session.open") {
		t.Errorf("an unknown session must say how to get one, got: %v", err)
	}

	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: "mrun_nope"}, &RunOutput{}); err == nil {
		t.Fatal("expected an error")
	} else if !strings.Contains(err.Error(), "daintree.ask") {
		t.Errorf("an unknown run must point at where runIds come from, got: %v", err)
	}

	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "a"}, &RunOutput{}); err != nil {
		t.Fatalf("first ask: %v", err)
	}
	err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "b"}, &RunOutput{})
	if err == nil {
		t.Fatal("expected a busy error")
	}
	// "do not retry" is the load-bearing phrase: a bare "busy" invites exactly the loop.
	for _, want := range []string{"do not retry", "daintree.inject", "daintree.interrupt"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the busy error must mention %q, got: %v", want, err)
		}
	}
	fake.letFinish()
}

// TestApprovalFlowThroughTheTools walks the whole "ask" mode as a caller sees it: a
// mutating tool parks, the run reports itself BLOCKED rather than merely slow, the
// caller approves, and the call proceeds.
func TestApprovalFlowThroughTheTools(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 0)
	fake.facts.ApprovalMode = string(ApprovalDelegate)
	fake.confirmInSend = &ApprovalRequest{
		Tool: "git.push", Risk: domain.RiskGit,
		Consequence: "pushes 3 commits to origin/main",
	}
	fake.script = func(sink agent.EventSink) {
		sink.AssistantStart()
		sink.ToolCall(agent.ToolCallEvent{ID: "c1", Name: "git.push"})
	}
	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "push it"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	waitForPending(t, fake.approvals, 1)

	// The list tool shows it with everything needed to decide.
	var list ApprovalsOutput
	if err := call(t, cs, "daintree.approvals", SessionRefInput{SessionID: sess.SessionID}, &list); err != nil {
		t.Fatalf("approvals: %v", err)
	}
	if list.Count != 1 || list.Mode != string(ApprovalDelegate) {
		t.Fatalf("approvals = %+v", list)
	}
	if list.Pending[0].Consequence == "" || list.Pending[0].Risk != string(domain.RiskGit) {
		t.Errorf("a caller cannot decide without risk + consequence: %+v", list.Pending[0])
	}

	// And a poll reports the run as BLOCKED, not merely running — polling harder would
	// never move it.
	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(polled.PendingApprovals) != 1 {
		t.Fatalf("the poll must surface the blocking approval, got %+v", polled)
	}
	if !strings.Contains(polled.NextAction, "BLOCKED") || !strings.Contains(polled.NextAction, "daintree.approve") {
		t.Errorf("nextAction must say the run is blocked and how to unblock it, got %q", polled.NextAction)
	}

	var acted ActedOutput
	if err := call(t, cs, "daintree.approve", ApproveInput{
		SessionID: sess.SessionID, ApprovalID: list.Pending[0].ID, Approve: true,
	}, &acted); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !acted.Acted {
		t.Fatalf("approve reported no action: %+v", acted)
	}
	select {
	case ok := <-fake.confirmResult:
		if !ok {
			t.Error("the approved call was refused")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the parked dispatch was never released")
	}
	fake.letFinish()
}

// TestApprovingASettledApprovalExplainsItself: a caller that answers just after the
// timer fired must learn what happened, not get a bare "not found".
func TestApprovingASettledApprovalExplainsItself(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 0)
	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)
	fake.letFinish()

	go fake.approvals.Confirm(context.Background(), ApprovalRequest{Tool: "terminal.run"})
	pa := waitForPending(t, fake.approvals, 1)[0]
	fake.approvals.Resolve(pa.ID, DecisionTimeout)

	var acted ActedOutput
	if err := call(t, cs, "daintree.approve", ApproveInput{
		SessionID: sess.SessionID, ApprovalID: pa.ID, Approve: true,
	}, &acted); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if acted.Acted {
		t.Error("a settled approval cannot be acted on again")
	}
	if !strings.Contains(acted.Message, "timeout") {
		t.Errorf("the message must say WHY it is gone, got %q", acted.Message)
	}

	// A never-real id is a different case and must be a hard error.
	if err := call(t, cs, "daintree.approve", ApproveInput{
		SessionID: sess.SessionID, ApprovalID: "apr_nope", Approve: true,
	}, &acted); err == nil {
		t.Error("an unknown approval id must be an error")
	}
}

// TestDeclineModeExplainsWhyNothingIsPending: "0 pending" otherwise reads as "nothing
// wanted approval", when the truth is that everything was refused outright.
func TestDeclineModeExplainsWhyNothingIsPending(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)
	fake.letFinish()

	var list ApprovalsOutput
	if err := call(t, cs, "daintree.approvals", SessionRefInput{SessionID: sess.SessionID}, &list); err != nil {
		t.Fatalf("approvals: %v", err)
	}
	if list.Count != 0 || list.Mode != string(ApprovalDecline) {
		t.Fatalf("approvals = %+v", list)
	}
	if !strings.Contains(list.Note, "approvals") {
		t.Errorf("an empty list in decline mode must explain itself, got %q", list.Note)
	}
}

// TestSessionCloseWaitsForAParkedDispatch is the deadlock guard, and it only means
// anything because the fake parks ON THE SEND GOROUTINE. Detaching the confirm to its
// own goroutine — as an earlier version of this test did — would let turns.Wait() return
// while a dispatch was still blocked, so the test would pass with RejectAll moved after
// the wait, i.e. with the deadlock reintroduced.
func TestSessionCloseWaitsForAParkedDispatch(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 0)
	fake.confirmInSend = &ApprovalRequest{Tool: "git.push", Risk: domain.RiskGit}

	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "push"}, &RunOutput{}); err != nil {
		t.Fatalf("ask: %v", err)
	}
	waitForPending(t, fake.approvals, 1)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = call(t, cs, "daintree.session.close", SessionRefInput{SessionID: sess.SessionID}, &CloseOutput{})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session.close deadlocked on a parked approval")
	}
	select {
	case ok := <-fake.confirmResult:
		if ok {
			t.Error("teardown must deny, never allow")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("close left the dispatch parked")
	}
	if !fake.isClosed() {
		t.Error("the runtime must be torn down after the dispatch unwound")
	}
}

// TestPendingApprovalsAreScopedToTheirRun: a blanket match would report every completed
// run in the session as BLOCKED whenever any turn was parked.
func TestPendingApprovalsAreScopedToTheirRun(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 0)
	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)

	// Run A completes cleanly.
	fake.letFinish()
	var runA RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "first", Wait: true, WaitMs: 5000}, &runA); err != nil {
		t.Fatalf("ask A: %v", err)
	}

	// An approval is raised naming a DIFFERENT run.
	go fake.approvals.Confirm(context.Background(), ApprovalRequest{Tool: "git.push", RunID: "mrun_other"})
	waitForPending(t, fake.approvals, 1)

	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: runA.RunID}, &polled); err != nil {
		t.Fatalf("poll A: %v", err)
	}
	if len(polled.PendingApprovals) != 0 {
		t.Errorf("run A must not report another run's approval as blocking it: %+v", polled.PendingApprovals)
	}
	if strings.Contains(polled.NextAction, "BLOCKED") {
		t.Errorf("a finished run must not be reported as blocked: %q", polled.NextAction)
	}
	fake.approvals.RejectAll()
}

// TestProductionApprovalsCarryTheirRunID: the scoping above is only correct if the run
// id actually reaches the approval, which is the adapter's job.
func TestProductionApprovalsCarryTheirRunID(t *testing.T) {
	fake := newFakeRuntime("ses_test")
	fake.approvals = NewApprovals(ApprovalDelegate, 0)
	fake.confirmInSend = &ApprovalRequest{Tool: "git.push"}
	cs, _ := connectWithApprovals(t, fake)
	sess := openSession(t, cs)

	var run RunOutput
	if err := call(t, cs, "daintree.ask", AskInput{SessionID: sess.SessionID, Prompt: "push"}, &run); err != nil {
		t.Fatalf("ask: %v", err)
	}
	pa := waitForPending(t, fake.approvals, 1)[0]
	if pa.RunID != run.RunID {
		t.Errorf("approval RunID = %q, want the run that raised it (%q)", pa.RunID, run.RunID)
	}
	// And the run reports itself blocked by it.
	var polled RunOutput
	if err := call(t, cs, "daintree.poll", PollInput{SessionID: sess.SessionID, RunID: run.RunID}, &polled); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if len(polled.PendingApprovals) != 1 {
		t.Fatalf("the run must surface its own blocking approval, got %+v", polled.PendingApprovals)
	}
	fake.approvals.RejectAll()
	fake.letFinish()
}

// connectWithApprovals wires the resource templates too, so approval tests exercise the
// same server surface a real client sees.
func connectWithApprovals(t *testing.T, fake *fakeRuntime) (*mcp.ClientSession, *Registry) {
	t.Helper()
	return connectWithResources(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) { return fake, nil })
}

// TestSessionOpenCarriesProjectIdentity: session.open exists so a client that cannot
// restart this process can repoint it, and project identity is exactly the thing worth
// repointing — projectId also decides which per-project state directory the session
// opens, so a dropped field would quietly share one project's database with another.
func TestSessionOpenCarriesProjectIdentity(t *testing.T) {
	var got OpenParams
	cs, _ := connect(t, func(_, _ context.Context, p OpenParams) (Runtime, error) {
		got = p
		return newFakeRuntime("ses_identity"), nil
	})
	in := OpenInput{Project: "/repo", ProjectID: "proj_fake_test", WindowID: "win_fake_test"}
	if err := call(t, cs, "daintree.session.open", in, &SessionOutput{}); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if got.ProjectID != "proj_fake_test" || got.WindowID != "win_fake_test" {
		t.Errorf("OpenParams identity = %q/%q, want the input's", got.ProjectID, got.WindowID)
	}
}

// TestSessionOpenSchemaDocumentsProjectIdentity: the schema is auto-derived from the
// struct tags, so a field added without its json tag would be invisible to a caller
// while still compiling. A caller that cannot see the field cannot pass it.
func TestSessionOpenSchemaDocumentsProjectIdentity(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_test"), nil
	})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	for _, tool := range res.Tools {
		if tool.Name != "daintree.session.open" {
			continue
		}
		// The PROPERTIES map, not a substring of the marshalled blob: another field's
		// description could mention "projectId" in prose and satisfy a text search while
		// the property itself was missing.
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal schema: %v", err)
		}
		// Decoded loosely on purpose: an optional field like debugLog carries a UNION
		// type (["null","boolean"]), so a struct pinning `type` to a string fails to
		// decode the whole schema over a field this test does not even look at.
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode schema: %v (raw %s)", err, raw)
		}
		for _, want := range []string{"projectId", "windowId"} {
			prop, ok := schema.Properties[want]
			if !ok {
				t.Errorf("session.open schema has no %q property:\n%s", want, raw)
				continue
			}
			if typ, _ := prop["type"].(string); typ != "string" {
				t.Errorf("%s type = %v, want string", want, prop["type"])
			}
			// A property a model cannot understand is barely better than a missing one.
			if desc, _ := prop["description"].(string); strings.TrimSpace(desc) == "" {
				t.Errorf("%s has no description — a caller would have to guess what it does", want)
			}
		}
		return
	}
	t.Fatal("daintree.session.open is not offered")
}

// The two headless surfaces must not drift: a runbook you can pin from argv you must be
// able to pin here. That starts with the argument being discoverable — a caller that
// cannot see the field in the schema has to read our source to find it, which is the
// exact problem --list-skills exists to end.
func TestSessionOpenAdvertisesTheSkillsArgument(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_test"), nil
	})
	res, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var open *mcp.Tool
	for _, tool := range res.Tools {
		if tool.Name == "daintree.session.open" {
			open = tool
		}
	}
	if open == nil || open.InputSchema == nil {
		t.Fatal("daintree.session.open is missing or has no schema")
	}
	b, err := json.Marshal(open.InputSchema)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	schema := string(b)
	if !strings.Contains(schema, `"skills"`) {
		t.Fatalf("session.open does not advertise a skills argument:\n%s", schema)
	}
	// A caller told to pass an id needs to be told where ids come from, or the field is
	// only usable by someone who already read the backend's source.
	if !strings.Contains(schema, "--list-skills") {
		t.Fatalf("the skills argument does not say how to discover an id:\n%s", schema)
	}
}

// The pins must reach the factory verbatim, and nil must stay nil so the merge below it
// can still tell "omitted" from "explicitly cleared".
func TestSessionOpenPassesSkillsThrough(t *testing.T) {
	var got OpenParams
	// A distinct id per open: the registry (rightly) refuses to reopen one that is
	// already live, and this test deliberately opens twice to compare "given" against
	// "omitted".
	opened := 0
	cs, _ := connect(t, func(_, _ context.Context, p OpenParams) (Runtime, error) {
		got = p
		opened++
		return newFakeRuntime(fmt.Sprintf("ses_test_%d", opened)), nil
	})
	var out SessionOutput
	if err := call(t, cs, "daintree.session.open",
		OpenInput{Project: "/repo", Skills: []string{"b.two", "a.one"}}, &out); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if len(got.Skills) != 2 || got.Skills[0] != "b.two" || got.Skills[1] != "a.one" {
		t.Fatalf("Skills = %v, want [b.two a.one] in order", got.Skills)
	}

	got = OpenParams{}
	if err := call(t, cs, "daintree.session.open", OpenInput{Project: "/repo"}, &out); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if got.Skills != nil {
		t.Fatalf("an omitted skills argument must stay nil so it can inherit the process default, got %#v", got.Skills)
	}
}

// Trimming a blank entry away would open a session pinned to LESS than was asked for —
// the same silent underrun `--skill=` is rejected for at the argument boundary.
func TestSessionOpenRejectsABlankSkillID(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		return newFakeRuntime("ses_test"), nil
	})
	var out SessionOutput
	err := call(t, cs, "daintree.session.open", OpenInput{Project: "/repo", Skills: []string{"a.one", "  "}}, &out)
	if err == nil {
		t.Fatal("a blank skills entry must fail the open rather than being silently dropped")
	}
	if !strings.Contains(err.Error(), "skills[1]") {
		t.Fatalf("the error must name which entry was blank: %v", err)
	}
}

// The non-fatal half of the pin preflight (pinning supported, no catalog to check
// against) has to reach the caller, and SessionOutput.Warnings is the field they already
// read for "conditions that will silently ruin a run".
func TestSessionOpenSurfacesThePinPreflightAdvisory(t *testing.T) {
	cs, _ := connect(t, func(_, _ context.Context, _ OpenParams) (Runtime, error) {
		rt := newFakeRuntime("ses_test")
		rt.facts.PinnedSkills = []string{"a.one"}
		rt.facts.PinPreflightWarning = "this backend advertises no skill catalog"
		return rt, nil
	})
	out := openSession(t, cs)
	if !strings.Contains(strings.Join(out.Warnings, " | "), "no skill catalog") {
		t.Fatalf("the pin advisory did not reach the caller: %v", out.Warnings)
	}
	if len(out.Facts.PinnedSkills) != 1 || out.Facts.PinnedSkills[0] != "a.one" {
		t.Fatalf("a caller that inherited a server-level pin must be able to see it: %+v", out.Facts)
	}
}

// The nil-versus-empty distinction has to survive the SDK's decode, not just
// applySliceIfSet's unit test — an explicit `"skills": []` is a caller CLEARING the
// server's `--skill` defaults for this session, and if it arrived as nil the server
// would do the opposite of what was asked and pin runbooks the caller declined.
//
// Sent as raw arguments deliberately: a typed OpenInput{Skills: []string{}} cannot
// express this, because the field's own `omitempty` drops an empty slice on the way out.
func TestSessionOpenDistinguishesEmptySkillsFromOmitted(t *testing.T) {
	var got OpenParams
	opened := 0
	cs, _ := connect(t, func(_, _ context.Context, p OpenParams) (Runtime, error) {
		got = p
		opened++
		return newFakeRuntime(fmt.Sprintf("ses_raw_%d", opened)), nil
	})

	var out SessionOutput
	if err := call(t, cs, "daintree.session.open",
		map[string]any{"project": "/repo", "skills": []any{}}, &out); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if got.Skills == nil {
		t.Fatal(`an explicit "skills": [] decoded to nil — the session cannot clear a server-level default`)
	}
	if len(got.Skills) != 0 {
		t.Fatalf("Skills = %v, want an empty (but non-nil) slice", got.Skills)
	}

	got = OpenParams{}
	if err := call(t, cs, "daintree.session.open", map[string]any{"project": "/repo"}, &out); err != nil {
		t.Fatalf("session.open: %v", err)
	}
	if got.Skills != nil {
		t.Fatalf("an omitted skills argument must stay nil so it inherits the default, got %#v", got.Skills)
	}
}

// terminalTrackingSink forwards every event and notes whether a TERMINAL one went past.
//
// It exists because the server now treats "returned cleanly, emitted no terminal event"
// as RUN_EVENT_STREAM_INCOMPLETE — the shape an unwired sink produces. A fake runtime has
// to model a correct runtime (exactly one terminal event) rather than the bug, and a
// script that emits its own must not then get a second one appended.
type terminalTrackingSink struct {
	agent.EventSink
	sawTerminal bool
}

func (s *terminalTrackingSink) AssistantEnd(content, reasoning string) {
	s.sawTerminal = true
	s.EventSink.AssistantEnd(content, reasoning)
}

func (s *terminalTrackingSink) AssistantCancelled(content string) {
	s.sawTerminal = true
	s.EventSink.AssistantCancelled(content)
}

// Error is terminal too: Recorder.Error proposes RunFailed, because a turn failure is a
// sentinel reply rather than a returned error. Without it a fake whose script emits an
// error got an AssistantEnd appended on top, producing a double-terminal stream no real
// runtime can produce — and letting a failure test pass for the wrong reason.
func (s *terminalTrackingSink) Error(message string) {
	s.sawTerminal = true
	s.EventSink.Error(message)
}
