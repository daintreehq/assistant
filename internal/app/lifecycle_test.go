package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/config"
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
