package ui

import (
	"encoding/json"
	"sort"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/daemon"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Dashboard is the operations snapshot the deck + status rollup render from. It is
// built off the loop (in a tea.Cmd) so Update stays cheap, then handed in via
// DashboardSnapshotMsg. It holds the inbox, timers, audit, and agents state.
type Dashboard struct {
	Inbox    []domain.QueueEvent  // severityAtLeast=attention, urgency-sorted
	Timers   []domain.TimerRecord // scheduled, soonest first
	Audit    []domain.AuditRecord // most-recent first
	Watchers []domain.WatcherRecord
	// Launches is the durable spawn roster (newest-first). It is LEFT-JOINed onto the
	// live watchers in BuildAgentRows so an agent whose session-scoped watcher has been
	// cancelled/completed stays on the deck from its stored saga instead of vanishing.
	Launches []domain.AgentLaunchRecord
	Agents   []AgentRow // built rows (watchers ⟕ launches, each merged with its preview)
}

// AgentRow is one supervised agent: one watcher merged with its watched terminal's
// preview. The user thinks "one agent doing one job", not a
// watcher and a terminal separately.
type AgentRow struct {
	ID             string
	Title          string
	Goal           string
	Badge          string
	Priority       int // lower = more urgent (needs-input first)
	EpistemicKind  domain.EpistemicKind
	AgentState     string
	Preview        string
	StartedAt      int64
	NeedsAttention bool
}

// BuildAgentRows merges live watchers, terminal previews, and the durable spawn
// roster into one ordered set of agent rows, sorted by urgency then recency. It is
// pure and Bubble-Tea-free so it stays unit-testable.
//
// A watcher and its spawn saga are the SAME agent to the user, so launches are
// LEFT-JOINed onto the watchers on the launch's WatcherID: a live watcher owns the
// row (badge from its classification); a launch whose watcher is gone
// (cancelled/completed) or never attached (a spawn that failed early) is synthesized
// from its saga stage so the agent does not vanish from the deck. Previews are matched
// to either by terminal id and fold the live AgentState + a one-line tail into the row.
func BuildAgentRows(watchers []domain.WatcherRecord, previews []daemon.TerminalPreview, launches []domain.AgentLaunchRecord) []AgentRow {
	previewByTerminal := make(map[string]daemon.TerminalPreview, len(previews))
	for _, p := range previews {
		if p.TerminalID != "" {
			previewByTerminal[p.TerminalID] = p
		}
	}

	rows := make([]AgentRow, 0, len(watchers)+len(launches))
	live := make(map[string]bool, len(watchers))
	for _, w := range watchers {
		live[w.ID] = true
		kind := domain.EpistemicKind("")
		if w.LastEpistemicKind != nil {
			kind = *w.LastEpistemicKind
		} else if w.LastClassification != nil {
			// Legacy rows lacking LastEpistemicKind: derive provenance from the
			// classification. usedModel=false → the deterministic-signal default.
			kind = domain.ClassificationEpistemicKind(domain.WatcherClassification(*w.LastClassification), false)
		}
		badge, prio := watcherBadge(w)
		row := AgentRow{
			ID:             w.ID,
			Title:          w.Title,
			Goal:           w.Goal,
			Badge:          badge,
			Priority:       prio,
			EpistemicKind:  kind,
			StartedAt:      w.CreatedAt,
			NeedsAttention: prio <= 1,
		}
		applyPreview(&row, previewByTerminal[firstTerminalID(w.TargetsJson)])
		rows = append(rows, row)
	}

	// LEFT-JOIN the roster: a launch whose watcher is still live is already on the
	// deck as that watcher row — skip it (no duplicate). A launch with no live watcher
	// is synthesized from its saga stage so the agent stays visible.
	for _, l := range launches {
		if l.WatcherID != nil && live[*l.WatcherID] {
			continue
		}
		terminalID := ""
		if l.TerminalID != nil {
			terminalID = *l.TerminalID
		}
		id := l.ID
		if terminalID != "" {
			id = terminalID
		}
		badge, prio := launchBadge(l.Stage)
		row := AgentRow{
			ID:             id,
			Title:          firstNonEmpty(l.Title, l.Name),
			Badge:          badge,
			Priority:       prio,
			StartedAt:      l.CreatedAt,
			NeedsAttention: prio <= 1,
		}
		applyPreview(&row, previewByTerminal[terminalID])
		rows = append(rows, row)
	}

	// Urgency (lower priority first), ties broken by most-recent StartedAt.
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		return rows[i].StartedAt > rows[j].StartedAt
	})
	return rows
}

