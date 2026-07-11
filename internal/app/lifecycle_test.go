package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/mcp"
	"github.com/daintreehq/daintree-assistant/internal/models/prompts"
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
	if rows[0].ID != "text-agent" || rows[0].DisplayName != "Text Agent" || rows[0].Pinned == nil || !*rows[0].Pinned {
		t.Fatalf("structured update dropped text fields: %+v", rows[0])
	}
	if rows[1].ID != "structured-agent" {
		t.Fatalf("structured-only row missing: %+v", rows)
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
