// Package mcpwrap holds the typed Daintree-MCP wrapper tools: recipe, worktree,
// forge reads, git.getProjectPulse, focus, and the workflow MCP passthroughs. Each
// forwards a validated argument set to a same-named (or remapped) Daintree MCP
// action via a shared passthrough forwarder. Unlike daintree.call (the raw
// escape hatch), these carry exact arg JSON-schemas + risk classes so the model
// can't drop a required argument.
package mcpwrap

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCPClient is the consumer-defined slice of the Daintree MCP transport these
// wrappers use. CallTool forwards a remapped action; Connected gates every call.
// It is intentionally identical in shape to tools.MCPClient so the same concrete
// client satisfies both, but declared locally to keep this package independently
// compilable.
type MCPClient interface {
	CallTool(ctx context.Context, name string, args map[string]any) (tools.MCPCallResult, error)
	Connected() bool
}

// WatcherStore is the slice of storage the supervisor-watcher attach path needs
// (workflow.startWorkOnIssue). InsertWatcher persists a supervisor watcher and
// returns the assigned record (its id back-links the workflow ledger row);
// ListWatchers lets us dedup an already-attached supervisor on a terminal.
type WatcherStore interface {
	InsertWatcher(ctx context.Context, rec domain.WatcherRecord) (domain.WatcherRecord, error)
	ListWatchers(ctx context.Context, status string) ([]domain.WatcherRecord, error)
}

// WorkflowStore is the slice of storage the workflow ledger touches when
// workflow.startWorkOnIssue spawns a terminal: InsertWorkflowRun records the work
// (returns the new id); UpdateWorkflowRun back-fills its links (e.g. watcherIdsJson)
// via an allowlisted patch. Optional — a nil WorkflowStore skips the ledger entirely.
type WorkflowStore interface {
	InsertWorkflowRun(ctx context.Context, rec domain.WorkflowRunRecord) (string, error)
	UpdateWorkflowRun(ctx context.Context, id string, patch map[string]any) error
}

// Deps is the dependency set the mcpwrap family is built against.
type Deps struct {
	// Store is optional; only the workflow.startWorkOnIssue supervisor-watcher
	// attach needs it. A nil Store degrades that path to "passthrough only".
	Store WatcherStore
	// WorkflowStore is optional; it records spawned work in the durable ledger so
	// /workflows surfaces it. A nil WorkflowStore degrades to "watcher only".
	WorkflowStore WorkflowStore
}
