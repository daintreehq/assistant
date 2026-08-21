package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/daintreehq/assistant/internal/agent"
	"github.com/daintreehq/assistant/internal/app"
	"github.com/daintreehq/assistant/internal/backend"
	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/storage"
	"github.com/daintreehq/assistant/internal/workflowgraph"
)

// UICommandResult is the structured return of the cockpit slash handler.
// The cockpit renders a card / switches a panel; nothing is printed.
type UICommandResult struct {
	Handled         bool
	Quit            bool
	Title           string
	Text            string
	SwitchPanel     PanelKey
	ClearTranscript bool
}

// inboxSeverities is the set /inbox accepts for severityAtLeast.
var inboxSeverities = map[string]domain.Severity{
	"info":      domain.SeverityInfo,
	"attention": domain.SeverityAttention,
	"urgent":    domain.SeverityUrgent,
	"blocked":   domain.SeverityBlocked,
}

// HandleUICommand handles a slash line for the cockpit, returning structured data.
// ctx carries cancellation for the model-backed commands (e.g. compact).
func HandleUICommand(ctx context.Context, line string, a *app.App) UICommandResult {
	return HandleUICommandWithProgress(ctx, line, a, nil)
}

// HandleUICommandWithProgress is HandleUICommand plus a live stage reporter for the
// slow, model-backed commands (/compact runs two backend model calls back to back —
// tens of seconds of otherwise total silence). progress is called with short
// human-readable stage labels ("Compacting conversation…"); nil is fine (one-shot
// and callers that have nowhere to show it). It is UI-thread-agnostic: the cockpit
// routes it through the event pump, the classic REPL prints it.
func HandleUICommandWithProgress(ctx context.Context, line string, a *app.App, progress func(stage string)) UICommandResult {
	if progress == nil {
		progress = func(string) {}
	}
	cmd, arg, rest := parseCommand(line)
	if cmd == "" {
		return UICommandResult{Handled: false}
	}
	name := canonical(cmd)
	if usage := noArgUsage(name, rest); usage != "" {
		return UICommandResult{Handled: true, Title: "Usage", Text: usage}
	}
	switch name {
	case "quit":
		return UICommandResult{Handled: true, Quit: true}
	case "help":
		return UICommandResult{Handled: true, SwitchPanel: PanelHelp, Title: "Help", Text: HelpTextUI()}
	case "status":
		return UICommandResult{Handled: true, Title: "Status", Text: statusText(a)}
	case "inbox":
		title, text, showPanel := inboxView(ctx, a, arg)
		result := UICommandResult{Handled: true, Title: title, Text: text}
		if showPanel {
			result.SwitchPanel = PanelInbox
		}
		return result
	case "tools":
		title, text := toolsView(a, arg)
		return UICommandResult{Handled: true, Title: title, Text: text}
	case "timers":
		return UICommandResult{Handled: true, SwitchPanel: PanelTimers, Title: "Timers", Text: timersText(a)}
	case "watchers":
		return UICommandResult{Handled: true, SwitchPanel: PanelWatchers, Title: "Watchers", Text: watchersText(a)}
	case "grants":
		return UICommandResult{Handled: true, Title: "Grants", Text: grantsText(a)}
	case "workflows":
		return UICommandResult{Handled: true, Title: "Workflows", Text: workflowsText(a, arg)}
	case "workflow":
		title, text := workflowGraphCommand(ctx, a, rest)
		return UICommandResult{Handled: true, Title: title, Text: text}
	case "launches":
		return UICommandResult{Handled: true, Title: "Launches", Text: launchesText(a)}
	case "audit":
		text, showPanel := auditText(a, rest, false)
		result := UICommandResult{Handled: true, Title: "Audit", Text: text}
		if showPanel {
			result.SwitchPanel = PanelAudit
		}
		return result
	case "explain":
		return UICommandResult{Handled: true, Title: "Explain", Text: explainText(a, arg)}
	case "models":
		return UICommandResult{Handled: true, Title: "Models", Text: modelsText(a)}
	case "cost":
		return UICommandResult{Handled: true, Title: "Cost", Text: costText(a)}
	case "backend":
		return UICommandResult{Handled: true, Title: "Backend", Text: backendText(a, arg)}
	case "routing":
		return UICommandResult{Handled: true, Title: "Routing", Text: routingText(ctx, a)}
	case "permissions":
		return UICommandResult{Handled: true, Title: "Permissions", Text: permissionsText(a, arg)}
	case "approvals":
		// The session approval allow-list lives on the cockpit Model (the UI intercepts
		// /approvals before this handler — see ui.onSubmit). The REPL has no interactive
		// per-call approvals, so this surface only explains where the command applies.
		return UICommandResult{Handled: true, Title: "Approvals", Text: "Session tool approvals are managed in the cockpit. Press A (bounded) or F (forever this session) on an approval prompt; run /approvals there to list or clear them."}
	case "memory":
		return UICommandResult{Handled: true, Title: "Memory", Text: memoryText(a, rest)}
	case "compact":
		return UICommandResult{Handled: true, Title: "Compact", Text: compactRun(ctx, a, progress)}
	case "clear":
		// #5: only wipe the UI transcript AFTER Session.Clear() actually succeeds. A
		// mid-turn clear returns ErrTurnInProgress (a clear would corrupt the streaming
		// snapshot while the agent keeps emitting), so reject it with a note instead of
		// desyncing the UI from a still-live session.
		if err := a.Session.Clear(); err != nil {
			return UICommandResult{Handled: true, Title: "Clear", Text: "Can't clear while a turn is in progress — cancel it (Esc) or wait for it to finish, then try again."}
		}
		// /clear is a full reset: tear down every live watcher, cancel every live
		// async operation, AND resolve every open inbox event (same clean slate the
		// next session boundary produces) so no prior supervision, async future, or
		// attention item carries over. Best-effort — none may block the clear.
		_, _ = a.ClearWatchers()
		_, _ = a.ClearAsyncWork()
		_, _ = a.ClearInbox()
		// The bill belongs to the conversation being discarded. Carrying it into a
		// deliberately fresh start would make /cost answer a question nobody asked.
		a.CostLedger.Reset()
		return UICommandResult{Handled: true, ClearTranscript: true, Title: "Clear", Text: "Conversation cleared — watchers, async operations, and inbox cleared — starting fresh."}
	case "doctor":
		return UICommandResult{Handled: true, Title: "Doctor", Text: FormatDoctor(RunDoctor(ctx, a))}
	case "reconnect":
		return UICommandResult{Handled: true, Title: "Reconnect", Text: reconnectRun(ctx, a)}
	default:
		if s := suggestCommand(cmd); s != "" {
			return UICommandResult{Handled: true, Title: "Unknown command", Text: "Unknown command /" + cmd + " — did you mean /" + s + "? (/help lists all commands)"}
		}
		return UICommandResult{Handled: true, Title: "Unknown command", Text: "Unknown command /" + cmd + ". Try /help."}
	}
}

