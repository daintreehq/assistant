// Package agenttaskx is the no-file-edit escape hatch: agentTask.spawnForEdits
// (risk "project"). The CLI never edits files itself and never spawns agents via a
// raw agent.launch either — when a task needs code changes (mode "edit") OR a
// read-only investigation delegated to a visible agent (mode "explore"), it spawns
// a Daintree agent in a worktree (via the agent.launch MCP tool) and optionally
// attaches a supervising terminal watcher. The spawn is an idempotent saga: a
// write-ahead DB record before the side-effecting MCP call, deterministic
// idempotency key over the task identity, and a reconcile-via-terminal-list path
// so an ambiguous/crashed launch never double-spawns.
package agenttaskx

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/config"
	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCPCallResult is the Daintree MCP call envelope.
type MCPCallResult struct {
	Text              string `json:"text"`
	StructuredContent any    `json:"structuredContent,omitempty"`
	IsError           bool   `json:"isError"`
}

// MCPClient is the slice of the Daintree MCP transport this family reaches:
// agent.launch (the side-effecting spawn) and terminal.list (reconciliation).
type MCPClient interface {
	Connected() bool
	CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error)
}

// Store is the slice of the storage layer the spawn saga touches. The signatures
// mirror storage.Store exactly so the concrete *Store satisfies this by structural
// match (consumer-interface idiom — we do NOT import the storage subsystem).
//   - InsertWatcher persists the supervising watcher (returns the assigned id).
//   - ListWatchers reads watchers by status (superviseTerminal dedupes on "active").
//   - FindActiveAgentLaunch returns the newest non-terminal saga for a key.
//   - InsertAgentLaunch write-ahead-logs the saga BEFORE the MCP call.
//   - UpdateAgentLaunch applies an allowlisted patch (stage/terminalId/…).
//   - GetAgentLaunch reads one saga by id (agentTask.status), or (nil, nil).
//   - ListAgentLaunches reads the newest-first sagas (agentTask.list).
//   - InsertWorkflowRun records the spawned work in the durable ledger (so /workflows
//     surfaces it); UpdateWorkflowRun back-fills its links best-effort. Both mirror
//     *storage.Store exactly, so the concrete store satisfies this without an adapter.
type Store interface {
	InsertWatcher(rec domain.WatcherRecord) (domain.WatcherRecord, error)
	ListWatchers(status string) ([]domain.WatcherRecord, error)
	FindActiveAgentLaunch(idempotencyKey string) (*domain.AgentLaunchRecord, error)
	InsertAgentLaunch(rec domain.AgentLaunchRecord) (domain.AgentLaunchRecord, error)
	UpdateAgentLaunch(id string, patch map[string]any) error
	GetAgentLaunch(id string) (*domain.AgentLaunchRecord, error)
	ListAgentLaunches(limit int) ([]domain.AgentLaunchRecord, error)
	InsertWorkflowRun(rec domain.WorkflowRunRecord) (domain.WorkflowRunRecord, error)
	UpdateWorkflowRun(id string, patch map[string]any) error
}

// Deps wires the agentTask family. DaemonActive reports whether the scheduler is
// running (nil ⇒ assume active) — drives the lifecycle NOTE wording.
type Deps struct {
	MCP          MCPClient
	DB           Store
	Config       config.AppConfig
	DaemonActive func() bool
}

// daemonActive resolves the nil-safe DaemonActive (absent ⇒ assume active).
func (d Deps) daemonActive() bool {
	if d.DaemonActive == nil {
		return true
	}
	return d.DaemonActive()
}

// Tools returns the agentTask family: the spawn escape hatch, the superviseTerminal
// adopt tool (re-attach supervision to an already-running terminal without
// re-spawning), plus two RiskRead readers (status by id, list newest-first) so the
// model can inspect the spawn saga without re-launching anything.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newSpawnForEditsTool(deps),
		newSuperviseTerminalTool(deps),
		newStatusTool(deps),
		newListTool(deps),
	}
}
