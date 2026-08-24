package commands

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/config"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/models"
	"github.com/daintreehq/assistant/internal/storage"
)

// Exercises handleUiCommand + the disconnected /doctor probe. Builds an offline
// App against a temp state dir and drives the structured attached session slash handler,
// asserting the per-command behaviors: /status, /permissions
// switch + reject-unknown, /inbox, /audit (grant_ok source bracket, sourceless row,
// export json/csv/bad-format, list), /explain (empty/list/timeline/unknown/malformed/
// truncation/full-arg-runId), /tools, /models, /help, /clear, unknown-command, and
// that every registry command is handled (no list/switch drift).

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

// newOfflineApp builds an offline App writing to a temp dir (cleaned by t).
func newOfflineApp(t *testing.T) *app.App {
	t.Helper()
	dir := t.TempDir()
	a, err := app.Create(app.CreateOptions{
		Overrides: config.ConfigOverrides{
			Offline:     boolPtr(true),
			StateDir:    strPtr(dir),
			ProjectPath: strPtr(dir),
			Tier:        strPtr("operator"),
		},
		// Inject a scripted backend so backend-backed commands (/compact via
		// RunCheckpoint + RunMemoryDistill) resolve deterministically and OFFLINE —
		// the real client would otherwise reach for the hardcoded local endpoint.
		BackendOverride: fakeBackend{},
	})
	if err != nil {
		t.Fatalf("app.Create: %v", err)
	}
	t.Cleanup(func() { _ = a.Shutdown() })
	return a
}

func ui(a *app.App, line string) UICommandResult {
	return HandleUICommand(context.Background(), line, a)
}

// TestUICompactManualBreadcrumbAndStages pins the manual /compact end-to-end against
// the scripted backend: the three stage labels stream in order, the compaction note
// carries the checkpoint AND the archived-transcript breadcrumb (the escape hatch back
// to the discarded history), and the completion message reports the token estimate.
func TestUICompactManualBreadcrumbAndStages(t *testing.T) {
	a := newOfflineApp(t)
	a.Session.InjectNote("UNIQUE_COMPACT_DETAIL: canary deploys go to studio-05")
	a.Session.InjectNote("a second note so the history is non-trivial")

	var stages []string
	res := HandleUICommandWithProgress(context.Background(), "/compact", a,
		func(s string) { stages = append(stages, s) })
	if !res.Handled || !strings.HasPrefix(res.Text, "Conversation compacted") {
		t.Fatalf("unexpected /compact result: %+v", res)
	}
	if got, want := strings.Join(stages, "|"),
		"Compacting conversation…|Applying compaction…|Distilling memories…"; got != want {
		t.Fatalf("stages = %q, want %q", got, want)
	}
	note := a.Session.Messages()[domain.ControlMessageCount].ContentToText()
	if !strings.Contains(note, "checkpoint") || !strings.Contains(note, "compacted") {
		t.Fatalf("first working message should be the checkpoint note, got %q", note)
	}
	if !strings.Contains(note, "artifact.read") || !strings.Contains(note, "artifact_") {
		t.Fatalf("note missing the archived-transcript breadcrumb: %q", note)
	}
}

func TestUIStatusReportsMCPAndConfig(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/status")
	if !r.Handled || r.Title != "Status" {
		t.Fatalf("status not handled: %+v", r)
	}
	if !strings.Contains(r.Text, "Daintree MCP") || !strings.Contains(r.Text, "disconnected") {
		t.Fatalf("status text missing MCP/disconnected: %q", r.Text)
	}
	for _, want := range []string{"backend", "project", "session", "state", "tier"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("status text missing %q: %q", want, r.Text)
		}
	}
	for _, stale := range []string{"deepseekApiKey", "largeModel", "workflowIntelligencefalse"} {
		if strings.Contains(r.Text, stale) {
			t.Errorf("status text contains stale/malformed field %q: %q", stale, r.Text)
		}
	}
}

func TestUIRejectsInvalidConstrainedArguments(t *testing.T) {
	a := newOfflineApp(t)
	tests := []struct {
		line string
		want string
	}{
		{line: "/inbox bananas", want: "Unknown severity"},
		{line: "/audit bananas", want: "Usage: /audit"},
		{line: "/audit 5 extra", want: "Usage: /audit"},
		{line: "/status extra", want: "Usage: /status"},
		{line: "/clear extra", want: "Usage: /clear"},
		{line: "/quit extra", want: "Usage: /quit"},
		{line: "/workflow cancel wfg_example extra", want: "Usage: /workflow cancel"},
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			r := ui(a, tt.line)
			if !r.Handled || !strings.Contains(r.Text, tt.want) {
				t.Fatalf("%s = %+v, want %q", tt.line, r, tt.want)
			}
			if r.Quit || r.ClearTranscript || r.SwitchPanel != "" {
				t.Fatalf("invalid command arguments caused an action: %+v", r)
			}
		})
	}
}