// HelpTextUI is the cockpit help blob: the command list followed by
// the key cheat-sheet, so /help (and the ? view) document the whole keymap in one place.
func HelpTextUI() string {
	lines := append([]string{}, HelpLines()...)
	lines = append(lines, "")
	lines = append(lines, KeyHelpLines()...)
	lines = append(lines, "", "Anything else goes to the assistant.")
	return strings.Join(lines, "\n")
}

// KeyHelpLines is the SINGLE source of truth for the cockpit key cheat-sheet — surfaced by
// /help and the ? view, and kept in sync with the actual dispatch in internal/ui
// (update_handlers + composer). One list so the help can never drift from the keymap.
func KeyHelpLines() []string {
	return []string{
		// Scoped to the COMPOSER on purpose: the help and operations decks rebind Esc
		// (back) and ↑/↓ (scroll), and their own footer says so. An unscoped "Keys" header
		// put this table and that footer on screen together making opposite claims.
		"Composer keys",
		"  Enter           send · mid-turn: add the text to the running turn",
		"                  · a leading / runs a command instead",
		`                  newline: modifier+Enter where your terminal supports`,
		`                  it, or a trailing \ then Enter (needs no modifier)`,
		"  ↑ / ↓           move by line, then recall prompt history",
		"  /               command palette  ·  Tab complete  ·  ↑/↓ navigate",
		"  Esc             with a draft: clear it · mid-turn with an empty",
		"                  composer: edit the latest queued follow-up, else",
		"                  cancel the running turn",
		"  Ctrl+C          cancel the running turn  ·  press again to exit",
		"  Ctrl+D          exit at an empty prompt",
		"  Ctrl+O          toggle the operations deck",
		"  Ctrl+X          toggle raw tool detail",
		"  Ctrl+L          redraw the screen (recover a corrupted footer)",
		"  ?               show this help (at an empty prompt)",
		"  ↑/↓ PgUp/PgDn   scroll the operations deck / help when it overflows",
		"  Editing         Ctrl+R search · Ctrl+A/E home/end · Ctrl+W kill word",
		"                  Ctrl+U/K kill line/to end · Ctrl+Y yank",
		"                  Alt+B/F move by word · Alt+D kill word",
	}
}

