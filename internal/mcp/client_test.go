package mcp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
)

// fakeLow is a controllable LowLevelClient for injection-seam tests.
type fakeLow struct {
	mu sync.Mutex

	tools       []rawTool
	listErr     error
	listCalls   int
	callResult  rawResult
	callErrs    []error // popped per attempt; nil = success
	callCalls   int
	serverInfo  *ServerInfo
	closeCalled bool
}

func (f *fakeLow) ListTools(ctx context.Context) ([]rawTool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tools, nil
}

func (f *fakeLow) CallTool(ctx context.Context, name string, args map[string]any) (rawResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.callCalls
	f.callCalls++
	if idx < len(f.callErrs) {
		if err := f.callErrs[idx]; err != nil {
			return rawResult{}, err
		}
	}
	return f.callResult, nil
}

func (f *fakeLow) GetServerVersion() *ServerInfo { return f.serverInfo }
func (f *fakeLow) Close() error                  { f.closeCalled = true; return nil }

func newInjected(low LowLevelClient) *Client {
	return New(config.AppConfig{McpURL: "http://x/mcp", McpToken: "t"}, Options{ClientOverride: low})
}

func TestFullJitterDelayBounds(t *testing.T) {
	base := 250 * time.Millisecond
	max := 2000 * time.Millisecond
	for attempt := 0; attempt < 12; attempt++ {
		// expected ceiling = min(max, base*2^attempt)
		ceiling := base
		for i := 0; i < attempt; i++ {
			ceiling *= 2
		}
		if ceiling > max {
			ceiling = max
		}
		for i := 0; i < 200; i++ {
			d := fullJitterDelay(attempt, base, max)
			if d < 0 || d > ceiling {
				t.Fatalf("attempt %d: delay %v out of [0,%v]", attempt, d, ceiling)
			}
		}
	}
}