func TestUIFilteredPanelCommandsRenderTheirExactResult(t *testing.T) {
	a := newOfflineApp(t)
	for _, line := range []string{"/inbox urgent", "/audit 2"} {
		r := ui(a, line)
		if !r.Handled || r.SwitchPanel != "" || r.Text == "" {
			t.Fatalf("%s should render a result card, not an independently-filtered panel: %+v", line, r)
		}
	}
}

func TestUIAuditExportRendersInsteadOfDiscardingTextInPanel(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/audit export json actor=main n=1")
	if !r.Handled || r.SwitchPanel != "" || !strings.HasPrefix(r.Text, "[") {
		t.Fatalf("audit export should render a transcript card: %+v", r)
	}
}

func TestToolDescriptionsAreCompactedForCommandOutput(t *testing.T) {
	in := "first line\n\nsecond   line " + strings.Repeat("x", 120)
	got := truncateText(strings.Join(strings.Fields(in), " "), 40)
	if strings.Contains(got, "\n") || len([]rune(got)) > 40 || !strings.HasSuffix(got, "…") {
		t.Fatalf("compacted tool description = %q", got)
	}
}

func TestUIPermissionsSwitchAndReject(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/permissions supervisor")
	if !strings.Contains(r.Text, "supervisor") || a.Tier() != domain.TierSupervisor {
		t.Fatalf("permissions switch failed: tier=%v text=%q", a.Tier(), r.Text)
	}
	before := a.Tier()
	r = ui(a, "/permissions wizard")
	if !strings.Contains(r.Text, "Unknown tier") {
		t.Fatalf("unknown tier not reported: %q", r.Text)
	}
	if a.Tier() != before {
		t.Fatalf("rejected tier mutated config to %v", a.Tier())
	}
}

// TestUIPermissionsWarnsOnSessionTierChange: changing the tier away from the boot tier
// (operator, from newOfflineApp) warns it's session-only and names the env var to set.
func TestUIPermissionsWarnsOnSessionTierChange(t *testing.T) {
	a := newOfflineApp(t) // boots at operator → InitialTier=operator
	r := ui(a, "/permissions supervisor")
	if !strings.Contains(r.Text, "session only") {
		t.Fatalf("tier change missing the session-only warning: %q", r.Text)
	}
	if !strings.Contains(r.Text, "DAINTREE_ASSISTANT_TIER=supervisor") {
		t.Fatalf("tier change missing the make-it-stick hint: %q", r.Text)
	}
	if !strings.Contains(r.Text, "operator") {
		t.Fatalf("tier change should name the revert (boot) tier operator: %q", r.Text)
	}
	// The bare query keeps warning while the live tier still diverges from boot.
	if r = ui(a, "/permissions"); !strings.Contains(r.Text, "session only") {
		t.Fatalf("bare /permissions should keep warning while diverged: %q", r.Text)
	}
}

// TestUIPermissionsNoWarnWhenTierUnchanged: re-asserting the boot tier (or just querying
// it) must not emit the session-only warning.
func TestUIPermissionsNoWarnWhenTierUnchanged(t *testing.T) {
	a := newOfflineApp(t) // boots at operator
	if r := ui(a, "/permissions operator"); strings.Contains(r.Text, "session only") {
		t.Fatalf("re-asserting the boot tier should not warn: %q", r.Text)
	}
	if r := ui(a, "/permissions"); strings.Contains(r.Text, "session only") {
		t.Fatalf("bare /permissions warned with a matching tier: %q", r.Text)
	}
}

// TestUIApprovalsHandledFallback: /approvals stays registered so typing it gets the
// truth rather than "unknown command" (which reads as a typo). The session
// allow-list it used to manage was a terminal-UI feature and no longer exists, so the
// one thing this must NOT do is describe affordances that are gone — the previous
// text sent people to look for "A" and "F" keys on a sheet that was deleted.
func TestUIApprovalsHandledFallback(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/approvals")
	if !r.Handled || r.Title != "Approvals" {
		t.Fatalf("/approvals not handled: %+v", r)
	}
	if !strings.Contains(r.Text, "no session approval allow-list") {
		t.Fatalf("/approvals must say the allow-list is gone: %q", r.Text)
	}
	// Guard the actual regression: never point at a UI affordance that was deleted.
	for _, gone := range []string{"Press A", "Press F", "attached session"} {
		if strings.Contains(r.Text, gone) {
			t.Fatalf("/approvals references a removed affordance %q: %s", gone, r.Text)
		}
	}
}