// HelpTextREPL is the REPL help blob.
func HelpTextREPL() string {
	lines := []string{"Commands"}
	for _, l := range HelpLines() {
		lines = append(lines, "  "+l)
	}
	lines = append(lines, "", "Anything else is sent to the assistant.")
	return strings.Join(lines, "\n")
}

// --- shared data accessors (used by both surfaces) ---

func statusText(a *app.App) string {
	rows := [][2]string{
		{"backend", a.Backend.BaseURL()},
		{"Daintree MCP", mcpStatusLine(mcpStatusOf(a))},
		{"project", a.Config.ProjectPath},
		{"session", a.SessionID},
		{"state", a.Config.StateDir},
		{"tier", string(a.Tier())},
		{"debug logging", enabledText(a.Config.DebugLog)},
		{"workflow graphs", enabledText(a.Config.WorkflowIntelligence)},
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, padRight(row[0], 17)+": "+row[1])
	}
	return strings.Join(lines, "\n")
}

func enabledText(on bool) string {
	if on {
		return "enabled"
	}
	return "off"
}

// mcpStatusLine mirrors app.mcpStatusLine (unexported there).
func mcpStatusLine(st mcpStatusLike) string {
	if st.Connected {
		count := "?"
		if st.ToolCount != nil {
			count = fmt.Sprintf("%d", *st.ToolCount)
		}
		return "connected (" + st.Transport + ", " + count + " tools)"
	}
	reason := st.Error
	if reason == "" {
		reason = "no url/token"
	}
	return "disconnected — " + reason
}

// mcpStatusLike is the subset of mcp.Status the status line reads (avoids leaking
// the mcp type into formatting helpers).
type mcpStatusLike struct {
	Connected bool
	Transport string
	ToolCount *int
	Error     string
}

func mcpStatusOf(a *app.App) mcpStatusLike {
	st := a.MCP.Status()
	return mcpStatusLike{Connected: st.Connected, Transport: st.Transport, ToolCount: st.ToolCount, Error: st.Error}
}

func inboxView(ctx context.Context, a *app.App, arg string) (title, text string, showPanel bool) {
	var sev *domain.Severity
	filter := strings.ToLower(strings.TrimSpace(arg))
	if filter != "" {
		s, ok := inboxSeverities[filter]
		if !ok {
			return "Inbox", "Unknown severity '" + arg + "'.\nUsage: /inbox [info|attention|urgent|blocked]", false
		}
		sev = &s
	}
	maxItems := 30
	events, err := a.Queue.Digest(ctx, domain.QueueDigestOptions{SeverityAtLeast: sev, MaxItems: &maxItems})
	if err != nil {
		return "Inbox", "Failed to read inbox: " + err.Error(), false
	}
	// The operations deck has its own fixed attention+ filter. Only bare /inbox
	// maps to that deck; an explicit severity must render this computed digest.
	return fmt.Sprintf("Inbox (%d)", len(events)), a.Queue.Format(events), filter == ""
}

func toolsView(a *app.App, arg string) (string, string) {
	all := a.Registry.List()
	q := strings.ToLower(strings.TrimSpace(arg))
	var rows []string
	matched := 0
	for _, t := range all {
		if q != "" && !strings.Contains(strings.ToLower(t.Name), q) && !strings.Contains(strings.ToLower(t.Description), q) {
			continue
		}
		matched++
		desc := truncateText(strings.Join(strings.Fields(t.Description), " "), 96)
		rows = append(rows, padRight(t.Name, 26)+"["+string(t.Risk)+"] "+desc)
	}
	title := fmt.Sprintf("Tools (%d/%d)", matched, len(all))
	if len(rows) == 0 {
		return title, "(no matching tools)"
	}
	return title, strings.Join(rows, "\n")
}

func timersText(a *app.App) string {
	timers, err := a.Store.ListTimers("scheduled")
	if err != nil {
		return "Failed to list timers: " + err.Error()
	}
	if len(timers) == 0 {
		return "(none)"
	}
	var rows []string
	for _, t := range timers {
		rows = append(rows, fmt.Sprintf("%s %s — %s (%s)", t.ID, localTime(t.FireAt), t.Title, t.PayloadType))
	}
	return strings.Join(rows, "\n")
}

func watchersText(a *app.App) string {
	watchers, err := a.Store.ListWatchers("active")
	if err != nil {
		return "Failed to list watchers: " + err.Error()
	}
	if len(watchers) == 0 {
		return "(none)"
	}
	var rows []string
	for _, w := range watchers {
		last := "pending"
		if w.LastClassification != nil && *w.LastClassification != "" {
			last = *w.LastClassification
		}
		rows = append(rows, fmt.Sprintf("%s %s — %s [%s]", w.ID, w.Title, w.Goal, last))
	}
	return strings.Join(rows, "\n")
}

