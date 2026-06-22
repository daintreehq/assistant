package ui

import (
	"sort"

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
	Agents   []AgentRow // built rows (one per watcher merged with its preview)
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

// BuildAgentRows merges watchers with terminal previews into agent rows, sorted by
// urgency then recency. It is pure and Bubble-Tea-free so it
// stays unit-testable. Previews are matched by terminal id parsed from targetsJson;
// the row id prefers the terminal id.
func BuildAgentRows(watchers []domain.WatcherRecord) []AgentRow {
	rows := make([]AgentRow, 0, len(watchers))
	for _, w := range watchers {
		kind := domain.EpistemicKind("")
		if w.LastEpistemicKind != nil {
			kind = *w.LastEpistemicKind
		} else if w.LastClassification != nil {
			// Legacy rows lacking LastEpistemicKind: derive provenance from the
			// classification. usedModel=false → the deterministic-signal default.
			kind = domain.ClassificationEpistemicKind(domain.WatcherClassification(*w.LastClassification), false)
		}
		badge, prio := watcherBadge(w)
		rows = append(rows, AgentRow{
			ID:             w.ID,
			Title:          w.Title,
			Goal:           w.Goal,
			Badge:          badge,
			Priority:       prio,
			EpistemicKind:  kind,
			StartedAt:      w.CreatedAt,
			NeedsAttention: prio <= 1,
		})
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