func TestUIInboxSwitchesPanel(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/inbox")
	if r.SwitchPanel != PanelInbox {
		t.Fatalf("inbox panel = %q", r.SwitchPanel)
	}
	if !strings.Contains(r.Title, "Inbox") {
		t.Fatalf("inbox title = %q", r.Title)
	}
}

func TestUIAuditGrantSourceBracket(t *testing.T) {
	a := newOfflineApp(t)
	src := domain.GrantSourceLocal
	gid := "grt_x"
	if _, err := a.Store.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorWatcher, ToolName: "git.commit", ArgsJson: "{}",
		Outcome: "grant_ok", DurationMs: 5, Summary: "committed",
		GrantSource: &src, GrantID: &gid,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}",
		Outcome: "ok", DurationMs: 1, Summary: "read",
	}); err != nil {
		t.Fatal(err)
	}
	r := ui(a, "/audit")
	if r.SwitchPanel != PanelAudit {
		t.Fatalf("audit panel = %q", r.SwitchPanel)
	}
	if !strings.Contains(r.Text, "grant_ok[local]") {
		t.Fatalf("grant_ok source bracket missing: %q", r.Text)
	}
	// A plain ok row is not tagged with a source bracket.
	var readLine string
	for _, l := range strings.Split(r.Text, "\n") {
		if strings.Contains(l, "fs.read") {
			readLine = l
		}
	}
	if !strings.Contains(readLine, " ok ") || strings.Contains(readLine, "[") {
		t.Fatalf("plain ok row mis-tagged: %q", readLine)
	}
}

func TestUIAuditSourcelessGrantOK(t *testing.T) {
	a := newOfflineApp(t)
	// grant_ok with no GrantSource (pre-v4 rows) must render plain, never [undefined].
	if _, err := a.Store.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorWatcher, ToolName: "git.push", ArgsJson: "{}",
		Outcome: "grant_ok", DurationMs: 2, Summary: "pushed",
	}); err != nil {
		t.Fatal(err)
	}
	r := ui(a, "/audit")
	var line string
	for _, l := range strings.Split(r.Text, "\n") {
		if strings.Contains(l, "git.push") {
			line = l
		}
	}
	if !strings.Contains(line, "grant_ok ") || strings.Contains(line, "grant_ok[") {
		t.Fatalf("sourceless grant_ok mis-rendered: %q", line)
	}
}

func TestUIAuditExportJSON(t *testing.T) {
	a := newOfflineApp(t)
	_, _ = a.Store.InsertAudit(domain.AuditRecord{
		Ts: 1000, Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}",
		Outcome: "ok", DurationMs: 1, Summary: "read",
	})
	_, _ = a.Store.InsertAudit(domain.AuditRecord{
		Ts: 2000, Actor: domain.ActorWatcher, ToolName: "git.commit", ArgsJson: "{}",
		Outcome: "error", DurationMs: 2, Summary: "boom",
	})
	r := ui(a, "/audit export json actor=main")
	if r.SwitchPanel != "" || !strings.Contains(r.Title, "Audit") {
		t.Fatalf("export json card wrong: %+v", r)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(r.Text), &rows); err != nil {
		t.Fatalf("export json not parseable: %v\n%s", err, r.Text)
	}
	if len(rows) != 1 || rows[0]["toolName"] != "fs.read" {
		t.Fatalf("filtered export wrong: %v", rows)
	}
}

func TestUIAuditExportCSV(t *testing.T) {
	a := newOfflineApp(t)
	_, _ = a.Store.InsertAudit(domain.AuditRecord{
		Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}",
		Outcome: "ok", DurationMs: 1, Summary: "read",
	})
	r := ui(a, "/audit export csv")
	header := strings.SplitN(r.Text, "\r\n", 2)[0]
	if !strings.Contains(header, "id,ts,actor,toolName") {
		t.Fatalf("csv header missing: %q", header)
	}
	if !strings.Contains(r.Text, "fs.read") {
		t.Fatalf("csv data row missing: %q", r.Text)
	}
}

func TestUIAuditExportBadFormat(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/audit export xml")
	if r.SwitchPanel != "" {
		t.Fatalf("export panel = %q", r.SwitchPanel)
	}
	if !strings.Contains(strings.ToLower(r.Text), "use export") && !strings.Contains(r.Text, "Unknown") {
		t.Fatalf("bad format not reported as usage error: %q", r.Text)
	}
}

func TestUIExplainEmptyIndex(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/explain")
	if !r.Handled {
		t.Fatal("explain not handled")
	}
	if !strings.Contains(r.Text, "no runs recorded") {
		t.Fatalf("empty explain index wrong: %q", r.Text)
	}
}

