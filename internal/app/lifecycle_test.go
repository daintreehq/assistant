package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/mcp"
	"github.com/daintreehq/assistant/internal/prompts"
)

func TestEnsureStartupForTurnJoinsMcpLifecycleGate(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.mcpLifecycleMu.Lock()
	done := make(chan struct{})
	go func() {
		a.ensureStartupForTurn(context.Background())
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("first turn raced ahead while the MCP lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}
	a.mcpLifecycleMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("first turn did not resume after the MCP lifecycle gate completed")
	}
}

func TestEnsureStartupForTurnDoesNotRetryCompletedDegradedAttempt(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	a.mcpLifecycleMu.Lock()
	a.startupConnectAttempted = true
	a.mcpLifecycleMu.Unlock()

	// If ensure incorrectly calls ConnectMcp, the disconnected refresh will block on
	// startupRefreshMu. A completed degraded attempt must instead fail open immediately.
	a.startupRefreshMu.Lock()
	done := make(chan struct{})
	go func() {
		a.ensureStartupForTurn(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		a.startupRefreshMu.Unlock()
		t.Fatal("degraded turn retried the full MCP connect/discovery path")
	}
	a.startupRefreshMu.Unlock()
}

// A turn whose MCP session was alive earlier THIS process, then died mid-session
// (evicted/dropped — not a credential revocation), must get a real reconnect
// attempt rather than running every remaining turn against a dead client. Proven
// via the same startupRefreshMu trick as the completed-degraded-attempt test
// above, inverted: ReconnectMcp's own refreshStartupContext call blocks on that
// mutex, so a reconnect attempt is detected by the call BLOCKING rather than
// fast-returning.
func TestEnsureStartupForTurnReconnectsAfterMidSessionDeath(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	a.mcpLifecycleMu.Lock()
	a.startupConnectAttempted = true
	a.mcpEverConnected = true
	// lastMcpReconnectAttempt deliberately left at its zero value: the production
	// code, not this test, must be the one that stamps it.
	a.mcpLifecycleMu.Unlock()

	a.startupRefreshMu.Lock()
	done := make(chan struct{})
	go func() {
		a.ensureStartupForTurn(context.Background())
		close(done)
	}()
	select {
	case <-done:
		a.startupRefreshMu.Unlock()
		t.Fatal("mid-session death did not trigger a reconnect attempt")
	case <-time.After(100 * time.Millisecond):
	}
	a.startupRefreshMu.Unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reconnect attempt never completed after the refresh gate released")
	}

	// Proves the PRODUCTION code, not this test's setup, did the stamping: this
	// test never wrote lastMcpReconnectAttempt itself, so a value here can only
	// have come from ensureStartupForTurn's own throttle-stamp line.
	a.mcpLifecycleMu.Lock()
	stamped := a.lastMcpReconnectAttempt
	a.mcpLifecycleMu.Unlock()
	if stamped.IsZero() || time.Since(stamped) > 5*time.Second {
		t.Fatalf("lastMcpReconnectAttempt = %v, want a recent stamp from ensureStartupForTurn itself", stamped)
	}
}

// The negative half of the mcpEverConnected latch: a boot that ran through the REAL
// ConnectMcp path but never actually connected (offline mode always fails to connect)
// must leave the flag false, so ensureStartupForTurn's mid-session recovery never
// fires for a session that was never live to begin with. Exercises the actual
// lifecycle.go assignment rather than pre-seeding the field, unlike the tests above
// (which have to pre-seed it — offline mode cannot reach a genuine successful
// connect through the public API without a fake LowLevelClient).
func TestConnectMcpOfflineNeverLatchesEverConnected(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.ConnectMcp(context.Background())

	a.mcpLifecycleMu.Lock()
	everConnected := a.mcpEverConnected
	a.mcpLifecycleMu.Unlock()
	if everConnected {
		t.Fatal("an offline connect attempt (which never actually connects) latched mcpEverConnected")
	}
}

// The mid-session recovery above must not turn a sustained outage into an 8s-capped
// reconnect handshake on EVERY turn — a recent attempt (within mcpTurnReconnectInterval)
// must make this turn fail open immediately instead, exactly like the never-connected
// case, so the cost is one handshake per interval rather than one per turn.
func TestEnsureStartupForTurnThrottlesRepeatedReconnectAttempts(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	a.mcpLifecycleMu.Lock()
	a.startupConnectAttempted = true
	a.mcpEverConnected = true
	a.lastMcpReconnectAttempt = time.Now()
	a.mcpLifecycleMu.Unlock()

	a.startupRefreshMu.Lock()
	done := make(chan struct{})
	go func() {
		a.ensureStartupForTurn(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		a.startupRefreshMu.Unlock()
		t.Fatal("a turn within the throttle window attempted another reconnect")
	}
	a.startupRefreshMu.Unlock()
}

func TestCanceledSplashAttemptRemainsRetryable(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.ConnectMcp(ctx)
	a.mcpLifecycleMu.Lock()
	attempted := a.startupConnectAttempted
	a.mcpLifecycleMu.Unlock()
	if attempted {
		t.Fatal("canceled splash was recorded as the one completed automatic attempt")
	}
}

func TestCancelledSplashDoesNotSpendLedgerReconcileOnce(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	a.maybeReconcileLedger(ctx, true)
	a.reconcileLedgerMu.Lock()
	done := a.reconcileLedgerDone
	a.reconcileLedgerMu.Unlock()
	if done {
		t.Fatal("canceled splash permanently spent the reconcile gate")
	}
}

// The MCP connect diagnostics line must never contain token material — no raw token,
// no substring, and no token-DERIVED value either (a truncated hash fingerprint is an
// offline verification oracle for low-entropy tokens). Only the URL host plus token
// presence and length may appear. This is the guard against reintroducing the old
// raw-credential debug dump or the hash fingerprint.
func TestMcpConnectDiagnosticsContainsNoTokenMaterial(t *testing.T) {
	dir := t.TempDir()
	const token = "dmt_live_supersecret_bearer_value_1234567890"
	a := &App{Config: config.AppConfig{
		McpURL:   "http://127.0.0.1:45454/mcp?session=" + token,
		McpToken: token,
		DebugLog: true,
		LogDir:   dir,
	}}
	a.logMcpConnectDiagnostics(mcp.Status{Connected: true, Transport: "streamable-http"})

	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		t.Fatalf("no debug log written (err=%v entries=%d)", err, len(entries))
	}
	var content strings.Builder
	for _, e := range entries {
		b, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("read %s: %v", e.Name(), rerr)
		}
		content.Write(b)
	}
	logged := content.String()
	if logged == "" {
		t.Fatal("debug log is empty — diagnostics line was not written")
	}
	if strings.Contains(logged, token) {
		t.Fatalf("debug log contains the raw MCP token:\n%s", logged)
	}
	// No long token substring may leak either (e.g. via the URL query).
	if strings.Contains(logged, token[:16]) {
		t.Fatalf("debug log contains token material (prefix leak):\n%s", logged)
	}
	if !strings.Contains(logged, "127.0.0.1:45454") {
		t.Fatalf("diagnostics line lost the URL host:\n%s", logged)
	}
	// No token-derived fingerprint may appear either — a short hash is an offline
	// verification oracle for a low-entropy token.
	sum := sha256.Sum256([]byte(token))
	if strings.Contains(logged, hex.EncodeToString(sum[:4])) {
		t.Fatalf("debug log contains a token-derived hash fingerprint:\n%s", logged)
	}
	// Presence + length are the only token facts allowed on the line.
	if !strings.Contains(logged, "tokenPresent=true") {
		t.Fatalf("diagnostics line lost the token-presence flag:\n%s", logged)
	}
	if !strings.Contains(logged, fmt.Sprintf("tokenLength=%d", len(token))) {
		t.Fatalf("diagnostics line lost the token length:\n%s", logged)
	}
}