// grantsText renders the LIVE automation grants (the store filters out revoked,
// expired, and use-exhausted ones). One line per grant — id, actor, source,
// uses-remaining/max, expiry, and what the grant authorizes — mirroring the
// timers/watchers card shape.
func grantsText(a *app.App) string {
	grants, err := a.Store.ListGrants("", 0)
	if err != nil {
		return "Failed to list grants: " + err.Error()
	}
	if len(grants) == 0 {
		return "(none)"
	}
	var rows []string
	for _, g := range grants {
		rows = append(rows, fmt.Sprintf("%s %s:%s [%s] uses=%d/%d expires=%s — %s",
			g.ID, string(g.ActorType), g.ActorID, string(g.Source),
			g.UsesRemaining, g.MaxUses, localTime(g.ExpiresAt), grantAllowSummary(g)))
	}
	return strings.Join(rows, "\n")
}

// grantAllowSummary describes what a grant authorizes: its allowed risk classes
// and/or tool names (the authorization is their union). "(any)" is a defensive
// fallback for a grant carrying neither list (or unparseable JSON columns); live
// grants normally hold at least one, so an empty tail is not expected in practice.
func grantAllowSummary(g domain.AutomationGrantRecord) string {
	var parts []string
	if risks := decodeJSONStringList(g.AllowedRiskClassesJson); len(risks) > 0 {
		parts = append(parts, "risks="+strings.Join(risks, ","))
	}
	if tools := decodeJSONStringList(g.AllowedToolNamesJson); len(tools) > 0 {
		parts = append(parts, "tools="+strings.Join(tools, ","))
	}
	if len(parts) == 0 {
		return "(any)"
	}
	return strings.Join(parts, " ")
}

// workflowsDisplayCap bounds the /workflows card (ListWorkflowRuns imposes no limit
// of its own); overflow is summarized as a "+N more" trailer.
const workflowsDisplayCap = 20

// workflowsText renders workflow runs. Bare /workflows shows the active runs (the
// common "what's in flight" view); "all" drops the filter; any other arg is a
// status filter (pending|active|blocked|done|cancelled|failed). Newest-first, capped
// at workflowsDisplayCap.
func workflowsText(a *app.App, arg string) string {
	// Workflow-intelligence graphs lead the card when the feature is on: they are
	// the richer, plan-shaped view of open work; the flat ledger runs follow.
	var graphHeader []string
	if svc := a.WorkflowGraphs(); svc != nil {
		if graphs, err := svc.List(workflowgraph.OpenStatuses, 5); err == nil && len(graphs) > 0 {
			graphHeader = append(graphHeader, "Workflow graphs (/workflow <id> for detail):")
			for _, g := range graphs {
				graphHeader = append(graphHeader, workflowGraphSummaryLines(g)...)
			}
			graphHeader = append(graphHeader, "", "Workflow runs (ledger):")
		}
	}

	// Normalize case so "/workflows Active" matches the stored lowercase status (a
	// literal SQL WHERE) instead of silently returning "(none)".
	status := strings.ToLower(strings.TrimSpace(arg))
	switch status {
	case "":
		status = string(domain.WorkflowActive)
	case "all":
		status = ""
	case string(domain.WorkflowPending), string(domain.WorkflowActive), string(domain.WorkflowBlocked),
		string(domain.WorkflowDone), string(domain.WorkflowCancelled), string(domain.WorkflowFailed):
		// Valid explicit status.
	default:
		return "Unknown workflow status '" + arg + "'.\nUsage: /workflows [pending|active|blocked|done|cancelled|failed|all]"
	}
	withHeader := func(body string) string {
		if len(graphHeader) == 0 {
			return body
		}
		return strings.Join(append(graphHeader, body), "\n")
	}
	runs, err := a.Store.ListWorkflowRuns(status)
	if err != nil {
		return withHeader("Failed to list workflows: " + err.Error())
	}
	if len(runs) == 0 {
		return withHeader("(none)")
	}
	more := 0
	if len(runs) > workflowsDisplayCap {
		more = len(runs) - workflowsDisplayCap
		runs = runs[:workflowsDisplayCap]
	}
	var rows []string
	for _, w := range runs {
		rows = append(rows, fmt.Sprintf("%s [%s] %s%s",
			w.ID, string(w.Status), localTime(w.UpdatedAt), workflowRef(w)))
	}
	if more > 0 {
		rows = append(rows, fmt.Sprintf("(+%d more)", more))
	}
	return withHeader(strings.Join(rows, "\n"))
}