func TestUIExplainListsRuns(t *testing.T) {
	a := newOfflineApp(t)
	_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 0, Type: "assistant:start"})
	end := `{"content":"hi"}`
	_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 1, Type: "assistant:end", Payload: &end})
	r := ui(a, "/explain")
	if !strings.Contains(r.Text, "run_a") || !strings.Contains(r.Text, "/explain <runId>") {
		t.Fatalf("explain list wrong: %q", r.Text)
	}
}

func TestUIExplainTimeline(t *testing.T) {
	a := newOfflineApp(t)
	if _, err := a.Store.InsertAudit(domain.AuditRecord{
		ID: "aud_1", Actor: domain.ActorMain, ToolName: "fs.read", ArgsJson: "{}",
		Outcome: "ok", DurationMs: 7, Summary: "read a file", RunID: strPtr("run_x"),
	}); err != nil {
		t.Fatal(err)
	}
	add := func(seq int, typ, payload string) {
		var p *string
		if payload != "" {
			p = &payload
		}
		_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_x", Seq: seq, Type: typ, Payload: p})
	}
	add(0, "assistant:start", "")
	add(1, "tool:call", `{"id":"c1","name":"fs.read","args":{"path":"a.ts"}}`)
	add(2, "tool:result", `{"id":"c1","name":"fs.read","ok":true,"summary":"read a file","auditId":"aud_1"}`)
	add(3, "assistant:end", `{"content":"done","reasoning":"thought about it"}`)

	r := ui(a, "/explain run_x")
	if r.SwitchPanel != "" {
		t.Fatalf("explain routes through transcript card, not a panel; got %q", r.SwitchPanel)
	}
	for _, want := range []string{"fs.read", "7ms", "thought about it", "done"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("explain timeline missing %q: %q", want, r.Text)
		}
	}
}

func TestUIExplainUnknownRun(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/explain run_missing")
	if !r.Handled || !strings.Contains(r.Text, "No events found") {
		t.Fatalf("unknown run wrong: %+v", r)
	}
}

func TestUIExplainMalformedPayload(t *testing.T) {
	a := newOfflineApp(t)
	bad := "{not json"
	_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_bad", Seq: 0, Type: "tool:call", Payload: &bad})
	r := ui(a, "/explain run_bad")
	if !r.Handled {
		t.Fatal("malformed payload crashed handler")
	}
	if !strings.Contains(r.Text, "tool") {
		t.Fatalf("malformed payload should still name a tool entry: %q", r.Text)
	}
}

func TestUIExplainTruncationNotice(t *testing.T) {
	a := newOfflineApp(t)
	p := `{"truncated":true,"bytes":9000,"preview":"the start of a very long answer"}`
	_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_trunc", Seq: 0, Type: "assistant:end", Payload: &p})
	r := ui(a, "/explain run_trunc")
	if !strings.Contains(r.Text, "truncated") || !strings.Contains(r.Text, "the start of a very long answer") {
		t.Fatalf("truncation notice wrong: %q", r.Text)
	}
}

func TestUIExplainFullArgRunID(t *testing.T) {
	a := newOfflineApp(t)
	_, _ = a.Store.InsertRunEvent(domain.RunEventRecord{RunID: "run_a", Seq: 0, Type: "assistant:start"})
	r := ui(a, "/explain run_a")
	if !strings.Contains(r.Title, "Explain") || strings.Contains(r.Text, "No events found") {
		t.Fatalf("full-arg runId resolution wrong: %+v", r)
	}
}

func TestUIToolsListsRegistry(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/tools")
	if !strings.HasPrefix(r.Title, "Tools") {
		t.Fatalf("tools title = %q", r.Title)
	}
	if len(r.Text) == 0 {
		t.Fatal("tools text empty")
	}
}

func TestUIModelsReportsRouting(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/models")
	if !r.Handled || r.Title != "Models" || len(r.Text) == 0 {
		t.Fatalf("models card wrong: %+v", r)
	}
}

func TestUIHelpListsEveryCommand(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/help")
	if r.SwitchPanel != PanelHelp {
		t.Fatalf("help panel = %q", r.SwitchPanel)
	}
	for _, line := range HelpLines() {
		if !strings.Contains(r.Text, line) {
			t.Fatalf("help text missing registry line %q", line)
		}
	}
	if !strings.Contains(r.Text, "/models") {
		t.Fatal("help text missing /models (issue #50)")
	}
}

func TestUIClearResetsSession(t *testing.T) {
	a := newOfflineApp(t)
	a.Session.InjectNote("some history")
	if len(a.Session.Messages()) <= domain.ControlMessageCount {
		t.Fatal("inject did not grow history")
	}
	r := ui(a, "/clear")
	if !r.Handled || r.Title != "Clear" || !r.ClearTranscript {
		t.Fatalf("clear card wrong: %+v", r)
	}
	if !strings.Contains(strings.ToLower(r.Text), "fresh") {
		t.Fatalf("clear text wrong: %q", r.Text)
	}
	if len(a.Session.Messages()) != domain.ControlMessageCount {
		t.Fatalf("session not reset to %d controls, has %d", domain.ControlMessageCount, len(a.Session.Messages()))
	}
	for _, m := range a.Session.Messages() {
		if strings.Contains(m.ContentToText(), "some history") {
			t.Fatal("leftover history after clear")
		}
	}
}

