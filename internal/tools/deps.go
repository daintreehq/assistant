package tools

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Cross-subsystem dependencies are declared as SMALL consumer-defined interfaces
// here: exactly the methods the dispatch pipeline / exemplar tools call on other
// subsystems. We deliberately do NOT import the concrete storage/mcp/queue/models
// packages (that would create import cycles and couple the build) — the concrete
// types will satisfy these by structural match. Spec: tools-core.md §8.3.

// Store is the slice of the storage layer the registry touches: the audit insert
// and the atomic grant consume. InsertAudit returns the new row id (mutated onto
// the ToolResult as AuditID). ConsumeGrant returns the consumed grant (non-nil)
// when a matching, live grant authorized the (actor, tool, risk); nil otherwise.
// Spec: tools-core.md §6.2, §9.
type Store interface {
	// InsertAudit persists one audit row and returns its id. Best-effort at the
	// call site (wrapped so it can never break a tool call).
	InsertAudit(ctx context.Context, rec domain.AuditRecord) (string, error)

	// ConsumeGrant atomically decrements and returns the first live grant for the
	// non-interactive actor that authorizes (toolName OR riskClass). The atomicity
	// (TTL / revocation / use-exhaustion races) lives in the WHERE clause of the
	// storage layer. Returns (nil, nil) when no grant matches.
	ConsumeGrant(ctx context.Context, actorID string, actorType domain.AutomationGrantActorType,
		toolName string, riskClass domain.RiskClass, now int64) (*domain.AutomationGrantRecord, error)
}

// MCPClient is the slice of the MCP transport that daintree.call forwards to.
// CallTool returns the structured result; Connected gates the call.
type MCPClient interface {
	CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error)
	Connected() bool
}

// MCPCallResult mirrors the Daintree MCP call envelope used by daintree.call.
type MCPCallResult struct {
	Text              string `json:"text"`
	StructuredContent any    `json:"structuredContent,omitempty"`
	IsError           bool   `json:"isError"`
}

// Queue is the slice of the attention queue the registry uses to publish the
// "Autonomous action blocked" denial event for non-interactive actors. Publishing
// is best-effort (wrapped so it can never break the call). Spec: tools-core.md §4.
type Queue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
}

// Router is the slice of model access a handler may reach. The dispatch pipeline
// itself does not call it; it is threaded through ToolContext for handlers (e.g.
// summarization tools).
type Router interface {
	// Reserved for handler use; kept minimal so the registry compiles in isolation.
	// Tool families that need streaming will widen their own consumer interface.
}