// workflowRef appends the most identifying human-readable context for a run — issue
// number (+title), else branch, else PR number — as a " — …" suffix (empty when the
// run carries none of them).
func workflowRef(w domain.WorkflowRunRecord) string {
	switch {
	case w.IssueNumber != nil:
		s := fmt.Sprintf(" — issue #%d", *w.IssueNumber)
		if w.IssueTitle != nil && *w.IssueTitle != "" {
			s += " " + *w.IssueTitle
		}
		return s
	case w.Branch != nil && *w.Branch != "":
		return " — " + *w.Branch
	case w.PRNumber != nil:
		return fmt.Sprintf(" — PR #%d", *w.PRNumber)
	default:
		return ""
	}
}

// launchesText renders the recent agent-spawn sagas (newest first, store-capped at
// 20). One line per launch — id, stage, last-update time, mode, title — with a
// trailing error suffix when the saga carries one.
func launchesText(a *app.App) string {
	launches, err := a.Store.ListAgentLaunches(0)
	if err != nil {
		return "Failed to list launches: " + err.Error()
	}
	if len(launches) == 0 {
		return "(none)"
	}
	var rows []string
	for _, l := range launches {
		line := fmt.Sprintf("%s [%s] %s %s — %s",
			l.ID, string(l.Stage), localTime(l.UpdatedAt), l.Mode, l.Title)
		if l.ErrorCode != nil || l.ErrorMessage != nil {
			line += " — error: " + launchError(l)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// launchError joins a launch's error code and message for display (either may be nil).
func launchError(l domain.AgentLaunchRecord) string {
	code, msg := "", ""
	if l.ErrorCode != nil {
		code = *l.ErrorCode
	}
	if l.ErrorMessage != nil {
		msg = *l.ErrorMessage
	}
	switch {
	case code != "" && msg != "":
		return code + ": " + msg
	case code != "":
		return code
	default:
		return msg
	}
}

// auditText renders the audit view. showPanel is true only for a successful list;
// exports and errors must remain transcript cards because panel switches discard Text.
func auditText(a *app.App, rest []string, _ bool) (text string, showPanel bool) {
	if len(rest) > 0 && rest[0] == "export" {
		parsed := ParseAuditExportArgs(rest[1:])
		if parsed.Error != "" {
			return parsed.Error, false
		}
		rows, err := a.Store.QueryAudit(parsed.Filters)
		if err != nil {
			return "Audit export failed: " + err.Error(), false
		}
		return SerializeAudit(rows, parsed.Format), false
	}
	const usage = "Usage: /audit [n] | /audit export <json|csv> [tool=<name>] [outcome=<value>] [actor=<actor>] [n=<limit>]"
	n := 15
	if len(rest) > 0 {
		if len(rest) > 1 {
			return usage, false
		}
		v := atoiSafe(rest[0])
		if v <= 0 {
			return usage, false
		}
		n = v
	}
	rows, err := a.Store.ListAudit(n)
	if err != nil {
		return "Failed to read audit: " + err.Error(), false
	}
	if len(rows) == 0 {
		return "(no audit entries)", len(rest) == 0
	}
	var lines []string
	for _, r := range rows {
		outcome := r.Outcome
		if r.Outcome == "grant_ok" && r.GrantSource != nil {
			outcome = "grant_ok[" + string(*r.GrantSource) + "]"
		}
		lines = append(lines, fmt.Sprintf("%s %s %s %dms — %s",
			localTime(r.Ts), padRight(r.ToolName, 22), outcome, r.DurationMs, r.Summary))
	}
	// The deck owns its own fixed row limit. Only bare /audit maps to it; /audit N
	// must render the exact N-row result computed here.
	return strings.Join(lines, "\n"), len(rest) == 0
}

// noArgUsage rejects arguments to commands whose syntax has no argument form.
// Silently accepting `/clear now` or `/quit later` makes typos look successful,
// which is especially surprising for state-changing commands.
func noArgUsage(name string, rest []string) string {
	if len(rest) == 0 {
		return ""
	}
	switch name {
	case "status", "timers", "watchers", "grants", "launches", "models",
		"compact", "clear", "doctor", "reconnect", "help", "quit":
		return "Usage: /" + name
	default:
		return ""
	}
}

func truncateText(s string, limit int) string {
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	if limit < 2 {
		return string(r[:limit])
	}
	return string(r[:limit-1]) + "…"
}

func explainText(a *app.App, arg string) string {
	runID := strings.TrimSpace(arg)
	if runID == "" {
		runs, err := a.Store.ListRuns(10)
		if err != nil {
			return "Failed to list runs: " + err.Error()
		}
		return FormatRunList(runs) + "\n\nPass a runId to replay it: /explain <runId>"
	}
	events, err := a.Store.ListRunEvents(runID)
	if err != nil {
		return "Failed to read run events: " + err.Error()
	}
	if len(events) == 0 {
		return "No events found for run " + runID + "."
	}
	auditRows, _ := a.Store.ListAuditByRunID(runID)
	return FormatRunTimeline(events, auditRows)
}

func modelsText(a *app.App) string {
	// Model routing is owned by the Daintree backend now; the CLI no longer selects a
	// model tier. Surface the backend endpoint + its public model id.
	return "Model routing is owned by the Daintree backend.\n" +
		padRight("backend", 8) + ": " + a.Backend.BaseURL() + "\n" +
		padRight("model", 8) + ": daintree-assistant"
}

func backendText(a *app.App, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return a.DescribeBackendChoices()
	}
	return BackendSwitchText(a, arg)
}

// BackendSwitchText applies a backend selection and renders the result. Exported so the
// cockpit's picker sheet reports in exactly the same words as the typed command — two
// surfaces describing the same action differently is how a user ends up unsure whether
// the sheet did the same thing.
func BackendSwitchText(a *app.App, arg string) string {
	var (
		target string
		err    error
	)
	reset := strings.EqualFold(arg, app.BackendResetAlias)
	if reset {
		target, err = a.ResetBackendURL()
	} else {
		target, err = a.SetBackendURL(arg)
	}
	if err != nil {
		if target == "" {
			return "Cannot switch backend: " + err.Error() + "\n\nRun /backend with no argument to see the choices."
		}
		// The swap SUCCEEDED and only persisting failed. Reporting that as a plain
		// failure would be worse than useless — the user would re-run a command that
		// already worked, against an endpoint that already changed.
		return "Backend is now " + target + " — it answers from your next message.\n\n" +
			"But: " + err.Error()
	}
	// "from your next message", not "switched": a turn already streaming finishes on the
	// client it started on, so claiming the change is live would be false for a few more
	// seconds in exactly the case someone would notice.
	//
	// The two outcomes say opposite things about the FILE, and saying the wrong one is
	// worse than saying nothing: "remembered" after a reset would leave the user
	// believing a preference exists that was just deleted.
	out := "Backend is now " + target + " — it answers from your next message.\n\n"
	if reset {
		out += "The remembered choice is forgotten; new sessions use the default."
	} else {
		out += "Remembered for future sessions."
	}
	if a.SnapshotConfig().BackendURLPinnedByEnv {
		out += "\n\nNOTE: DAINTREE_BACKEND_URL (or --backend-url) is set and will OVERRIDE this\n" +
			"on the next launch. Unset it for the choice to stick."
	}
	return out
}

func permissionsText(a *app.App, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "Current tier: " + string(a.Tier()) + "\n" +
			"supervisor: read/local/ui · operator: +terminal/project/external · system: +git/system" +
			tierDivergenceNote(a)
	}
	t := domain.Tier(arg)
	if !t.IsValid() {
		return "Unknown tier '" + arg + "'. Use supervisor | operator | system."
	}
	a.SetTier(t)
	return "Tier set to " + string(t) + "." + tierDivergenceNote(a)
}

// tierDivergenceNote warns, in a leading-newline suffix, when the live tier differs from
// the boot tier (resolved from env/DEFAULTS): the change is session-only and reverts next
// launch, with the exact env var to set to make it stick. Empty when the tier matches the
// boot tier (or no boot tier was recorded) so the common case stays quiet.
func tierDivergenceNote(a *app.App) string {
	if a.InitialTier == "" || a.Tier() == a.InitialTier {
		return ""
	}
	return "\nThis applies to the current session only; next launch reverts to " +
		string(a.InitialTier) + ". Set DAINTREE_ASSISTANT_TIER=" + string(a.Tier()) +
		" to make it stick."
}

// memoryText powers /memory: bare/list shows the pinned-first memory store, and
// pin/unpin/forget curate by id. A pin-state change needs no runtime-context refresh —
// pinned memories live in the uncached turn footer now (issue #263), re-read every round,
// so the change surfaces on the assistant's next turn automatically. The classic REPL
// reaches this via its default delegation to HandleUICommand, so there is no separate
// REPL handler.
func memoryText(a *app.App, rest []string) string {
	const usage = "Usage: /memory list | pin <id> | unpin <id> | forget <id>"
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "list":
		if (sub == "" && len(rest) != 0) || (sub == "list" && len(rest) != 1) {
			return usage
		}
		limit := 50
		rows, err := a.Store.ListMemories(storage.MemoryListOptions{Limit: &limit})
		if err != nil {
			return "Failed to list memories: " + err.Error()
		}
		if len(rows) == 0 {
			return "No memories yet.\n\n" + usage
		}
		var lines []string
		for _, m := range rows {
			pin := "  "
			if m.PinnedAt != nil {
				pin = "📌"
			}
			lines = append(lines, fmt.Sprintf("%s %s [%s] %s", pin, m.ID, string(m.Source), oneLine(m.Content)))
		}
		lines = append(lines, "", usage)
		return strings.Join(lines, "\n")
	case "pin":
		if len(rest) != 2 {
			return "Usage: /memory pin <id>"
		}
		id := argAt(rest, 1)
		rec, err := a.Store.PinMemory(id, domain.NowMS())
		if err != nil {
			return "Failed to pin: " + err.Error()
		}
		if rec == nil {
			return "No such memory: " + id
		}
		// No RefreshRuntimeContext: pins live in the uncached turn footer now (issue #263),
		// re-read every round, so the pin surfaces on the assistant's next turn automatically.
		return "Pinned " + id + " — it surfaces in the assistant's context on its next turn."
	case "unpin":
		if len(rest) != 2 {
			return "Usage: /memory unpin <id>"
		}
		id := argAt(rest, 1)
		rec, err := a.Store.UnpinMemory(id, domain.NowMS())
		if err != nil {
			return "Failed to unpin: " + err.Error()
		}
		if rec == nil {
			return "No such memory: " + id
		}
		// No RefreshRuntimeContext: the footer re-reads pins every round, so the unpin drops
		// it from the assistant's context on its next turn.
		return "Unpinned " + id + "."
	case "forget":
		if len(rest) != 2 {
			return "Usage: /memory forget <id>"
		}
		id := argAt(rest, 1)
		found, err := a.Store.ForgetMemory(id, domain.NowMS())
		if err != nil {
			return "Failed to forget: " + err.Error()
		}
		if !found {
			return "No such memory: " + id
		}
		// A forgotten memory might have been pinned, but no RefreshRuntimeContext is needed:
		// the footer re-reads pins every round, so it leaves the assistant's context on its
		// next turn.
		return "Forgot " + id + "."
	default:
		return usage
	}
}

