package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/app"
	"github.com/daintreehq/daintree-assistant/internal/backend"
	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/storage"
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
	case "grants":
		return UICommandResult{Handled: true, Title: "Grants", Text: grantsText(a)}
	case "workflows":
		return UICommandResult{Handled: true, Title: "Workflows", Text: workflowsText(a, arg)}
	case "launches":
		return UICommandResult{Handled: true, Title: "Launches", Text: launchesText(a)}
	case "audit":
		return UICommandResult{Handled: true, SwitchPanel: PanelAudit, Title: "Audit", Text: auditText(a, rest, false)}
	case "explain":
		return UICommandResult{Handled: true, Title: "Explain", Text: explainText(a, arg)}
	case "models":
		return UICommandResult{Handled: true, Title: "Models", Text: modelsText(a)}
	case "permissions":
		return UICommandResult{Handled: true, Title: "Permissions", Text: permissionsText(a, arg)}
	case "approvals":
		// The session approval allow-list lives on the cockpit Model (the UI intercepts
		// /approvals before this handler — see ui.onSubmit). The REPL has no interactive
		// per-call approvals, so this surface only explains where the command applies.
		return UICommandResult{Handled: true, Title: "Approvals", Text: "Session tool approvals are managed in the cockpit. Press A (bounded) or F (forever this session) on an approval prompt; run /approvals there to list or clear them."}
	case "skills":
		return UICommandResult{Handled: true, Title: "Skills", Text: skillsText(ctx, a, rest)}
	case "memory":
		return UICommandResult{Handled: true, Title: "Memory", Text: memoryText(a, rest)}
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
		// /clear is a full reset: tear down every live watcher AND resolve every open
		// inbox event (same clean slate the next session boundary produces) so no
		// prior supervision or attention item carries over. Best-effort — neither must
		// block the conversation clear.
		_, _ = a.ClearWatchers()
		_, _ = a.ClearInbox()
		return UICommandResult{Handled: true, ClearTranscript: true, Title: "Clear", Text: "Conversation cleared — watchers and inbox cleared — starting fresh."}
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
		"Keys",
		`  Enter           send  ·  Shift/Alt+Enter or trailing \ then Enter → newline`,
		"  ↑ / ↓           recall prompt history",
		"  /               command palette  ·  Tab complete  ·  ↑/↓ navigate",
		"  Esc             clear the input  ·  while busy, retract the last queued turn (else cancel)",
		"  Ctrl+C          cancel the running turn  ·  press again to exit",
		"  Ctrl+D          exit at an empty prompt",
		"  Ctrl+O          toggle the operations deck",
		"  Ctrl+X          toggle raw tool detail",
		"  Ctrl+L          redraw the screen (recover a corrupted footer)",
		"  ?               show this help (at an empty prompt)",
		"  ↑/↓ PgUp/PgDn   scroll the operations deck / help when it overflows",
		"  Editing         Ctrl+A/E home/end · Ctrl+W/U/K kill · Ctrl+Y yank · Alt+B/F word · Alt+D kill-word",
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
	// Normalize case so "/workflows Active" matches the stored lowercase status (a
	// literal SQL WHERE) instead of silently returning "(none)".
	status := strings.ToLower(strings.TrimSpace(arg))
	switch status {
	case "":
		status = string(domain.WorkflowActive)
	case "all":
		status = ""
	}
	runs, err := a.Store.ListWorkflowRuns(status)
	if err != nil {
		return "Failed to list workflows: " + err.Error()
	}
	if len(runs) == 0 {
		return "(none)"
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
	return strings.Join(rows, "\n")
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
	// Model routing is owned by the Daintree backend now; the CLI no longer selects a
	// model tier. Surface the backend endpoint + its public model id.
	return "Model routing is owned by the Daintree backend.\n" +
		padRight("backend", 8) + ": " + a.Backend.BaseURL() + "\n" +
		padRight("model", 8) + ": daintree-assistant"
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

// skillsText powers /skills. Skill selection is now SERVER-OWNED: the Daintree
// backend's selector picks and injects the right runbook(s) per turn, and surfaces
// the active set in each response's skills metadata. There is no local skill
// catalog to browse or load, so this command is informational only.
func skillsText(_ context.Context, _ *app.App, _ []string) string {
	return "Skills are managed by the Daintree backend.\n" +
		"The backend's selector picks and injects the right runbook(s) automatically each turn — " +
		"there is no local skill catalog to browse, find, or load. Newly-loaded skills are surfaced " +
		"in the conversation as they are applied."
}

// memoryText powers /memory: bare/list shows the pinned-first memory store, and
// pin/unpin/forget curate by id. A pin-state change needs no runtime-context refresh —
// pinned memories live in the uncached turn footer now (issue #263), re-read every round,
// so the change surfaces on the assistant's next turn automatically. Mirrors skillsText's
// subcommand shape. The classic REPL reaches this via its default delegation to
// HandleUICommand, so there is no separate REPL handler.
func memoryText(a *app.App, rest []string) string {
	const usage = "Usage: /memory list | pin <id> | unpin <id> | forget <id>"
	sub := ""
	if len(rest) > 0 {
		sub = rest[0]
	}
	switch sub {
	case "", "list":
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
		id := argAt(rest, 1)
		if id == "" {
			return "Usage: /memory pin <id>"
		}
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
		id := argAt(rest, 1)
		if id == "" {
			return "Usage: /memory unpin <id>"
		}
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
		id := argAt(rest, 1)
		if id == "" {
			return "Usage: /memory forget <id>"
		}
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

// compactRun checkpoints the conversation via the backend's checkpoint.v1 task then
// compacts, after distilling any durable facts from the transcript so they survive
// the discard.
func compactRun(ctx context.Context, a *app.App) string {
	full := transcriptString(a)
	cp, err := backend.RunCheckpoint(ctx, a.Backend, backend.CheckpointInput{Transcript: capTail(full, 12000)})
	if err != nil {
		return "Compaction failed: " + err.Error()
	}
	summary := renderCheckpointJSON(cp)
	// Capture the distill input (freshest TAIL of the history) BEFORE compaction
	// discards it.
	distillInput := capTail(full, domain.DistillTranscriptMaxRunes)
	if err := a.Session.Compact(summary); err != nil {
		return "Can't compact while a turn is in progress — cancel it (Esc) or wait for it to finish, then try again."
	}
	// Only AFTER the history is actually discarded do we persist the distilled facts —
	// so a rejected compaction never writes premature memories (best-effort; the
	// distillation itself never affects the already-completed compaction).
	saved := distillFromTranscript(ctx, a, distillInput)
	msg := "Conversation compacted."
	if saved > 0 {
		msg += fmt.Sprintf(" Distilled %d %s.", saved, pluralMemory(saved))
	}
	return msg
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
// memory_distill.v1 task and saves the novel ones as source="compact" memories. It
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

// transcriptString flattens the non-system history to "role: text" lines (empty text
// → "[tool call]"), newline-joined and uncapped. Capping is the caller's choice — both
// the summary and distillation callers use capTail, keeping the freshest content where
// active handles (IDs, branches, grants) and durable decisions are most likely to live;
// the head is the part normal compaction would have summarized away anyway.
func transcriptString(a *app.App) string {
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
	return b.String()
}

// capTail keeps the last maxRunes runes (freshest content).
func capTail(s string, maxRunes int) string {
	if r := []rune(s); len(r) > maxRunes {
		return string(r[len(r)-maxRunes:])
	}
	return s
}