func TestMergedResultObjectRecursivelyUnionsTextAndStructured(t *testing.T) {
	res := mcp.CallResult{
		Text: `{"project":{"id":"p1","name":"from text"},"textOnly":true}`,
		StructuredContent: map[string]any{
			"project": map[string]any{"name": "structured", "path": "/repo"},
		},
	}
	got := mergedResultObject(res)
	project, ok := got["project"].(map[string]any)
	if !ok {
		t.Fatalf("project = %#v, want object", got["project"])
	}
	if project["id"] != "p1" || project["name"] != "structured" || project["path"] != "/repo" {
		t.Fatalf("recursive merge lost fields or precedence: %#v", project)
	}
	if got["textOnly"] != true {
		t.Fatalf("text-only field missing: %#v", got)
	}
}

func TestParseAvailableAgentsPreservesCompleteRegistryMetadata(t *testing.T) {
	res := mcp.CallResult{Text: `{"agents":[
        {"id":"claude","displayName":"Claude Code","source":"built-in","availability":"ready","installed":true,"toolbarVisible":true,"pinned":true},
        {"id":"custom-agent","displayName":"Team Agent","source":"user","availability":"unauthenticated"},
        {"id":"daintree-assistant","displayName":"Assistant","source":"built-in"},
        {"id":"  "}
    ]}`}
	got, complete, availabilityComplete, ok := parseAvailableAgents(res)
	if !ok {
		t.Fatal("canonical available-agent payload was not recognized")
	}
	if complete || availabilityComplete {
		t.Fatalf("omitted completeness flags must remain false: complete=%v availability=%v", complete, availabilityComplete)
	}
	if len(got) != 2 {
		t.Fatalf("agents = %+v, want two valid rows", got)
	}
	if got[0].ID != "claude" || got[0].Source != "built-in" || got[0].Pinned == nil || !*got[0].Pinned {
		t.Fatalf("explicit pin was not preserved: %+v", got[0])
	}
	if got[0].ToolbarVisible == nil || !*got[0].ToolbarVisible {
		t.Fatalf("resolved toolbar state was not preserved: %+v", got[0])
	}
	if got[1].Source != "user" || got[1].Installed == nil || !*got[1].Installed {
		t.Fatalf("custom availability metadata was not derived: %+v", got[1])
	}
}