func TestIsRetriableMcpError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"canceled", context.Canceled, false},
		{"deadline", context.DeadlineExceeded, true},
		{"jsonrpc -32001", &jsonrpc.Error{Code: -32001}, true},
		{"jsonrpc -32000", &jsonrpc.Error{Code: -32000}, true},
		{"jsonrpc app error", &jsonrpc.Error{Code: -32603}, false},
		{"econnreset msg", errors.New("read tcp: ECONNRESET"), true},
		{"fetch failed", errors.New("fetch failed"), true},
		{"random", errors.New("the tool says no"), false},
		// Binding-terminal markers must NOT be retriable.
		{"binding gone", errors.New("SESSION_BINDING_GONE: window closed"), false},
		{"binding stale", errors.New("BINDING_STALE"), false},
	}
	for _, tc := range cases {
		if got := isRetriableMcpError(tc.err); got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

func TestBindingTerminalDetection(t *testing.T) {
	if !isBindingTerminal("error: SESSION_BINDING_GONE") {
		t.Error("expected SESSION_BINDING_GONE terminal")
	}
	if !isBindingTerminal("binding_stale now") {
		t.Error("expected case-insensitive BINDING_STALE terminal")
	}
	if isBindingTerminal("all good") {
		t.Error("unexpected terminal")
	}
}

func TestListToolsCachingAndInputSchemaDefault(t *testing.T) {
	low := &fakeLow{tools: []rawTool{
		{Name: "a", Description: "da", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": 1}}},
		{Name: "b"}, // no schema → default substituted
	}}
	c := newInjected(low)
	ctx := context.Background()

	tools, err := c.ListTools(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 {
		t.Fatalf("want 2 tools got %d", len(tools))
	}
	if tools[1].InputSchema["type"] != "object" {
		t.Errorf("default schema missing type")
	}
	if _, ok := tools[1].InputSchema["properties"]; !ok {
		t.Errorf("default schema missing properties")
	}
	// Cache-first: a second non-forced call must not hit the low client again.
	if _, err := c.ListTools(ctx, false); err != nil {
		t.Fatal(err)
	}
	if low.listCalls != 1 {
		t.Errorf("expected 1 list call (cached), got %d", low.listCalls)
	}
	// force=true re-fetches.
	if _, err := c.ListTools(ctx, true); err != nil {
		t.Fatal(err)
	}
	if low.listCalls != 2 {
		t.Errorf("expected 2 list calls after force, got %d", low.listCalls)
	}
}

func TestConnectInjectedWarmsOnce(t *testing.T) {
	low := &fakeLow{tools: []rawTool{{Name: "actions.getContext"}}}
	c := newInjected(low)
	if !c.IsConnected() {
		t.Fatal("injected client should be connected")
	}
	// Cache cold until Connect warms it.
	if c.Status().ToolCount != nil {
		t.Fatal("cache should be cold before Connect")
	}
	c.Connect(context.Background())
	st := c.Status()
	if st.ToolCount == nil || *st.ToolCount != 1 {
		t.Fatalf("expected warmed tool count 1, got %v", st.ToolCount)
	}
	if st.Transport != transportInjected {
		t.Errorf("expected injected transport, got %s", st.Transport)
	}
}

func TestStatusDriftCollapseAndCopies(t *testing.T) {
	// One documented tool present, the rest missing → drift on the missing ones.
	low := &fakeLow{tools: []rawTool{{Name: "actions.getContext"}}}
	c := newInjected(low)
	c.Connect(context.Background())
	st := c.Status()
	if len(st.DriftWarnings) == 0 {
		t.Fatal("expected drift warnings for missing documented tools")
	}
	if len(st.DriftWarnings) != len(st.DriftToolNames) {
		t.Fatal("drift arrays must be index-aligned")
	}
	// Mutating the returned slice must not affect internal state (defensive copy).
	st.DriftWarnings[0] = "MUTATED"
	if c.Status().DriftWarnings[0] == "MUTATED" {
		t.Error("status did not return a defensive copy")
	}

	// No drift (all documented present) → nil, not [].
	all := make([]rawTool, len(DocumentedMcpToolNames))
	for i, n := range DocumentedMcpToolNames {
		all[i] = rawTool{Name: n}
	}
	low2 := &fakeLow{tools: all}
	c2 := newInjected(low2)
	c2.Connect(context.Background())
	st2 := c2.Status()
	if st2.DriftWarnings != nil || st2.DriftToolNames != nil {
		t.Errorf("expected nil drift arrays when no drift, got %v / %v", st2.DriftWarnings, st2.DriftToolNames)
	}
}

func TestDriftLiveSizeZeroReturns(t *testing.T) {
	// Empty live set → treat as unknown, not "everything drifted".
	low := &fakeLow{tools: []rawTool{}}
	c := newInjected(low)
	c.Connect(context.Background())
	st := c.Status()
	if st.DriftWarnings != nil {
		t.Errorf("empty live set must not produce drift, got %v", st.DriftWarnings)
	}
}

func TestCallToolRetryThenSucceed(t *testing.T) {
	// First attempt a retriable transport error, second succeeds. Connection must
	// stay healthy (retry-before-degrade).
	low := &fakeLow{
		tools:      []rawTool{{Name: "x"}},
		callErrs:   []error{&jsonrpc.Error{Code: -32000}, nil},
		callResult: rawResult{Text: "done"},
	}
	c := newInjected(low)
	c.Connect(context.Background())
	// A read-only tool name so the retry budget is honored (the read-only guard
	// forces mutations single-shot).
	res, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 2})
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if res.Text != "done" {
		t.Errorf("want done got %q", res.Text)
	}
	if low.callCalls != 2 {
		t.Errorf("expected 2 attempts, got %d", low.callCalls)
	}
	if !c.IsConnected() {
		t.Error("connection should remain healthy after a successful retry")
	}
}

func TestCallToolDegradesAfterBudget(t *testing.T) {
	low := &fakeLow{
		callErrs: []error{&jsonrpc.Error{Code: -32000}, &jsonrpc.Error{Code: -32000}},
	}
	c := newInjected(low)
	c.Connect(context.Background())
	_, err := c.CallTool(context.Background(), "terminal.getStatus", nil, CallOptions{Retries: 1})
	if err == nil {
		t.Fatal("expected error after retry budget spent")
	}
	if c.IsConnected() {
		t.Error("connection should be degraded after the budget is spent")
	}
}

func TestCallToolNoRetryDefault(t *testing.T) {
	// Retries default 0: a mutating tool must never auto-retry.
	low := &fakeLow{callErrs: []error{&jsonrpc.Error{Code: -32000}}}
	c := newInjected(low)
	c.Connect(context.Background())
	_, _ = c.CallTool(context.Background(), "x", nil, CallOptions{})
	if low.callCalls != 1 {
		t.Errorf("expected exactly 1 attempt with Retries=0, got %d", low.callCalls)
	}
}

func TestCallToolUnavailableWhenDisconnected(t *testing.T) {
	c := New(config.AppConfig{}, Options{}) // no override → disconnected
	_, err := c.CallTool(context.Background(), "x", nil, CallOptions{})
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("expected UnavailableError, got %v", err)
	}
	if ue.Code() != UnavailableCode {
		t.Errorf("want code %s got %s", UnavailableCode, ue.Code())
	}
}