// compactRun checkpoints the conversation via the backend's checkpoint task then
// compacts, after distilling any durable facts from the transcript so they survive
// the discard. progress narrates each stage (two serial backend model calls — the
// caller shows the labels so the user is never staring at a silent cockpit).
func compactRun(ctx context.Context, a *app.App, progress func(string)) string {
	// The FULL flattened transcript, not a 12k tail: RunCheckpoint clamps to the task
	// contract's own (much larger) bound, and the checkpoint prompt's whole job is to
	// digest everything that is about to be discarded — a tail-only input silently
	// dropped every ID and decision older than the last few exchanges.
	full := transcriptString(a)
	before := a.Session.EstimateTokens()
	// User-facing stage labels say "compacting", never "checkpointing" — the checkpoint
	// is the mechanism, compaction is what the user asked for.
	progress("Compacting conversation…")
	// agent.BuildCheckpoint, NOT backend.RunCheckpoint directly: it runs the
	// ID-preservation validation pass over the FULL transcript, so an ID the model
	// dropped is re-injected — the same guarantee the auto-compact path has.
	cp, err := agent.BuildCheckpoint(ctx, a.Backend, full)
	if err != nil {
		return "Compaction failed: " + err.Error()
	}
	summary := renderCheckpointJSON(cp)
	// Capture the distill input (freshest TAIL of the history) BEFORE compaction
	// discards it.
	distillInput := capTail(full, domain.DistillTranscriptMaxRunes)
	progress("Applying compaction…")
	// CompactWithTranscript archives the full transcript as a durable artifact and
	// appends the escape-hatch breadcrumb to the note (same as auto-compact) — and
	// rejects a busy session BEFORE archiving, so a refused /compact never strands
	// an orphaned transcript artifact.
	if err := a.Session.CompactWithTranscript(summary, full); err != nil {
		return "Can't compact while a turn is in progress — cancel it (Esc) or wait for it to finish, then try again."
	}
	after := a.Session.EstimateTokens()
	// Only AFTER the history is actually discarded do we persist the distilled facts —
	// so a rejected compaction never writes premature memories (best-effort; the
	// distillation itself never affects the already-completed compaction).
	progress("Distilling memories…")
	saved := distillFromTranscript(ctx, a, distillInput)
	msg := fmt.Sprintf("Conversation compacted: ~%s → ~%s tokens (est).",
		formatTokenCount(before), formatTokenCount(after))
	if saved > 0 {
		msg += fmt.Sprintf(" Distilled %d %s.", saved, pluralMemory(saved))
	}
	return msg
}