func TestParseAvailableAgentsRejectsMissingWrapper(t *testing.T) {
	if got, _, _, ok := parseAvailableAgents(mcp.CallResult{Text: `{"complete":true}`}); ok || got != nil {
		t.Fatalf("missing agents wrapper parsed as got=%+v ok=%v", got, ok)
	}
	if got, _, _, ok := parseAvailableAgents(mcp.CallResult{Text: `{"agents":"not-an-array"}`}); ok || got != nil {
		t.Fatalf("malformed agents wrapper parsed as got=%+v ok=%v", got, ok)
	}
	if got, _, _, ok := parseAvailableAgents(mcp.CallResult{Text: `{"agents":null}`}); ok || got != nil {
		t.Fatalf("null agents wrapper parsed as got=%+v ok=%v", got, ok)
	}
}

func TestParseAvailableAgentsUnionsTextAndStructuredRows(t *testing.T) {
	pinned := true
	res := mcp.CallResult{
		Text: `{"complete":true,"agents":[{"id":"text-agent","displayName":"Text Agent","availability":"ready"}]}`,
		StructuredContent: map[string]any{
			"availabilityComplete": true,
			"agents": []any{
				map[string]any{"id": "text-agent", "pinned": pinned},
				map[string]any{"id": "structured-agent", "displayName": "Structured Agent"},
			},
		},
	}
	rows, complete, availabilityComplete, ok := parseAvailableAgents(res)
	if !ok || !complete || !availabilityComplete || len(rows) != 2 {
		t.Fatalf("union = rows:%+v complete:%v availability:%v ok:%v", rows, complete, availabilityComplete, ok)
	}
	// Both rows carry an empty source, so the canonical (source, id) order puts the
	// structured-only row ahead of the merged text row regardless of which channel saw
	// each id first.
	if rows[0].ID != "structured-agent" {
		t.Fatalf("structured-only row missing: %+v", rows)
	}
	if rows[1].ID != "text-agent" || rows[1].DisplayName != "Text Agent" || rows[1].Pinned == nil || !*rows[1].Pinned {
		t.Fatalf("structured update dropped text fields: %+v", rows[1])
	}
}

