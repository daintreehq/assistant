package tools

import (
	"context"
	"time"

	"github.com/daintreehq/assistant/internal/domain"
)

// Cross-subsystem dependencies are declared as SMALL consumer-defined interfaces
// here: exactly the methods the dispatch pipeline / exemplar tools call on other
// subsystems. We deliberately do NOT import the concrete storage/mcp/queue/models
// packages (that would create import cycles and couple the build) — the concrete
// types will satisfy these by structural match.

// Store is the slice of the storage layer the registry touches: the audit insert
// and the atomic grant consume. InsertAudit returns the new row id (mutated onto
// the ToolResult as AuditID). ConsumeGrant returns the consumed grant (non-nil)
// when a matching, live grant authorized the (actor, tool, risk); nil otherwise.
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

// MCPCallOptions are the per-call knobs a tool handler may set on ONE MCP call.
//
// Timeout is the wire deadline for this call. Zero means "use the transport's own
// default" (120s), which is right for every bounded Daintree read/write. It exists
// for the handful of actions whose SERVER-SIDE budget legitimately exceeds that —
// project.runCheck runs a project command for up to an hour — where the default
// would abort the call long before the work it is waiting on could finish, and
// report that abort as a tool error rather than as the truncation it is.
//
// Deliberately NOT a mirror of mcp.CallOptions: Retries is withheld, because a
// wrapper that could ask for retries could ask for them on a non-idempotent
// action. Retry-safety stays a property of the tool, decided in internal/mcp.
type MCPCallOptions struct {
	Timeout time.Duration
}

// MCPClient is the slice of the MCP transport that daintree.call forwards to.
// CallTool returns the structured result; Connected gates the call. The opts
// parameter carries per-call knobs (see MCPCallOptions); pass the zero value for
// the transport default.
type MCPClient interface {
	CallTool(ctx context.Context, name string, args map[string]any, opts MCPCallOptions) (MCPCallResult, error)
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
// is best-effort (wrapped so it can never break the call).
type Queue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
}