// formatTokenCount renders an approximate token count for the compaction report:
// 412_345 → "412k", 950 → "950".
func formatTokenCount(n int) string {
	if n >= 1000 {
		return fmt.Sprintf("%dk", (n+500)/1000)
	}
	return fmt.Sprintf("%d", n)
}

// renderCheckpointJSON pretty-prints a checkpoint object as the compaction note body.
func renderCheckpointJSON(cp backend.CheckpointOutput) string {
	b, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}

// distillFromTranscript is the manual /compact counterpart of Session.distillCompact:
// it extracts durable facts from a soon-to-be-discarded transcript via the backend's
// memory_distill task and saves the novel ones as source="compact" memories. It
// lives here (not in the agent seam) because the command layer has direct *app.App
// access — no import cycle. Best-effort: any failure yields 0 and never affects compaction.
func distillFromTranscript(ctx context.Context, a *app.App, transcript string) (saved int) {
	defer func() { _ = recover() }()
	if a.Store == nil || a.Backend == nil || strings.TrimSpace(transcript) == "" {
		return 0
	}
	out, err := backend.RunMemoryDistill(ctx, a.Backend, backend.MemoryDistillInput{Transcript: transcript})
	if err != nil {
		return 0
	}
	for _, fact := range out.Facts {
		content := strings.TrimSpace(fact.Fact)
		if content == "" {
			continue
		}
		// Route each fact to its kind: semantic (a durable fact) vs episodic (an
		// instructive trajectory trace). Manual /compact has no live turn runID to
		// attribute, but episodic rows are still namespaced to the current session so
		// they can be scoped/expired later; semantic facts carry no sessionId.
		kind := domain.MemoryKindSemantic
		if fact.Kind == string(domain.MemoryKindEpisodic) {
			kind = domain.MemoryKindEpisodic
		}
		exists, exErr := a.Store.MemoryExists(content)
		if exErr != nil || exists {
			continue
		}
		now := domain.NowMS()
		rec := domain.MemoryRecord{
			Content:   content,
			Source:    domain.MemoryCompact,
			Kind:      kind,
			CreatedAt: now,
			UpdatedAt: now,
		}
		if kind == domain.MemoryKindEpisodic && a.SessionID != "" {
			sid := a.SessionID
			rec.SessionID = &sid
		}
		if _, insErr := a.Store.InsertMemory(rec); insErr == nil {
			saved++
		}
	}
	return saved
}