// The roster rides the cacheable startup block, so an unchanged registry must serialize
// byte-identically no matter what order Daintree's discovery happened to report. Every
// fixture field is deliberately anti-correlated with the expected sequence: sorting by id
// alone, by display name, by availability, or by (source, displayName) / (source,
// availability) each yields a DIFFERENT order. So the literal wantOrder pins the real key,
// where merely proving the permutations agree with each other would also accept a sort on
// the wrong field.
func TestParseAvailableAgentsProducesStablePayloadAcrossDiscoveryOrder(t *testing.T) {
	const (
		builtinZ = `{"id":"z-built-in","displayName":"Alpha display","source":"built-in","availability":"blocked"}`
		userA    = `{"id":"a-user","displayName":"Bravo display","source":"user","availability":"missing"}`
		builtinB = `{"id":"b-built-in","displayName":"Zulu display","source":"built-in","availability":"ready"}`
		pluginA  = `{"id":"a-plugin","displayName":"Middle display","source":"plugin","availability":"unauthenticated"}`

		pluginWithoutSource = `{"id":"a-plugin","displayName":"Middle display","availability":"unauthenticated"}`
		wantOrder           = "built-in/b-built-in,built-in/z-built-in,plugin/a-plugin,user/a-user"
	)

	textResult := func(rawRows ...string) mcp.CallResult {
		return mcp.CallResult{
			Text: `{"complete":true,"availabilityComplete":true,"agents":[` + strings.Join(rawRows, ",") + `]}`,
		}
	}

	tests := []struct {
		name string
		res  mcp.CallResult
	}{
		{name: "forward", res: textResult(builtinZ, userA, builtinB, pluginA)},
		{name: "reverse", res: textResult(pluginA, builtinB, userA, builtinZ)},
		{name: "rotation", res: textResult(builtinB, pluginA, builtinZ, userA)},
		{
			// The split case proves the order is computed from MERGED field values, not from
			// whatever each channel saw on its own: a-plugin arrives from text with no source
			// at all, and only the structured channel supplies the "plugin" the primary key
			// needs. The structured rows are ordered so the last thing that happens is that
			// source patch, never a new id — otherwise a sorter that re-ran on every append
			// would be re-sorting after the fix and would survive with the right answer.
			name: "split across text and structured channels",
			res: mcp.CallResult{
				Text: `{"complete":true,"availabilityComplete":true,"agents":[` +
					userA + `,` + builtinZ + `,` + pluginWithoutSource + `]}`,
				StructuredContent: map[string]any{
					"complete":             true,
					"availabilityComplete": true,
					"agents": []any{
						map[string]any{
							"id":           "b-built-in",
							"displayName":  "Zulu display",
							"source":       "built-in",
							"availability": "ready",
						},
						map[string]any{"id": "a-plugin", "source": "plugin"},
					},
				},
			},
		},
	}

	var firstPayload []byte
	for index, tc := range tests {
		rows, complete, availabilityComplete, ok := parseAvailableAgents(tc.res)
		if !ok || !complete || !availabilityComplete || len(rows) != 4 {
			t.Fatalf("%s: parse = rows:%+v complete:%v availability:%v ok:%v", tc.name, rows, complete, availabilityComplete, ok)
		}
		keys := make([]string, len(rows))
		for i, row := range rows {
			keys[i] = row.Source + "/" + row.ID
		}
		if got := strings.Join(keys, ","); got != wantOrder {
			t.Fatalf("%s: agent order = %q, want %q", tc.name, got, wantOrder)
		}

		// Marshal the whole carrier, not just the slice: the wire block is the unit that
		// must stay byte-stable, and this also pins every derived per-row field.
		payload, err := json.Marshal(prompts.AgentRosterContext{
			Agents:               rows,
			Complete:             complete,
			AvailabilityComplete: availabilityComplete,
			TotalCount:           len(rows),
		})
		if err != nil {
			t.Fatalf("%s: marshal roster: %v", tc.name, err)
		}
		if index == 0 {
			firstPayload = payload
			continue
		}
		if string(payload) != string(firstPayload) {
			t.Fatalf("%s: payload differs by discovery order:\nfirst: %s\n%s: %s", tc.name, firstPayload, tc.name, payload)
		}
	}
}

func TestWorktreeLabelDistinguishesUnknownAndNone(t *testing.T) {
	if got := worktreeLabel(nil); got != "" {
		t.Fatalf("unknown worktree label = %q, want empty", got)
	}
	if got := worktreeLabel(&prompts.WorktreeContext{Present: false}); got != "(none — not in a worktree)" {
		t.Fatalf("definitive-none label = %q", got)
	}
	if got := worktreeLabel(&prompts.WorktreeContext{Present: true, Branch: "feature/x", ID: "wt-1"}); got != "feature/x" {
		t.Fatalf("branch should win, got %q", got)
	}
	if got := worktreeLabel(&prompts.WorktreeContext{Present: true, ID: "wt-1", Path: "/repo"}); got != "wt-1" {
		t.Fatalf("id fallback = %q", got)
	}
}