// --- #5: /clear is rejected (not desynced) while a turn is in flight ---

// fakeBackend is a scripted Daintree backend for the commands tests. It satisfies the
// full backend.Backend surface (so newOfflineApp can inject it via
// app.CreateOptions.BackendOverride) and, by subset, agent.AssistantBackend (so a
// hand-built Session can drive it). RespondStream optionally BLOCKS on `release`,
// holding a turn in flight so the /clear-during-a-turn guard can be exercised; RunTask
// returns scripted checkpoint/distill outputs so the /compact path resolves offline.
type fakeBackend struct {
	// release, when non-nil, makes RespondStream block until it is closed (or ctx is
	// cancelled) — the in-flight-turn fixture that keeps Session.inFlight set so
	// Session.Clear() returns ErrTurnInProgress. nil ⇒ RespondStream returns at once.
	release <-chan struct{}
}

func (f fakeBackend) RespondStream(ctx context.Context, _ backend.RespondRequest, cb backend.StreamCallbacks) (backend.RespondResult, error) {
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return backend.RespondResult{}, ctx.Err()
		}
	}
	// Mirror the production backendconv flow: meta first (state token), then content.
	if cb.OnMeta != nil {
		cb.OnMeta(backend.StreamMeta{State: "s1"})
	}
	if cb.OnContent != nil {
		cb.OnContent("done")
	}
	return backend.RespondResult{
		Meta:    backend.StreamMeta{State: "s1"},
		Message: backend.RespondMessage{Role: "assistant", Content: "done"},
	}, nil
}

func (fakeBackend) RunTask(_ context.Context, req backend.TaskRequest) (backend.TaskResult, error) {
	out := json.RawMessage(`{}`)
	switch req.Task {
	case backend.TaskCheckpoint:
		out = json.RawMessage(`{"goal":"compacted"}`)
	case backend.TaskMemoryDistill:
		out = json.RawMessage(`{"facts":[]}`)
	}
	return backend.TaskResult{Task: req.Task, Output: out}, nil
}

func (fakeBackend) VerifyKey(context.Context) (backend.KeyVerification, error) {
	return backend.KeyVerification{Valid: true}, nil
}

func (fakeBackend) Capabilities(context.Context) (backend.Capabilities, error) {
	return backend.Capabilities{}, nil
}
func (fakeBackend) Version(context.Context) (backend.Version, error) { return backend.Version{}, nil }
func (fakeBackend) Health(context.Context) error                     { return nil }
func (fakeBackend) Ready(context.Context) error                      { return nil }
func (fakeBackend) BaseURL() string                                  { return "http://fake-backend" }

// noopRunner satisfies agent.ToolRunner with no tools (the turn never dispatches).
type noopRunner struct{}

func (noopRunner) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (noopRunner) ResolveWireName(string) string                   { return "" }
func (noopRunner) Dispatch(context.Context, string, string, agent.TurnContext) domain.ToolResult {
	return domain.Ok("ok", nil)
}

func TestUIClearRejectedDuringActiveTurn(t *testing.T) {
	a := newOfflineApp(t)
	release := make(chan struct{})
	// Swap in a session whose turn blocks (inFlight stays set) so /clear races a turn.
	a.Session = agent.NewSession(agent.SessionDeps{
		Backend:   fakeBackend{release: release},
		Tools:     noopRunner{},
		SessionID: "sess_test",
	})
	a.Session.InjectNote("important history")
	historyBefore := len(a.Session.Messages())

	// Start the blocking turn on a goroutine.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = a.Session.Send(context.Background(), "go", agent.SendOptions{})
	}()

	// Wait until the turn is actually in flight (Clear returns ErrTurnInProgress).
	deadline := time.Now().Add(2 * time.Second)
	for a.Session.Clear() == nil {
		if time.Now().After(deadline) {
			t.Fatal("turn never became in-flight")
		}
		// Clear succeeded (turn not yet in flight) — re-seed and retry.
		a.Session.InjectNote("important history")
		time.Sleep(time.Millisecond)
	}

	// /clear DURING the in-flight turn must be REJECTED: no ClearTranscript, a note is
	// surfaced, and the session history is left intact (never wiped mid-stream, #5).
	r := ui(a, "/clear")
	if !r.Handled || r.ClearTranscript {
		t.Fatalf("mid-turn /clear must be rejected (no ClearTranscript): %+v", r)
	}
	if !strings.Contains(strings.ToLower(r.Text), "in progress") {
		t.Errorf("rejection note missing: %q", r.Text)
	}
	if len(a.Session.Messages()) < historyBefore {
		t.Error("mid-turn /clear must not wipe the session history")
	}

	// Release the turn and ensure a post-turn /clear now succeeds.
	close(release)
	wg.Wait()
	r2 := ui(a, "/clear")
	if !r2.ClearTranscript {
		t.Errorf("/clear after the turn settled must succeed: %+v", r2)
	}
}