// applyPreview folds a matched terminal preview's live state into a row. A zero-value
// preview (no card for that terminal this tick) is a no-op, leaving the row's state
// blank. The tail is collapsed to a single line and shown to the HUMAN only — it is
// NEVER fed to the main model.
func applyPreview(row *AgentRow, p daemon.TerminalPreview) {
	if p.AgentState != "" {
		row.AgentState = p.AgentState
	}
	if snippet := previewSnippet(p.Tail); snippet != "" {
		row.Preview = snippet
	}
}

// previewSnippet reduces a multi-line preview tail to its last non-blank line for the
// single-line agent row (the full tail lives in a human-only card elsewhere). Embedded
// newlines would otherwise corrupt the deck layout.
func previewSnippet(tail string) string {
	lines := strings.Split(tail, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return ""
}

// launchBadge maps a spawn-saga stage to a short badge + priority for a launch-only
// row (its watcher is gone or never attached). Mirrors watcherBadge's priority scale
// (lower = more urgent): a failed spawn needs attention, an ambiguous one wants
// review, an in-flight or confirmed spawn is just working.
func launchBadge(stage domain.AgentLaunchStage) (string, int) {
	switch stage {
	case domain.LaunchFailed:
		return "FAILED", 1
	case domain.LaunchAmbiguous:
		return "REVIEW", 2
	case domain.LaunchConfirmed:
		return "WORKING", 3
	default: // launch_requested, agent_started, terminal_bound, watcher_attached
		return "STARTING", 3
	}
}

// terminalIDs parses a watcher's targetsJson (a JSON string[]). Malformed JSON
// degrades to nil — a snapshot build must never break the UI.
func terminalIDs(targetsJson string) []string {
	if targetsJson == "" {
		return nil
	}
	var ids []string
	if err := json.Unmarshal([]byte(targetsJson), &ids); err != nil {
		return nil
	}
	return ids
}

// firstTerminalID returns the first non-empty terminal id a watcher targets, used to
// match its preview card.
func firstTerminalID(targetsJson string) string {
	for _, id := range terminalIDs(targetsJson) {
		if id != "" {
			return id
		}
	}
	return ""
}

// watcherBadge maps a watcher's last classification to a short badge label + a
// priority (lower = more urgent). An unknown/absent classification is mid-priority.
func watcherBadge(w domain.WatcherRecord) (string, int) {
	if w.LastClassification == nil {
		return "WORKING", 3
	}
	switch domain.WatcherClassification(*w.LastClassification) {
	case domain.ClassWaitingForInput, domain.ClassPermissionPrompt:
		return "NEEDS INPUT", 0
	case domain.ClassMergeConflict, domain.ClassRateLimited:
		return "BLOCKED", 1
	case domain.ClassCommandFailed, domain.ClassTestsFailed:
		return "FAILED", 1
	case domain.ClassCompletedSuccess, domain.ClassTestsPassed:
		return "DONE", 4
	case domain.ClassCompletedUnverified, domain.ClassCompletedUnknown, domain.ClassTerminalExited:
		return "REVIEW", 2
	case domain.ClassStillWorking, domain.ClassNoChange:
		return "WORKING", 3
	default:
		return "WORKING", 3
	}
}

// topSeverity returns the worst severity across the inbox (for the attention-count
// tint), or info when empty.
func (d Dashboard) topSeverity() domain.Severity {
	worst := domain.SeverityInfo
	for _, e := range d.Inbox {
		if domain.RankOf(e.Severity) > domain.RankOf(worst) {
			worst = e.Severity
		}
	}
	return worst
}
