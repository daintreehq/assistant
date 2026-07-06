// Package runsv2 is the client for the backend's durable run protocol
// (/v2/runs — the Run Supervisor; see docs/RUN_SUPERVISOR.md in the backend
// repo). Under v2 the BACKEND owns the turn loop and the transcript: the CLI
// submits user turns, tails the run's event log over SSE, claims tool leases,
// executes them through the local registry/permission system, and posts
// exactly-once results.
//
// This package is groundwork: a complete, typed protocol client with resume
// cursors and idempotency-aware result posting. Wiring it into the session
// loop (replacing RespondStream round-driving) is the follow-on milestone —
// the natural host is the internal/host executor loop, which is already
// non-blocking and lease-shaped.
package runsv2

import "encoding/json"

// ---- turns -----------------------------------------------------------------

// Message is one user message in a turn submission. v2 accepts ONLY user-role
// messages: assistant and tool messages are server-recorded facts.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// TurnSubmit is the body of POST /v2/runs/{id}/turns (and the "turn" block of
// POST /v2/runs). TurnID is the idempotency key: an identical resubmission is
// acknowledged as a duplicate; a different payload under the same TurnID is a
// 409 turn_conflict.
type TurnSubmit struct {
	TurnID     string            `json:"turn_id"`
	Messages   []Message         `json:"messages"`
	Runtime    *json.RawMessage  `json:"runtime,omitempty"`
	Turn       *json.RawMessage  `json:"turn,omitempty"`
	Tools      []json.RawMessage `json:"tools,omitempty"`
	Generation *Generation       `json:"generation,omitempty"`
	Selection  *Selection        `json:"selection,omitempty"`
}

// Generation carries the per-run generation knobs v2 accepts (streaming and
// response_format are server-owned).
type Generation struct {
	Temperature *float64 `json:"temperature,omitempty"`
	MaxTokens   *int     `json:"max_tokens,omitempty"`
	Stop        []string `json:"stop,omitempty"`
}

// Selection mirrors the v1 selection policy block.
type Selection struct {
	Policy string `json:"policy,omitempty"`
	Force  bool   `json:"force,omitempty"`
}