// There is no /runbooks command. Backend runbook selection is prompt-assembly machinery the
// user neither approves nor steers, so the CLI reports it nowhere — not in the transcript,
// not behind a command (see agent.Session.emitRunbookLoads). The NAME is deliberately kept
// free for future user-authored assistant runbooks, which are intent-driven and will want
// it for list/create/edit. Pinned so it is not re-added out of reflex.
func TestUIRunbooksCommandDoesNotExist(t *testing.T) {
	a := newOfflineApp(t)
	// An unrecognized command is still "handled" — as the Unknown-command card — so the
	// assertion is on the TITLE, not on Handled.
	if r := ui(a, "/runbooks"); r.Title != "Unknown command" {
		t.Fatalf("/runbooks resolves to a real command again: %+v", r)
	}
	for _, c := range COMMAND_REGISTRY {
		if c.Name == "runbooks" {
			t.Fatalf("the runbooks command is back in the registry: %+v", c)
		}
	}
}

func TestUIUnknownCommand(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/frobnicate")
	if r.Title != "Unknown command" {
		t.Fatalf("unknown command card wrong: %+v", r)
	}
}

// TestUIEveryRegistryCommandHandled: a registered command must never fall through
// to the unknown-command default branch (no list/switch drift).
func TestUIEveryRegistryCommandHandled(t *testing.T) {
	a := newOfflineApp(t)
	for _, cmd := range COMMAND_REGISTRY {
		r := ui(a, "/"+cmd.Name)
		if !r.Handled {
			t.Errorf("/%s not handled", cmd.Name)
		}
		if r.Title == "Unknown command" {
			t.Errorf("/%s fell through to unknown-command", cmd.Name)
		}
	}
}

// TestUIMemoryCommand: /memory list shows the store; pin marks the memory pinned (and,
// post issue #263, surfaces it in the uncached footer — never message[1]); unpin/forget
// reverse it. Curation must not fall through to unknown.
func TestUIMemoryCommand(t *testing.T) {
	a := newOfflineApp(t)
	rec, err := a.Store.InsertMemory(domain.MemoryRecord{Content: "deploy uses fireworks tokens", Source: domain.MemoryUser})
	if err != nil {
		t.Fatal(err)
	}

	// list surfaces the memory id.
	if r := ui(a, "/memory list"); !r.Handled || !strings.Contains(r.Text, rec.ID) {
		t.Fatalf("/memory list missing the memory: %q", r.Text)
	}

	// pin → the list now marks it pinned (📌); the pin must NOT be injected into message[1]
	// (it surfaces in the per-round footer now, so no RefreshRuntimeContext runs).
	if r := ui(a, "/memory pin "+rec.ID); !strings.Contains(r.Text, "Pinned") {
		t.Fatalf("/memory pin: %q", r.Text)
	}
	if r := ui(a, "/memory list"); !strings.Contains(r.Text, "📌") {
		t.Fatalf("/memory list should mark the pinned memory: %q", r.Text)
	}
	if msgs := a.Session.Messages(); len(msgs) >= 2 && strings.Contains(msgs[1].StringContent, "deploy uses fireworks tokens") {
		t.Fatal("a pin must NOT be injected into message[1] (it surfaces in the footer now)")
	}

	// unpin → the list no longer marks it pinned.
	if r := ui(a, "/memory unpin "+rec.ID); !strings.Contains(r.Text, "Unpinned") {
		t.Fatalf("/memory unpin: %q", r.Text)
	}
	if r := ui(a, "/memory list"); strings.Contains(r.Text, "📌") {
		t.Fatalf("/memory list must not mark any memory pinned after unpin: %q", r.Text)
	}

	// forget removes it from the list.
	if r := ui(a, "/memory forget "+rec.ID); !strings.Contains(r.Text, "Forgot") {
		t.Fatalf("/memory forget: %q", r.Text)
	}

	// unknown id reports cleanly (not an unknown-command fall-through).
	r := ui(a, "/memory pin mem_does_not_exist")
	if !r.Handled || !strings.Contains(r.Text, "No such memory") {
		t.Fatalf("/memory pin unknown id: %+v", r)
	}
	// Trailing garbage must not turn a malformed state-changing command into a mutation.
	if r := ui(a, "/memory pin "+rec.ID+" extra"); !strings.Contains(r.Text, "Usage:") {
		t.Fatalf("/memory pin accepted trailing arguments: %+v", r)
	}
	if rows, err := a.Store.ListMemories(storage.MemoryListOptions{}); err != nil || len(rows) != 0 {
		t.Fatalf("invalid trailing-argument command mutated memories: rows=%+v err=%v", rows, err)
	}
}