func TestCallToolNormalize(t *testing.T) {
	low := &fakeLow{callResult: rawResult{
		Text:              "hello",
		Content:           nil, // → default []
		StructuredContent: map[string]any{"k": "v"},
		IsError:           true,
	}}
	c := newInjected(low)
	c.Connect(context.Background())
	res, err := c.CallTool(context.Background(), "x", nil, CallOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Content == nil {
		t.Error("content must default to non-nil []")
	}
	if !res.IsError {
		t.Error("isError must pass through")
	}
	if res.StructuredContent == nil {
		t.Error("structuredContent must pass through")
	}
}

func TestConnectOfflineAndMissingCreds(t *testing.T) {
	c := New(config.AppConfig{Offline: true, McpURL: "http://x/mcp", McpToken: "t"}, Options{})
	st := c.Connect(context.Background())
	if st.Connected || st.Error != "offline mode" {
		t.Errorf("offline: got connected=%v err=%q", st.Connected, st.Error)
	}

	c2 := New(config.AppConfig{}, Options{})
	st2 := c2.Connect(context.Background())
	if st2.Connected || st2.Error != "DAINTREE_MCP_URL / DAINTREE_MCP_TOKEN not set" {
		t.Errorf("missing creds: got connected=%v err=%q", st2.Connected, st2.Error)
	}
}

func TestSSEPathRewrite(t *testing.T) {
	cases := map[string]string{
		"/mcp":     "/sse",
		"/mcp/":    "/sse",
		"/api/mcp": "/api/sse",
		"/other":   "/other", // unchanged when not ending in /mcp
		"/mcps":    "/mcps",  // anchored: /mcps is not /mcp
		"/mcp/sub": "/mcp/sub",
	}
	for in, want := range cases {
		if got := sseRewriteRe.ReplaceAllString(in, "/sse"); got != want {
			t.Errorf("rewrite %q: got %q want %q", in, got, want)
		}
	}
}

func TestReadProjectName(t *testing.T) {
	// structuredContent top-level projectName.
	if got := ReadProjectName(CallResult{StructuredContent: map[string]any{"projectName": "  Acme  "}}); got != "Acme" {
		t.Errorf("structured top-level: got %q", got)
	}
	// nested project.name.
	if got := ReadProjectName(CallResult{StructuredContent: map[string]any{"project": map[string]any{"name": "Nested"}}}); got != "Nested" {
		t.Errorf("nested: got %q", got)
	}
	// text-JSON fallback (load-bearing).
	if got := ReadProjectName(CallResult{Text: `{"projectName":"FromText"}`}); got != "FromText" {
		t.Errorf("text fallback: got %q", got)
	}
	// failure → "".
	if got := ReadProjectName(CallResult{Text: "not json"}); got != "" {
		t.Errorf("bad json should yield empty, got %q", got)
	}
	if got := ReadProjectName(CallResult{}); got != "" {
		t.Errorf("empty should yield empty, got %q", got)
	}
}

func TestToolsAdvertiseGrantSupport(t *testing.T) {
	tools := []ToolInfo{{Name: "a"}, {Name: "b"}}
	// Empty grant list (today) → always false.
	if ToolsAdvertiseGrantSupport(tools, GrantToolNames) {
		t.Error("empty grant list must short-circuit to false")
	}
	if ToolsAdvertiseGrantSupport(tools, []string{}) {
		t.Error("empty grant list must be false")
	}
	if !ToolsAdvertiseGrantSupport(tools, []string{"b"}) {
		t.Error("present grant tool should be true")
	}
	if ToolsAdvertiseGrantSupport(tools, []string{"z"}) {
		t.Error("absent grant tool should be false")
	}
}

func TestHasGrantSupportObservational(t *testing.T) {
	c := New(config.AppConfig{}, Options{}) // disconnected
	if c.HasGrantSupport() {
		t.Error("disconnected client must report no grant support")
	}
	low := &fakeLow{tools: []rawTool{{Name: "actions.getContext"}}}
	c2 := newInjected(low)
	c2.Connect(context.Background())
	if c2.HasGrantSupport() {
		t.Error("grant support must be false today (empty allowlist)")
	}
}

func TestDoctorProbe(t *testing.T) {
	// Not connected → not reachable.
	c := New(config.AppConfig{}, Options{})
	if r := c.Doctor(context.Background()); r.Reachable {
		t.Error("disconnected doctor should be unreachable")
	}

	// Tool not advertised → workbench-tier failure.
	low := &fakeLow{tools: []rawTool{{Name: "other.tool"}}}
	c2 := newInjected(low)
	c2.Connect(context.Background())
	r2 := c2.Doctor(context.Background())
	if r2.OK || r2.ToolListed {
		t.Errorf("expected workbench-unavailable failure, got %+v", r2)
	}

	// Advertised + clean call → OK.
	low3 := &fakeLow{
		tools:      []rawTool{{Name: doctorProbeTool}},
		callResult: rawResult{Text: "ctx", IsError: false},
	}
	c3 := newInjected(low3)
	c3.Connect(context.Background())
	r3 := c3.Doctor(context.Background())
	if !r3.OK {
		t.Errorf("expected OK probe, got %+v", r3)
	}

	// Advertised + isError result → fail.
	low4 := &fakeLow{
		tools:      []rawTool{{Name: doctorProbeTool}},
		callResult: rawResult{IsError: true},
	}
	c4 := newInjected(low4)
	c4.Connect(context.Background())
	if c4.Doctor(context.Background()).OK {
		t.Error("isError result should fail the probe")
	}
}

func TestReconnectResetsState(t *testing.T) {
	low := &fakeLow{tools: []rawTool{{Name: "actions.getContext"}}}
	c := newInjected(low)
	c.Connect(context.Background())
	// Reconnect on an injected client has no real transport, so it lands degraded
	// (missing creds path is short-circuited because URL/token ARE set but there's
	// no live server). We only assert state was reset + close was called.
	c.Reconnect(context.Background())
	if !low.closeCalled {
		t.Error("reconnect should close the prior low client")
	}
}

func TestCloseSwallowsAndDisconnects(t *testing.T) {
	low := &fakeLow{}
	c := newInjected(low)
	if err := c.Close(); err != nil {
		t.Errorf("close should not return an error, got %v", err)
	}
	if c.IsConnected() {
		t.Error("close must set connected=false")
	}
	if !low.closeCalled {
		t.Error("close must call the low client's Close")
	}
}