// A degraded reconnect must clear every Daintree-owned snapshot atomically instead of
// leaking a prior project's agents/worktree into later backend requests.
func TestRefreshStartupContextNotConnectedClears(t *testing.T) {
	a := newOfflineApp(t)
	defer a.Shutdown()

	a.startupMu.Lock()
	a.cachedProject = &prompts.ProjectContext{ID: "old"}
	a.cachedAgents = &prompts.AgentRosterContext{Agents: []prompts.AgentContext{{ID: "claude"}}}
	a.cachedWorktree = &prompts.WorktreeContext{Present: true, Branch: "feature/x"}
	a.startupMu.Unlock()

	a.refreshStartupContext(context.Background(), false)

	a.startupMu.RLock()
	project, agents, worktree := a.cachedProject, a.cachedAgents, a.cachedWorktree
	a.startupMu.RUnlock()
	if project != nil || agents != nil || worktree != nil {
		t.Fatalf("degraded refresh retained stale context: project=%+v agents=%+v worktree=%+v", project, agents, worktree)
	}
	pc := a.PromptContext()
	if pc.AgentRoster != nil || pc.Worktree != nil {
		t.Fatalf("PromptContext retained stale Daintree state: %+v", pc)
	}
	if got := a.activeWorktreeForFooter(); got != "" {
		t.Fatalf("cleared worktree label = %q, want empty", got)
	}
}

// asyncRec builds a live await.async record owned by sessionID. terminal.await.async
// is the watch-only starter, so nothing here implies a side effect that already ran.
func asyncRec(id, sessionID string) domain.AsyncInvocationRecord {
	return domain.AsyncInvocationRecord{
		ID: id, ToolName: "terminal.await.async", Title: "job " + id, GroupID: "run_" + id,
		SessionID: sessionID, TerminalIdsJson: `["term-1"]`,
		Status: domain.AsyncRunning, CreatedAt: 1_000, ExpiresAt: 1 << 40,
	}
}

// spyContext records whether anyone consulted Done(). It is how the fast-path tests
// below assert "returned without entering the poll loop" WITHOUT timing anything: any
// implementation that reaches the select must consult Done(), whatever the machine is
// doing, so the spy is a load-bearing property where an elapsed-time bound is only a
// guess about scheduler load. blocked closes once Done() is first consulted, which also
// gives the blocking tests a deterministic "the waiter is in the select now" signal.
type spyContext struct {
	context.Context
	once     sync.Once
	blocked  chan struct{}
	consults atomic.Int32
}

func newSpyContext(parent context.Context) *spyContext {
	return &spyContext{Context: parent, blocked: make(chan struct{})}
}

func (c *spyContext) Done() <-chan struct{} {
	c.consults.Add(1)
	c.once.Do(func() { close(c.blocked) })
	return c.Context.Done()
}

// enteredWait reports whether the waiter reached its blocking select.
func (c *spyContext) enteredWait() bool { return c.consults.Load() > 0 }

