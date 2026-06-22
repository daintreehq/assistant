package commands

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/models"
)

// UICommandResult is the structured return of the cockpit slash handler
// (commandData.ts UiCommandResult). The cockpit renders a card / switches a panel;
// nothing is printed.
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
// ctx carries cancellation for the model-backed commands (compact, skills find).
func HandleUICommand(ctx context.Context, line string, a *app.App) UICommandResult {
	cmd, arg, rest := parseCommand(line)
	if cmd == "" {
		return UICommandResult{Handled: false}
	}
	switch canonical(cmd) {
	case "quit":
		return UICommandResult{Handled: true, Quit: true}
	case "help":
		return UICommandResult{Handled: true, SwitchPanel: PanelHelp, Title: "Help", Text: HelpTextUI()}
	case "status":
		return UICommandResult{Handled: true, Title: "Status", Text: statusText(a)}
	case "inbox":
		title, text := inboxView(ctx, a, arg)
		return UICommandResult{Handled: true, SwitchPanel: PanelInbox, Title: title, Text: text}
	case "tools":
		title, text := toolsView(a, arg)
		return UICommandResult{Handled: true, Title: title, Text: text}
	case "timers":
		return UICommandResult{Handled: true, SwitchPanel: PanelTimers, Title: "Timers", Text: timersText(a)}
	case "watchers":
		return UICommandResult{Handled: true, SwitchPanel: PanelWatchers, Title: "Watchers", Text: watchersText(a)}
	case "audit":
		return UICommandResult{Handled: true, SwitchPanel: PanelAudit, Title: "Audit", Text: auditText(a, rest, false)}
	case "explain":
		return UICommandResult{Handled: true, Title: "Explain", Text: explainText(a, arg)}
	case "models":
		return UICommandResult{Handled: true, Title: "Models", Text: modelsText(a)}
	case "permissions":
		return UICommandResult{Handled: true, Title: "Permissions", Text: permissionsText(a, arg)}
	case "skills":
		return UICommandResult{Handled: true, Title: "Skills", Text: skillsText(ctx, a, rest)}
	case "compact":
		return UICommandResult{Handled: true, Title: "Compact", Text: compactRun(ctx, a)}
	case "clear":
		// #5: only wipe the UI transcript AFTER Session.Clear() actually succeeds. A
		// mid-turn clear returns ErrTurnInProgress (a clear would corrupt the streaming
		// snapshot while the agent keeps emitting), so reject it with a note instead of
		// desyncing the UI from a still-live session.
		if err := a.Session.Clear(); err != nil {
			return UICommandResult{Handled: true, Title: "Clear", Text: "Can't clear while a turn is in progress — cancel it (Esc) or wait for it to finish, then try again."}
		}
		return UICommandResult{Handled: true, ClearTranscript: true, Title: "Clear", Text: "Conversation cleared — starting fresh."}
	case "doctor":
		return UICommandResult{Handled: true, Title: "Doctor", Text: FormatDoctor(RunDoctor(ctx, a))}
	case "reconnect":
		return UICommandResult{Handled: true, Title: "Reconnect", Text: reconnectRun(ctx, a)}
	default:
		return UICommandResult{Handled: true, Title: "Unknown command", Text: "Unknown command /" + cmd + ". Try /help."}
	}
}

// HelpTextUI is the cockpit help blob (commandRegistry §6.6).
func HelpTextUI() string {
	lines := append([]string{}, HelpLines()...)
	lines = append(lines, "", "Keys: ? help · ^O toggle ops deck · ^C exit. Anything else goes to the assistant.")
	return strings.Join(lines, "\n")
}

