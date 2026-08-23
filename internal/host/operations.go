package host

import "github.com/daintreehq/assistant/internal/redact"

// operations.go carries the operations deck — what the assistant is watching, running
// and has recently done — to an embedded host.
//
// The cockpit built this from the project store on demand and drew it as a seven-part
// deck. A protocol-only consumer had no way to see any of it: the panel could say it
// "keeps watch on the runs" while being structurally unable to show what it was
// watching. These are the same rows, as data rather than as drawn lines, so a host
// decides presentation and the engine stays the single source of the facts.
//
// Sections mirror the cockpit's, in its order: NOW, NEEDS ATTENTION, WORKFLOWS,
// AGENTS, ASYNC, SCHEDULED, RECENT.

// OperationsSnapshot is one reading of the deck.
type OperationsSnapshot struct {
	// Inbox is attention-or-higher, urgency-sorted — the NEEDS ATTENTION section.
	Inbox []InboxRow
	// Workflows are the open execution graphs, newest first.
	Workflows []WorkflowRow
	// Agents are supervised agents: a watcher merged with its terminal's preview. The
	// user thinks "one agent doing one job", not a watcher and a terminal separately.
	Agents []AgentRow
	// Async is the live async-futures ledger, oldest first.
	Async []AsyncRow
	// Timers are scheduled operations, soonest first.
	Timers []TimerRow
	// Audit is the most recent tool calls, newest first.
	Audit []AuditRow
}

// InboxRow is one item needing attention.
type InboxRow struct {
	ID       string
	Severity string
	Source   string
	Summary  string
	At       int64
}

// WorkflowRow is one open execution graph, reduced to the deck's two-line view.
type WorkflowRow struct {
	ID     string
	Goal   string
	Status string
	// Progress reads like "3/5 done · current: Run tests".
	Progress string
	// Next is the next-action label, empty when there is none.
	Next    string
	Blocked bool
}

// AgentRow is one supervised agent.
type AgentRow struct {
	ID    string
	Title string
	Goal  string
	Badge string
	// AgentState is the passive read of what the agent is doing.
	AgentState string
	// Preview is the tail of its terminal, so a supervisor can see it working.
	Preview        string
	StartedAt      int64
	NeedsAttention bool
}

// AsyncRow is one accepted-but-still-running async operation.
type AsyncRow struct {
	ID        string
	Title     string
	Tool      string
	StartedAt int64
}

// TimerRow is one scheduled operation.
type TimerRow struct {
	ID    string
	Label string
	DueAt int64
}

// AuditRow is one recent tool call.
type AuditRow struct {
	Tool       string
	Outcome    string
	DurationMs int64
	At         int64
}

// EvOperations — operations:snapshot. Answers an `operations` command.
//
// Sent on request rather than streamed: the cockpit rebuilt its deck when the user
// opened it, and pushing every store change to a host that may not be showing the deck
// would be a great deal of traffic for a view nobody is looking at.
type EvOperations struct {
	Snapshot OperationsSnapshot
}

func (e EvOperations) encode(sid string, seq uint64) ([]byte, error) {
	s := e.Snapshot
	inbox := make([]map[string]any, 0, len(s.Inbox))
	for _, r := range s.Inbox {
		inbox = append(inbox, map[string]any{
			"id": r.ID, "severity": r.Severity, "source": r.Source,
			"summary": redact.String(r.Summary), "at": r.At,
		})
	}
	workflows := make([]map[string]any, 0, len(s.Workflows))
	for _, r := range s.Workflows {
		workflows = append(workflows, map[string]any{
			"id": r.ID, "goal": redact.String(r.Goal), "status": r.Status,
			"progress": r.Progress, "next": r.Next, "blocked": r.Blocked,
		})
	}
	agents := make([]map[string]any, 0, len(s.Agents))
	for _, r := range s.Agents {
		agents = append(agents, map[string]any{
			"id": r.ID, "title": redact.String(r.Title), "goal": redact.String(r.Goal),
			"badge": r.Badge, "agentState": r.AgentState,
			// The terminal tail is the most likely place for a token to appear, since
			// it is whatever the agent last printed.
			"preview": redact.String(r.Preview), "startedAt": r.StartedAt,
			"needsAttention": r.NeedsAttention,
		})
	}
	async := make([]map[string]any, 0, len(s.Async))
	for _, r := range s.Async {
		async = append(async, map[string]any{
			"id": r.ID, "title": redact.String(r.Title), "tool": r.Tool, "startedAt": r.StartedAt,
		})
	}
	timers := make([]map[string]any, 0, len(s.Timers))
	for _, r := range s.Timers {
		timers = append(timers, map[string]any{
			"id": r.ID, "label": redact.String(r.Label), "dueAt": r.DueAt,
		})
	}
	audit := make([]map[string]any, 0, len(s.Audit))
	for _, r := range s.Audit {
		audit = append(audit, map[string]any{
			"tool": r.Tool, "outcome": r.Outcome, "durationMs": r.DurationMs, "at": r.At,
		})
	}
	return marshalEvent("operations:snapshot", sid, seq, map[string]any{
		"inbox":     inbox,
		"workflows": workflows,
		"agents":    agents,
		"async":     async,
		"timers":    timers,
		"audit":     audit,
	})
}