// TestDoctorNoProbeWhenDisconnected: with MCP offline/disconnected, runDoctor adds
// no "mcp probe" check (the live probe is connection-gated).
func TestDoctorNoProbeWhenDisconnected(t *testing.T) {
	a := newOfflineApp(t)
	foundMode := false
	for _, c := range RunDoctor(context.Background(), a) {
		if c.Label == "mcp probe" {
			t.Fatalf("disconnected MCP must add no probe check, got %+v", c)
		}
		if c.Label == "mcp mode" && c.OK && strings.Contains(c.Detail, "offline") {
			foundMode = true
		}
		if c.OK && c.Fix != "" {
			t.Fatalf("successful doctor check must not carry remediation: %+v", c)
		}
	}
	if !foundMode {
		t.Fatal("explicit offline mode should collapse MCP checks into one informational row")
	}
}

// TestUIGrantsEmptyAndLive: /grants reports "(none)" with an empty store and a text
// card (no panel switch), and renders a live grant's id, actor, source, uses, and the
// allowed risk classes once seeded.
func TestUIGrantsEmptyAndLive(t *testing.T) {
	a := newOfflineApp(t)
	if r := ui(a, "/grants"); !r.Handled || r.Title != "Grants" || r.Text != "(none)" {
		t.Fatalf("empty /grants: %+v", r)
	}
	if r := ui(a, "/grants"); r.SwitchPanel != "" {
		t.Fatalf("/grants must not switch panels, got %q", r.SwitchPanel)
	}
	future := time.Now().Add(time.Hour).UnixMilli()
	risks := `["project"]`
	tools := `["git.commit"]`
	// MaxUses=5 with UsesRemaining=2 (explicit, so InsertGrant's "0 ⇒ MaxUses" default
	// does not overwrite it) pins the uses=remaining/max display order. Both lists are
	// set so grantAllowSummary's risks= AND tools= branches are exercised.
	rec, err := a.Store.InsertGrant(domain.AutomationGrantRecord{
		ActorID:                "wch_abc",
		ActorType:              domain.GrantActorWatcher,
		AllowedRiskClassesJson: &risks,
		AllowedToolNamesJson:   &tools,
		ExpiresAt:              future,
		MaxUses:                5,
		UsesRemaining:          2,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := ui(a, "/grants")
	for _, want := range []string{rec.ID, "wch_abc", "watcher", "uses=2/5", "risks=project", "tools=git.commit"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("/grants missing %q: %q", want, r.Text)
		}
	}
}

// TestUIWorkflowsDefaultActiveAndFilters: bare /workflows shows only active runs (and
// the issue ref), "/workflows all" shows every status, "/workflows done" filters to done.
func TestUIWorkflowsDefaultActiveAndFilters(t *testing.T) {
	a := newOfflineApp(t)
	if r := ui(a, "/workflows"); !r.Handled || r.Title != "Workflows" || r.Text != "(none)" {
		t.Fatalf("empty /workflows: %+v", r)
	}
	issueNum := 170
	issueTitle := "Add inspection commands"
	active, err := a.Store.InsertWorkflowRun(domain.WorkflowRunRecord{
		Status:      domain.WorkflowActive,
		IssueNumber: &issueNum,
		IssueTitle:  &issueTitle,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := a.Store.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowDone})
	if err != nil {
		t.Fatal(err)
	}

	// Bare /workflows defaults to active; the done run must not appear.
	r := ui(a, "/workflows")
	if !strings.Contains(r.Text, active.ID) || strings.Contains(r.Text, done.ID) {
		t.Fatalf("default /workflows should show only active: %q", r.Text)
	}
	if !strings.Contains(r.Text, "issue #170") || !strings.Contains(r.Text, "Add inspection commands") {
		t.Fatalf("/workflows missing the issue ref: %q", r.Text)
	}
	// "all" drops the filter — both statuses appear.
	if r := ui(a, "/workflows all"); !strings.Contains(r.Text, active.ID) || !strings.Contains(r.Text, done.ID) {
		t.Fatalf("/workflows all should show both: %q", r.Text)
	}
	// An explicit status filters to that status only.
	if r := ui(a, "/workflows done"); strings.Contains(r.Text, active.ID) || !strings.Contains(r.Text, done.ID) {
		t.Fatalf("/workflows done should show only done: %q", r.Text)
	}
}

// TestUILaunchesEmptyAndWithError: /launches reports "(none)" when empty and renders a
// failed launch's id, stage, mode, title, and error code+message once seeded.
func TestUILaunchesEmptyAndWithError(t *testing.T) {
	a := newOfflineApp(t)
	if r := ui(a, "/launches"); !r.Handled || r.Title != "Launches" || r.Text != "(none)" {
		t.Fatalf("empty /launches: %+v", r)
	}
	code := "spawn_failed"
	msg := "worktree busy"
	rec, err := a.Store.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "idem-1",
		AgentID:        "agent-1",
		Mode:           "edit",
		Title:          "Fix the bug",
		Name:           "bugfix",
		Stage:          domain.LaunchFailed,
		ErrorCode:      &code,
		ErrorMessage:   &msg,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := ui(a, "/launches")
	for _, want := range []string{rec.ID, "failed", "edit", "Fix the bug", "spawn_failed", "worktree busy"} {
		if !strings.Contains(r.Text, want) {
			t.Fatalf("/launches missing %q: %q", want, r.Text)
		}
	}
}

// TestUILaunchesNoErrorSuffix: a launch with no error code/message must NOT render a
// trailing " — error:" suffix (guards the ErrorCode/ErrorMessage nil checks).
func TestUILaunchesNoErrorSuffix(t *testing.T) {
	a := newOfflineApp(t)
	rec, err := a.Store.InsertAgentLaunch(domain.AgentLaunchRecord{
		IdempotencyKey: "idem-ok",
		AgentID:        "agent-ok",
		Mode:           "explore",
		Title:          "Survey the layout",
		Name:           "survey",
		Stage:          domain.LaunchConfirmed,
	})
	if err != nil {
		t.Fatal(err)
	}
	r := ui(a, "/launches")
	if !strings.Contains(r.Text, rec.ID) || !strings.Contains(r.Text, "confirmed") {
		t.Fatalf("/launches missing the confirmed launch: %q", r.Text)
	}
	if strings.Contains(r.Text, "error:") {
		t.Fatalf("/launches appended an error suffix to an error-free launch: %q", r.Text)
	}
}

// TestUIWorkflowsCapCaseAndInvalid: the display caps at 20 rows with a "(+N more)"
// trailer, mixed-case status args are normalized, and an unknown status is rejected
// with the accepted values instead of masquerading as a valid empty result.
func TestUIWorkflowsCapCaseAndInvalid(t *testing.T) {
	a := newOfflineApp(t)
	for i := 0; i < 21; i++ {
		if _, err := a.Store.InsertWorkflowRun(domain.WorkflowRunRecord{Status: domain.WorkflowActive}); err != nil {
			t.Fatal(err)
		}
	}
	// Bare /workflows defaults to active: 20 rows shown + a "(+1 more)" trailer.
	r := ui(a, "/workflows")
	if n := strings.Count(r.Text, "[active]"); n != 20 {
		t.Fatalf("/workflows should cap at 20 rows, rendered %d: %q", n, r.Text)
	}
	if !strings.Contains(r.Text, "(+1 more)") {
		t.Fatalf("/workflows missing the overflow trailer: %q", r.Text)
	}
	// Mixed-case status is normalized and still matches the stored lowercase status.
	if r := ui(a, "/workflows ACTIVE"); !strings.Contains(r.Text, "[active]") {
		t.Fatalf("/workflows ACTIVE should match active runs: %q", r.Text)
	}
	// An unknown status is a usage error, not a valid empty filter.
	if r := ui(a, "/workflows nope"); !strings.Contains(r.Text, "Unknown workflow status") || !strings.Contains(r.Text, "pending|active") {
		t.Fatalf("/workflows nope should report accepted statuses: %q", r.Text)
	}
}

// capTail is the recency-preserving cap used by BOTH the manual /compact summary and
// distillation paths (the oldest-head capHead is gone — issue #253). It must keep the
// freshest TAIL, leave under-cap input untouched, and slice on rune (not byte)
// boundaries so multi-byte content is never split mid-rune.
func TestCapTail(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{"empty", "", 5, ""},
		{"under cap is identity", "abc", 5, "abc"},
		{"exactly at cap is identity", "abcde", 5, "abcde"},
		{"over cap keeps the freshest tail", "abcdefgh", 3, "fgh"},
		{"zero cap drops everything", "abc", 0, ""},
		// Multi-byte runes: each emoji is 4 bytes, so a byte-based slice would corrupt
		// them. capTail must count runes — keeping the last two of three emojis whole.
		{"counts runes not bytes", "😀😁😂", 2, "😁😂"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := capTail(tc.in, tc.maxRunes); got != tc.want {
				t.Errorf("capTail(%q, %d) = %q, want %q", tc.in, tc.maxRunes, got, tc.want)
			}
		})
	}
}