// HelpTextREPL is the REPL help blob (commandRegistry §6.6).
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
	mcpLine := "Daintree MCP: " + mcpStatusLine(mcpStatusOf(a))
	desc := config.DescribeConfig(a.Config)
	keys := make([]string, 0, len(desc))
	for k := range desc {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := []string{mcpLine}
	for _, k := range keys {
		lines = append(lines, "  "+padRight(k, 20)+desc[k])
	}
	return strings.Join(lines, "\n")
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

func inboxView(ctx context.Context, a *app.App, arg string) (string, string) {
	var sev *domain.Severity
	if s, ok := inboxSeverities[strings.TrimSpace(arg)]; ok {
		sev = &s
	}
	maxItems := 30
	events, err := a.Queue.Digest(ctx, domain.QueueDigestOptions{SeverityAtLeast: sev, MaxItems: &maxItems})
	if err != nil {
		return "Inbox", "Failed to read inbox: " + err.Error()
	}
	return fmt.Sprintf("Inbox (%d)", len(events)), a.Queue.Format(events)
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
		rows = append(rows, padRight(t.Name, 26)+"["+string(t.Risk)+"] "+t.Description)
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

// auditText renders the audit view. rest[0]=="export" triggers a serialized dump.
func auditText(a *app.App, rest []string, _ bool) string {
	if len(rest) > 0 && rest[0] == "export" {
		parsed := ParseAuditExportArgs(rest[1:])
		if parsed.Error != "" {
			return parsed.Error
		}
		rows, err := a.Store.QueryAudit(parsed.Filters)
		if err != nil {
			return "Audit export failed: " + err.Error()
		}
		return SerializeAudit(rows, parsed.Format)
	}
	n := 15
	if len(rest) > 0 {
		if v := atoiSafe(rest[0]); v > 0 {
			n = v
		}
	}
	rows, err := a.Store.ListAudit(n)
	if err != nil {
		return "Failed to read audit: " + err.Error()
	}
	if len(rows) == 0 {
		return "(no audit entries)"
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
	return strings.Join(lines, "\n")
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
	d := a.Router.Describe()
	order := []string{"large", "medium", "small"}
	var lines []string
	for _, k := range order {
		lines = append(lines, padRight(k, 7)+": "+d[k])
	}
	return strings.Join(lines, "\n")
}

func permissionsText(a *app.App, arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "Current tier: " + string(a.Config.Tier) + "\n" +
			"supervisor: read/local/ui · operator: +terminal/project/external · system: +git/system"
	}
	t := domain.Tier(arg)
	if !t.IsValid() {
		return "Unknown tier '" + arg + "'. Use supervisor | operator | system."
	}
	a.Config.Tier = t
	a.Session.RefreshRuntimeContext(a.PromptContext())
	return "Tier set to " + string(t) + "."
}

// skillsLoadCap is the per-load skill cap (commandRegistry §6.4).
const skillsLoadCap = 3

func skillsText(ctx context.Context, a *app.App, rest []string) string {
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "":
		all := a.Skills.List()
		var lines []string
		for _, s := range all {
			lines = append(lines, fmt.Sprintf("%s [%s] %s — %s", s.ID, string(s.Risk), s.Title, s.Summary))
		}
		lines = append(lines, "", "Usage: /skills loaded | find <query> | load <id…> | clear")
		return strings.Join(lines, "\n")
	case "loaded":
		return describeLoaded(a)
	case "clear":
		a.Session.SetSkills(nil)
		return "Cleared loaded skills."
	case "load":
		ids := rest[1:]
		var known []string
		for _, id := range ids {
			if a.Skills.Has(id) {
				known = append(known, id)
			}
		}
		if len(known) == 0 {
			return "No known skill ids in: " + strings.Join(ids, " ") + " — loaded set unchanged."
		}
		if len(known) > skillsLoadCap {
			known = known[:skillsLoadCap]
		}
		a.Session.SetSkills(known)
		return describeLoaded(a)
	case "find":
		query := strings.Join(rest[1:], " ")
		if strings.TrimSpace(query) == "" {
			return "Usage: /skills find <query>"
		}
		res := a.Session.FindSkills(ctx, query)
		if !res.Ok {
			return "Skill selector unavailable: " + res.Reason
		}
		if res.Matched {
			ids := make([]string, 0, len(res.Selected))
			for _, s := range res.Selected {
				ids = append(ids, s.ID)
			}
			return "Loaded: " + strings.Join(ids, ", ")
		}
		return "No skill matched."
	default:
		return "Usage: /skills loaded | find <query> | load <id…> | clear"
	}
}

func describeLoaded(a *app.App) string {
	ids := a.Session.ActiveSkillIDs()
	if len(ids) == 0 {
		return "No skills loaded."
	}
	var lines []string
	for _, s := range a.Skills.GetMany(ids) {
		lines = append(lines, fmt.Sprintf("%s — %s", s.ID, s.Title))
	}
	return strings.Join(lines, "\n")
}

// compactRun summarizes the conversation with the small model then compacts.
func compactRun(ctx context.Context, a *app.App) string {
	transcript := buildCompactionTranscript(a, 12000)
	res, err := a.Router.Chat(ctx, domain.ModelSmall, models.ChatOptions{
		Messages: []models.ChatMessage{
			models.TextMessage("system", "Summarize this assistant session into a tight brief: goals, decisions, open watchers/timers, and next steps. <= 200 words."),
			models.TextMessage("user", transcript),
		},
		MaxTokens: intPtr(400),
	})
	if err != nil {
		return "Compaction failed: " + err.Error()
	}
	a.Session.Compact(res.Content)
	return "Conversation compacted."
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

// buildCompactionTranscript flattens the non-system history to "role: text"
// (empty text → "[tool call]"), joined by newlines and sliced to maxChars
// (commandRegistry §6.4 compact).
func buildCompactionTranscript(a *app.App, maxChars int) string {
	var b strings.Builder
	for _, m := range a.Session.Messages() {
		if m.Role == "system" {
			continue
		}
		text := m.ContentToText()
		if strings.TrimSpace(text) == "" {
			text = "[tool call]"
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}
	s := b.String()
	rs := []rune(s)
	if len(rs) > maxChars {
		return string(rs[:maxChars])
	}
	return s
}