// ClientInfo identifies the submitting client (mirrors the v1 block).
type ClientInfo struct {
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// CreateRun is the body of POST /v2/runs.
type CreateRun struct {
	SessionID string      `json:"session_id,omitempty"`
	ProjectID string      `json:"project_id,omitempty"`
	Title     string      `json:"title,omitempty"`
	Client    *ClientInfo `json:"client,omitempty"`
	Turn      TurnSubmit  `json:"turn"`
}

// TurnAccepted acknowledges a turn submission.
type TurnAccepted struct {
	RunID     string `json:"run_id"`
	TurnID    string `json:"turn_id"`
	Status    string `json:"status"`
	Seq       int64  `json:"seq,omitempty"`
	Duplicate bool   `json:"duplicate,omitempty"`
}

// ---- runs --------------------------------------------------------------------

// Run is the run detail view. Status is one of idle | running | awaiting_tools |
// awaiting_approval | failed.
type Run struct {
	RunID           string   `json:"run_id"`
	Status          string   `json:"status"`
	SessionID       string   `json:"session_id,omitempty"`
	ProjectID       string   `json:"project_id,omitempty"`
	Title           string   `json:"title,omitempty"`
	ActiveSkills    []string `json:"active_skills"`
	CurrentRound    int      `json:"current_round"`
	CurrentTurnID   string   `json:"current_turn_id,omitempty"`
	LastSeq         int64    `json:"last_seq"`
	ExecutorPresent bool     `json:"executor_present"`
	LastError       string   `json:"last_error,omitempty"`
	CreatedAt       float64  `json:"created_at"`
	UpdatedAt       float64  `json:"updated_at"`
}

// RunCreated is the response of POST /v2/runs.
type RunCreated struct {
	Run  Run          `json:"run"`
	Turn TurnAccepted `json:"turn"`
}

// ---- events ---------------------------------------------------------------------

// Event is one entry from GET /v2/runs/{id}/events. Persisted events carry a
// positive Seq (the resume cursor); ephemeral events (model.delta,
// model.status, stream.reset) have Seq 0 and are advisory only — a reconnect
// loses nothing durable.
type Event struct {
	Seq     int64
	Type    string
	TS      float64
	Payload json.RawMessage
}

// Ephemeral reports whether the event is advisory (never persisted).
func (e Event) Ephemeral() bool { return e.Seq == 0 }

// ---- leases ------------------------------------------------------------------------

// Lease is a durable, claimable unit of local tool work. The CLI executes the
// tool named by ToolName with Arguments (a JSON string, exactly as the model
// emitted it) and posts one result. IdempotencyKey is stable across retries
// and re-claims — thread it into any side-effecting operation that supports one.
type Lease struct {
	LeaseID          string  `json:"lease_id"`
	RunID            string  `json:"run_id"`
	Round            int     `json:"round"`
	ToolCallID       string  `json:"tool_call_id"`
	ToolName         string  `json:"tool_name"`
	Arguments        string  `json:"arguments"`
	Risk             string  `json:"risk,omitempty"`
	Status           string  `json:"status"`
	RequiresApproval bool    `json:"requires_approval"`
	ApprovalID       string  `json:"approval_id,omitempty"`
	ExecutorID       string  `json:"executor_id,omitempty"`
	Attempt          int     `json:"attempt"`
	ClaimExpiresAt   float64 `json:"claim_expires_at,omitempty"`
	Deadline         float64 `json:"deadline"`
	IdempotencyKey   string  `json:"idempotency_key"`
	ResultStatus     string  `json:"result_status,omitempty"`
	CreatedAt        float64 `json:"created_at"`
	UpdatedAt        float64 `json:"updated_at"`
}

// LeaseClaim acknowledges a claim. Outcome is "claimed" (fresh claim) or
// "extended" (the same executor re-claimed to extend its TTL).
type LeaseClaim struct {
	Outcome string `json:"outcome"`
	Lease   Lease  `json:"lease"`
}

// LeaseResult is the body of POST /v2/tool-leases/{id}/result. Status is
// ok | error | declined (declined = the local user refused confirmation).
type LeaseResult struct {
	ExecutorID string `json:"executor_id"`
	Status     string `json:"status"`
	Content    string `json:"content"`
}

// LeaseResultAck acknowledges a result. Outcome "recorded" means this call won
// the exactly-once fence; "duplicate" means an identical result was already
// recorded (a safe retry) — either way the side effect is counted once.
type LeaseResultAck struct {
	Outcome string `json:"outcome"`
	Lease   Lease  `json:"lease"`
}

// ---- approvals ---------------------------------------------------------------------

// ApprovalResolve is the body of POST /v2/approvals/{id}.
type ApprovalResolve struct {
	Approve bool   `json:"approve"`
	Note    string `json:"note,omitempty"`
}

// Approval is the approval view returned after resolution.
type Approval struct {
	ApprovalID  string  `json:"approval_id"`
	RunID       string  `json:"run_id"`
	LeaseID     string  `json:"lease_id"`
	Status      string  `json:"status"`
	Reason      string  `json:"reason,omitempty"`
	Note        string  `json:"note,omitempty"`
	RequestedAt float64 `json:"requested_at"`
	ResolvedAt  float64 `json:"resolved_at,omitempty"`
}

// ---- executors ------------------------------------------------------------------------

// ExecutorTool advertises one local tool (wire name + risk class) so the
// backend can risk-classify leases and gate background mutation on approvals.
type ExecutorTool struct {
	Name        string `json:"name"`
	Risk        string `json:"risk,omitempty"`
	Description string `json:"description,omitempty"`
}

// Heartbeat is the body of POST /v2/executors/heartbeat.
type Heartbeat struct {
	ExecutorID   string         `json:"executor_id"`
	ProjectID    string         `json:"project_id,omitempty"`
	Client       *ClientInfo    `json:"client,omitempty"`
	Tools        []ExecutorTool `json:"tools,omitempty"`
	Capabilities map[string]any `json:"capabilities,omitempty"`
	TTLSeconds   float64        `json:"ttl_seconds,omitempty"`
}

// HeartbeatAck returns the executor's registered lease-pool view: every
// pending lease, so a reconnecting executor can sweep for work it should claim.
type HeartbeatAck struct {
	ExecutorID    string  `json:"executor_id"`
	ExpiresAt     float64 `json:"expires_at"`
	PendingLeases []Lease `json:"pending_leases"`
}

// ---- wakeups -----------------------------------------------------------------------------

// WakeupCreate schedules a durable, server-side wake for a run: exactly one of
// DelaySeconds / FireAt must be set. The backend fires it whether or not any
// CLI is connected.
type WakeupCreate struct {
	DelaySeconds *float64 `json:"delay_seconds,omitempty"`
	FireAt       *float64 `json:"fire_at,omitempty"`
	Note         string   `json:"note,omitempty"`
}

// Wakeup identifies a scheduled wake.
type Wakeup struct {
	WakeupID  string  `json:"wakeup_id"`
	RunID     string  `json:"run_id"`
	FireAt    float64 `json:"fire_at"`
	Status    string  `json:"status,omitempty"`
	Note      string  `json:"note,omitempty"`
	CreatedAt float64 `json:"created_at,omitempty"`
	FiredAt   float64 `json:"fired_at,omitempty"`
}

// ---- replay -------------------------------------------------------------------------------

// ReplayReport verifies that a recorded round's prompt is exactly reproducible
// from the durable event log.
type ReplayReport struct {
	Found                   bool            `json:"found"`
	Match                   bool            `json:"match"`
	Round                   int             `json:"round"`
	RecordedFingerprint     string          `json:"recorded_fingerprint,omitempty"`
	ComputedFingerprint     string          `json:"computed_fingerprint,omitempty"`
	MismatchReason          string          `json:"mismatch_reason,omitempty"`
	CatalogRevisionRecorded string          `json:"catalog_revision_recorded,omitempty"`
	CatalogRevisionCurrent  string          `json:"catalog_revision_current,omitempty"`
	Request                 json.RawMessage `json:"request,omitempty"`
}
