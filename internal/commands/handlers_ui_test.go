package commands

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
	"github.com/daintreehq/daintree-assistant/internal/skills"
)

// Exercises handleUiCommand + the disconnected /doctor probe. Builds an offline
// App against a temp state dir and drives the structured cockpit slash handler,
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

func TestUIStatusReportsMCPAndConfig(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/status")
	if !r.Handled || r.Title != "Status" {
		t.Fatalf("status not handled: %+v", r)
	}
	if !strings.Contains(r.Text, "Daintree MCP") || !strings.Contains(r.Text, "disconnected") {
		t.Fatalf("status text missing MCP/disconnected: %q", r.Text)
	}
}

func TestUIPermissionsSwitchAndReject(t *testing.T) {
	a := newOfflineApp(t)
	r := ui(a, "/permissions supervisor")
	if !strings.Contains(r.Text, "supervisor") || a.Config.Tier != domain.TierSupervisor {
		t.Fatalf("permissions switch failed: tier=%v text=%q", a.Config.Tier, r.Text)
	}
	before := a.Config.Tier
	r = ui(a, "/permissions wizard")
	if !strings.Contains(r.Text, "Unknown tier") {
		t.Fatalf("unknown tier not reported: %q", r.Text)
	}
	if a.Config.Tier != before {
		t.Fatalf("rejected tier mutated config to %v", a.Config.Tier)
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
	if r.SwitchPanel != PanelAudit || !strings.Contains(r.Title, "Audit") {
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
	if r.SwitchPanel != PanelAudit {
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

// blockStreamRouter holds Stream until release is closed, keeping the session's
// inFlight set so Session.Clear() returns ErrTurnInProgress.
type blockStreamRouter struct{ release <-chan struct{} }

func (r blockStreamRouter) Stream(ctx context.Context, _ domain.ModelTier, _ models.ChatOptions, _ func(string)) (models.ChatResult, error) {
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return models.ChatResult{Content: "done"}, nil
}
func (blockStreamRouter) Chat(context.Context, domain.ModelTier, models.ChatOptions) (models.ChatResult, error) {
	return models.ChatResult{Content: "s"}, nil
}
func (blockStreamRouter) ModelFor(domain.ModelTier) string { return "m" }

// noopRunner satisfies agent.ToolRunner with no tools (the turn never dispatches).
type noopRunner struct{}

func (noopRunner) OpenAITools([]string) ([]models.ChatTool, error) { return nil, nil }
func (noopRunner) ResolveWireName(string) string                   { return "" }
func (noopRunner) ReadOnlyToolNames() []string                     { return nil }
func (noopRunner) Dispatch(context.Context, string, string, agent.TurnContext) domain.ToolResult {
	return domain.Ok("ok", nil)
}

// noopSelector / noopCatalog satisfy the skill seams with empty results.
type noopSelector struct{}

func (noopSelector) Select(context.Context, []skills.SkillMetadata, string) (skills.SkillSelection, error) {
	return skills.SkillSelection{}, nil
}

type noopCatalog struct{}

func (noopCatalog) MetadataForSelection() []skills.SkillMetadata { return nil }
func (noopCatalog) GetMany([]string) []skills.Skill              { return nil }
func (noopCatalog) Has(string) bool                              { return false }

func TestUIClearRejectedDuringActiveTurn(t *testing.T) {
	a := newOfflineApp(t)
	release := make(chan struct{})
	// Swap in a session whose turn blocks (inFlight stays set) so /clear races a turn.
	a.Session = agent.NewSession(agent.SessionDeps{
		Router:        blockStreamRouter{release: release},
		Tools:         noopRunner{},
		SkillSelector: noopSelector{},
		SkillCatalog:  noopCatalog{},
		SessionID:     "sess_test",
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

// TestDoctorNoProbeWhenDisconnected: with MCP offline/disconnected, runDoctor adds
// no "mcp probe" check (the live probe is connection-gated).
func TestDoctorNoProbeWhenDisconnected(t *testing.T) {
	a := newOfflineApp(t)
	for _, c := range RunDoctor(context.Background(), a) {
		if c.Label == "mcp probe" {
			t.Fatalf("disconnected MCP must add no probe check, got %+v", c)
		}
	}
}