// pluralMemory renders the singular/plural noun for a memory count.
func pluralMemory(n int) string {
	if n == 1 {
		return "memory"
	}
	return "memories"
}

// argAt returns rest[i] or "" when out of range.
func argAt(rest []string, i int) string {
	if i >= 0 && i < len(rest) {
		return rest[i]
	}
	return ""
}

// oneLine flattens newlines and truncates to a compact single-line preview.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > 80 {
		return string(r[:77]) + "..."
	}
	return s
}

func reconnectRun(ctx context.Context, a *app.App) string {
	st := a.ReconnectMcp(ctx)
	if st.Connected {
		count := 0
		if st.ToolCount != nil {
			count = *st.ToolCount
		}
		return fmt.Sprintf("Reconnected (%s, %d tools).", st.Transport, count)
	}
	reason := st.Error
	if reason == "" {
		reason = "unknown error"
	}
	return "Still not connected — " + reason
}

func intPtr(n int) *int { return &n }

// transcriptString flattens the non-system history to "role: text" lines via the
// SHARED agent.FlattenTranscript (tool-call names + argument JSON folded in — the
// manual path used to emit a bare "[tool call]" and silently dropped every
// argument-only ID from the checkpoint's preservation pass). Uncapped: the checkpoint
// caller sends it whole (the task clamps server-side); the distillation caller keeps
// the freshest tail via capTail, where active handles and durable decisions are most
// likely to live.
func transcriptString(a *app.App) string {
	return agent.FlattenTranscript(a.Session.Messages())
}

// capTail keeps the last maxRunes runes (freshest content).
func capTail(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		return string(r[len(r)-maxRunes:])
	}
	return s
}
