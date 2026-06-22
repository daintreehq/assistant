// Package mcpwrap holds the typed Daintree-MCP wrapper tools: recipe, worktree,
// forge reads, git-snapshot, focus, and the workflow MCP passthroughs. Each
// forwards a validated argument set to a same-named (or remapped) Daintree MCP
// action via a shared passthrough forwarder. Unlike daintree.call (the raw
// escape hatch), these carry exact arg JSON-schemas + risk classes so the model
// can't drop a required argument.
//
// Spec: docs/port/tools-families.md §4.14 (mcpTools.ts wrappers).
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
// (workflow.startWorkOnIssue). insertWatcher persists a supervisor watcher;
// listActiveWatchers lets us dedup an already-attached supervisor on a terminal.
type WatcherStore interface {
	InsertWatcher(ctx context.Context, rec domain.WatcherRecord) error
	ListWatchers(ctx context.Context, status string) ([]domain.WatcherRecord, error)
}

// Deps is the dependency set the mcpwrap family is built against.
type Deps struct {
	// Store is optional; only the workflow.startWorkOnIssue supervisor-watcher
	// attach needs it. A nil Store degrades that path to "passthrough only".
	Store WatcherStore
}
