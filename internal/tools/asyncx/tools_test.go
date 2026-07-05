package asyncx

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

/* --------------------------------- fakes ---------------------------------- */

type fakeReader struct {
	connected bool
	live      []string
	listOK    bool
}

func (r *fakeReader) Connected() bool { return r.connected }
func (r *fakeReader) ListTerminals(context.Context) ([]string, bool) {
	return r.live, r.listOK
}

type fakeSender struct {
	sent [][2]string
	err  error
}

func (s *fakeSender) SendCommand(_ context.Context, terminalID, command string) error {
	if s.err != nil {
		return s.err
	}
	s.sent = append(s.sent, [2]string{terminalID, command})
	return nil
}

type fakeCoordinator struct {
	started      bool
	registered   []domain.AsyncInvocationRecord
	deregistered []string
	registerErr  error
}

func (c *fakeCoordinator) Started() bool { return c.started }
func (c *fakeCoordinator) Register(rec domain.AsyncInvocationRecord, _ []string) error {
	if c.registerErr != nil {
		return c.registerErr
	}
	c.registered = append(c.registered, rec)
	return nil
}
func (c *fakeCoordinator) Deregister(id string) { c.deregistered = append(c.deregistered, id) }

type fakeStore struct {
	rows    map[string]*domain.AsyncInvocationRecord
	nextID  int
	liveCap int // CountLive returns this when >= 0 (else the real live count)
}

func newFakeStore() *fakeStore {
	return &fakeStore{rows: map[string]*domain.AsyncInvocationRecord{}, liveCap: -1}
}

func (s *fakeStore) InsertAsyncInvocation(rec domain.AsyncInvocationRecord) (domain.AsyncInvocationRecord, error) {
	if rec.ID == "" {
		s.nextID++
		rec.ID = "asy_test" + string(rune('0'+s.nextID))
	}
	if rec.Status == "" {
		rec.Status = domain.AsyncStarting
	}
	if rec.GroupID == "" {
		rec.GroupID = rec.ID
	}
	cp := rec
	s.rows[rec.ID] = &cp
	return rec, nil
}