// shutdownOnce guards a test's App teardown so an explicit Shutdown and a t.Cleanup
// safety net cannot both fire. Shutdown is documented safe to call ONCE, and a test
// that Fatalf's before its explicit call would otherwise leave a real scheduler and
// coordinator ticking against a Store whose temp dir is about to be removed.
func shutdownOnce(t *testing.T, a *App) func() {
	t.Helper()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Reported, never discarded: a teardown that fails is exactly the regression
			// these tests exist to catch, and swallowing it would hide a scheduler that
			// could not be drained.
			if err := a.Shutdown(); err != nil {
				t.Errorf("Shutdown: %v", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

// TestWaitForSessionAsyncIsFreeWithoutWork: the common flagged run starts no async work
// at all, and it must not pay a poll interval to learn that. Also covers the
// never-started-scheduler case, where the barrier has to be an outright no-op — that is
// what keeps the DEFAULT one-shot path unchanged.
func TestWaitForSessionAsyncIsFreeWithoutWork(t *testing.T) {
	a := newOfflineApp(t)
	shutdownOnce(t, a)

	spy := newSpyContext(context.Background())
	if err := a.WaitForSessionAsync(spy); err != nil {
		t.Fatalf("WaitForSessionAsync before StartScheduler = %v, want nil", err)
	}
	a.StartScheduler(context.Background(), nil)
	if err := a.WaitForSessionAsync(spy); err != nil {
		t.Fatalf("WaitForSessionAsync with no work = %v, want nil", err)
	}
	if spy.enteredWait() {
		t.Error("the no-work barrier consulted ctx.Done(); it must return on the fast path without entering the poll loop")
	}
}

// TestWaitForSessionAsyncIgnoresForeignSessionWork: Start adopts every live async row in
// the PROJECT, which is correct — whoever holds the lease supervises everything — but an
// inherited backlog must never decide when a script exits.
func TestWaitForSessionAsyncIgnoresForeignSessionWork(t *testing.T) {
	a := newOfflineApp(t)
	shutdownOnce(t, a)
	a.StartScheduler(context.Background(), nil)

	if err := a.asyncCoordinator.Register(asyncRec("asy_theirs", "ses_someone_else"), []string{"term-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	spy := newSpyContext(context.Background())
	if err := a.WaitForSessionAsync(spy); err != nil {
		t.Fatalf("WaitForSessionAsync with only foreign work = %v, want nil", err)
	}
	if spy.enteredWait() {
		t.Error("foreign-session work made the barrier block; only THIS session's work may gate the exit")
	}
}

// TestWaitForSessionAsyncBlocksOnOwnWorkUntilDeadline: the whole point of the barrier is
// that the run does NOT exit while its own handles are live. The waiter is proven to be
// IN the select before the context is killed — otherwise a fast machine could satisfy
// this test without the barrier ever blocking — and Shutdown must still complete after a
// cancellation that lands mid-poll.
func TestWaitForSessionAsyncBlocksOnOwnWorkUntilDeadline(t *testing.T) {
	a := newOfflineApp(t)
	stop := shutdownOnce(t, a)
	a.StartScheduler(context.Background(), nil)

	if err := a.asyncCoordinator.Register(asyncRec("asy_mine", a.SessionID), []string{"term-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A deadline far enough out that only the explicit cancel below ends the wait, so
	// the test never depends on how fast the machine gets there.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spy := newSpyContext(ctx)
	done := make(chan error, 1)
	go func() { done <- a.WaitForSessionAsync(spy) }()

	select {
	case <-spy.blocked: // the waiter reached its select
	case err := <-done:
		t.Fatalf("WaitForSessionAsync returned %v without blocking, but this session has live work", err)
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForSessionAsync never entered its poll loop")
	}
	// Still blocked: the work has not cleared, so nothing may release it.
	select {
	case err := <-done:
		t.Fatalf("WaitForSessionAsync returned %v while its own work is still live", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("WaitForSessionAsync = %v, want the context error", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForSessionAsync did not return after its context was cancelled")
	}

	// The work is still live: the barrier never cancels or abandons it, so the next
	// owner can adopt it.
	if n := a.asyncCoordinator.ActiveCountForSession(a.SessionID); n != 1 {
		t.Errorf("live session work after the timeout = %d, want 1 (the wait must not mutate it)", n)
	}
	// Teardown after a cancellation that landed mid-poll must still complete.
	stop()
}

// TestWaitForSessionAsyncReturnsWhenOwnWorkClears: the barrier releases as soon as the
// last of this session's invocations deregisters (which publishGroup does after the
// completion event is published). Deregistering only AFTER the waiter is provably in its
// select keeps this from passing vacuously through the fast path.
func TestWaitForSessionAsyncReturnsWhenOwnWorkClears(t *testing.T) {
	a := newOfflineApp(t)
	shutdownOnce(t, a)
	a.StartScheduler(context.Background(), nil)

	if err := a.asyncCoordinator.Register(asyncRec("asy_mine", a.SessionID), []string{"term-1"}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	spy := newSpyContext(ctx)
	done := make(chan error, 1)
	go func() { done <- a.WaitForSessionAsync(spy) }()

	select {
	case <-spy.blocked:
	case err := <-done:
		t.Fatalf("WaitForSessionAsync returned %v without blocking on its own live work", err)
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForSessionAsync never entered its poll loop")
	}

	a.asyncCoordinator.Deregister("asy_mine")
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WaitForSessionAsync = %v, want nil once the work cleared", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WaitForSessionAsync did not return after its last invocation deregistered")
	}
}