func (s *fakeStore) GetAsyncInvocation(id string) (*domain.AsyncInvocationRecord, error) {
	if r, ok := s.rows[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, nil
}

func (s *fakeStore) ListAsyncInvocations(string) ([]domain.AsyncInvocationRecord, error) {
	var out []domain.AsyncInvocationRecord
	for _, r := range s.rows {
		out = append(out, *r)
	}
	return out, nil
}

func (s *fakeStore) ListLiveAsyncInvocations() ([]domain.AsyncInvocationRecord, error) {
	var out []domain.AsyncInvocationRecord
	for _, r := range s.rows {
		if !r.Status.IsTerminal() {
			out = append(out, *r)
		}
	}
	return out, nil
}

func (s *fakeStore) CountLiveAsyncInvocations() (int, error) {
	if s.liveCap >= 0 {
		return s.liveCap, nil
	}
	live, _ := s.ListLiveAsyncInvocations()
	return len(live), nil
}

func (s *fakeStore) ClaimLiveAsyncInvocation(id string, patch map[string]any) (bool, error) {
	r, ok := s.rows[id]
	if !ok || r.Status.IsTerminal() {
		return false, nil
	}
	if st, ok := patch["status"].(string); ok {
		r.Status = domain.AsyncStatus(st)
	}
	if er, ok := patch["endedReason"].(string); ok {
		r.EndedReason = &er
	}
	return true, nil
}

func testDeps() (Deps, *fakeReader, *fakeSender, *fakeCoordinator, *fakeStore) {
	reader := &fakeReader{connected: true, live: []string{"terminal-aaaa1111"}, listOK: true}
	sender := &fakeSender{}
	coord := &fakeCoordinator{started: true}
	store := newFakeStore()
	deps := Deps{
		Reader: reader, Sender: sender, Coordinator: coord, Store: store,
		SessionID: "ses_test", Now: func() int64 { return 42_000 },
	}
	return deps, reader, sender, coord, store
}

func toolByName(t *testing.T, deps Deps, name string) tools.Tool {
	t.Helper()
	for _, tool := range Tools(deps) {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %s not found", name)
	return tools.Tool{}
}

func handle(t *testing.T, tool tools.Tool, args string, tctx *tools.ToolContext) domain.ToolResult {
	t.Helper()
	raw := json.RawMessage(args)
	if tool.Decode != nil {
		parsed, err := tool.Decode(raw)
		if err != nil {
			t.Fatalf("decode rejected %s: %v", args, err)
		}
		raw = parsed
	}
	return tool.Handle(context.Background(), raw, tctx)
}

/* ------------------------------- run.async -------------------------------- */

func TestRunAsyncHappyPath(t *testing.T) {
	deps, _, sender, coord, store := testDeps()
	tool := toolByName(t, deps, "terminal.run.async")
	tctx := &tools.ToolContext{RunID: "run_777", ToolCallID: "call_1"}

	res := handle(t, tool, `{"terminalId":"terminal-aaaa1111","command":"npm test"}`, tctx)
	if !res.Ok {
		t.Fatalf("run.async failed: %+v", res)
	}
	if res.Async == nil {
		t.Fatal("result missing the typed async handle")
	}
	if res.Async.ToolName != "terminal.run.async" || res.Async.GroupID != "run_777" {
		t.Errorf("handle = %+v", res.Async)
	}
	if len(sender.sent) != 1 || sender.sent[0][0] != "terminal-aaaa1111" || sender.sent[0][1] != "npm test" {
		t.Errorf("send calls = %v", sender.sent)
	}
	if len(coord.registered) != 1 {
		t.Fatalf("coordinator registrations = %d, want 1", len(coord.registered))
	}
	rec := store.rows[res.Async.ID]
	if rec == nil || rec.Status != domain.AsyncRunning {
		t.Errorf("ledger row = %+v, want running", rec)
	}
	if rec.Command == nil || *rec.Command != "npm test" {
		t.Errorf("command not persisted: %+v", rec)
	}
	if !strings.Contains(res.Summary, "asynchronously") {
		t.Errorf("summary %q should state the async contract", res.Summary)
	}
}

func TestRunAsyncSendFailureFinalizesRow(t *testing.T) {
	deps, _, sender, coord, store := testDeps()
	sender.err = context.DeadlineExceeded
	tool := toolByName(t, deps, "terminal.run.async")

	res := handle(t, tool, `{"terminalId":"terminal-aaaa1111","command":"npm test"}`, &tools.ToolContext{})
	if res.Ok {
		t.Fatal("send failure must fail the call")
	}
	if res.Error == nil || res.Error.Code != codeSendFailed {
		t.Errorf("error = %+v, want SEND_FAILED", res.Error)
	}
	if len(coord.registered) != 0 {
		t.Error("a failed send must never register with the coordinator")
	}
	for _, r := range store.rows {
		if r.Status != domain.AsyncFailed {
			t.Errorf("ledger row after failed send = %q, want failed", r.Status)
		}
	}
}

func TestRunAsyncResolvesPrefixIDs(t *testing.T) {
	deps, reader, sender, _, _ := testDeps()
	reader.live = []string{"terminal-5284bfef-3d11-424c-90cb-136f24046295"}
	tool := toolByName(t, deps, "terminal.run.async")
	res := handle(t, tool, `{"terminalId":"terminal-5284bfef","command":"go test"}`, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("prefix id should resolve: %+v", res)
	}
	if sender.sent[0][0] != "terminal-5284bfef-3d11-424c-90cb-136f24046295" {
		t.Errorf("sent to %q, want the canonical id", sender.sent[0][0])
	}
}

func TestAsyncPreflightGates(t *testing.T) {
	// Coordinator not started → ASYNC_UNAVAILABLE.
	deps, _, _, coord, _ := testDeps()
	coord.started = false
	tool := toolByName(t, deps, "terminal.run.async")
	res := handle(t, tool, `{"terminalId":"terminal-aaaa1111","command":"x"}`, &tools.ToolContext{})
	if res.Ok || res.Error.Code != codeAsyncUnavailable {
		t.Errorf("want ASYNC_UNAVAILABLE, got %+v", res.Error)
	}

	// Live cap reached → ASYNC_LIMIT.
	deps2, _, _, _, store2 := testDeps()
	store2.liveCap = maxLiveInvocations
	tool2 := toolByName(t, deps2, "terminal.await.async")
	res2 := handle(t, tool2, `{"terminalIds":["terminal-aaaa1111"]}`, &tools.ToolContext{})
	if res2.Ok || res2.Error.Code != codeAsyncLimit {
		t.Errorf("want ASYNC_LIMIT, got %+v", res2.Error)
	}

	// MCP down → MCP_UNAVAILABLE.
	deps3, reader3, _, _, _ := testDeps()
	reader3.connected = false
	tool3 := toolByName(t, deps3, "terminal.run.async")
	res3 := handle(t, tool3, `{"terminalId":"terminal-aaaa1111","command":"x"}`, &tools.ToolContext{})
	if res3.Ok || res3.Error.Code != codeMCPUnavailable {
		t.Errorf("want MCP_UNAVAILABLE, got %+v", res3.Error)
	}
}

func TestRunAsyncValidation(t *testing.T) {
	deps, _, _, _, _ := testDeps()
	tool := toolByName(t, deps, "terminal.run.async")
	for _, bad := range []string{
		`{"terminalId":"","command":"x"}`,
		`{"terminalId":"t","command":"  "}`,
		`{"terminalId":"t","command":"x","timeoutMs":1}`,
		`{"terminalId":"t","command":"x","timeoutMs":99999999}`,
	} {
		if _, err := tool.Decode(json.RawMessage(bad)); err == nil {
			t.Errorf("decode accepted %s", bad)
		}
	}
}

/* ------------------------------ await.async ------------------------------- */

func TestAwaitAsyncHappyPathAndValidation(t *testing.T) {
	deps, reader, _, coord, store := testDeps()
	reader.live = []string{"terminal-aaaa1111", "terminal-bbbb2222"}
	tool := toolByName(t, deps, "terminal.await.async")

	res := handle(t, tool, `{"terminalIds":["terminal-aaaa1111","terminal-bbbb2222"],"title":"cohort"}`, &tools.ToolContext{RunID: "run_9"})
	if !res.Ok || res.Async == nil {
		t.Fatalf("await.async failed: %+v", res)
	}
	if len(res.Async.TerminalIDs) != 2 {
		t.Errorf("handle terminals = %v", res.Async.TerminalIDs)
	}
	if len(coord.registered) != 1 {
		t.Errorf("registrations = %d", len(coord.registered))
	}
	if rec := store.rows[res.Async.ID]; rec == nil || rec.Command != nil {
		t.Errorf("watch-only row must have no command: %+v", rec)
	}

	// Duplicates rejected at decode.
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":["a","a"]}`)); err == nil {
		t.Error("duplicate ids must be rejected")
	}
	if _, err := tool.Decode(json.RawMessage(`{"terminalIds":[]}`)); err == nil {
		t.Error("empty ids must be rejected")
	}
}

/* ------------------------------ list / cancel ----------------------------- */

func TestAsyncListAndCancel(t *testing.T) {
	deps, _, _, coord, store := testDeps()
	rec, _ := store.InsertAsyncInvocation(domain.AsyncInvocationRecord{
		ToolName: "terminal.run.async", Title: "npm test", SessionID: "ses_test",
		TerminalIdsJson: `["terminal-aaaa1111"]`, Status: domain.AsyncRunning, ExpiresAt: 99_000,
	})

	listTool := toolByName(t, deps, "async.list")
	res := handle(t, listTool, `{}`, &tools.ToolContext{})
	if !res.Ok {
		t.Fatalf("list failed: %+v", res)
	}
	payload := res.Result.(map[string]any)
	if payload["live"].(int) != 1 {
		t.Errorf("live = %v, want 1", payload["live"])
	}

	cancelTool := toolByName(t, deps, "async.cancel")
	cres := handle(t, cancelTool, `{"asyncId":"`+rec.ID+`"}`, &tools.ToolContext{})
	if !cres.Ok {
		t.Fatalf("cancel failed: %+v", cres)
	}
	if !strings.Contains(cres.Summary, "NOT killed") {
		t.Errorf("cancel summary %q must state the process is untouched", cres.Summary)
	}
	if store.rows[rec.ID].Status != domain.AsyncCancelled {
		t.Errorf("row after cancel = %q", store.rows[rec.ID].Status)
	}
	if len(coord.deregistered) != 1 || coord.deregistered[0] != rec.ID {
		t.Errorf("deregistered = %v", coord.deregistered)
	}

	// Cancelling again: already terminal → informative Ok, no second transition.
	cres2 := handle(t, cancelTool, `{"asyncId":"`+rec.ID+`"}`, &tools.ToolContext{})
	if !cres2.Ok || !strings.Contains(cres2.Summary, "already") {
		t.Errorf("second cancel = %+v", cres2)
	}

	// Unknown id → NOT_FOUND.
	cres3 := handle(t, cancelTool, `{"asyncId":"asy_nope"}`, &tools.ToolContext{})
	if cres3.Ok || cres3.Error.Code != domain.CodeNotFound {
		t.Errorf("unknown id = %+v", cres3)
	}
}
