package host

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// HostEvent is the outbound (host → Daintree) wire union. Every event carries a
// "type" discriminator + "sessionId"; the Go encoder writes each as one NDJSON
// line on stdout. Concrete event structs implement encode() which injects "type"
// and "sessionId" so callers never repeat them. Field names are verbatim wire.
type HostEvent interface {
	// encode returns the full NDJSON object (with type + sessionId) for stdout.
	encode(sessionID string) ([]byte, error)
}

// marshalEvent serializes a flat map as one JSON object. Centralizes the "+type
// +sessionId" injection so every event encodes identically.
func marshalEvent(typ, sessionID string, fields map[string]any) ([]byte, error) {
	obj := make(map[string]any, len(fields)+2)
	obj["type"] = typ
	obj["sessionId"] = sessionID
	for k, v := range fields {
		obj[k] = v
	}
	// No HTML escaping: a token/summary/message containing <, >, & must reach
	// Daintree literal (matching JSON.stringify), not as <. The encoder
	// appends a newline; trim it (the transport adds the single NDJSON newline).
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(obj); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}

// EvReady — host:ready. resumedSessionId only set when the descriptor carried a
// resumeSessionId.
type EvReady struct {
	ProtocolVersion  int
	ResumedSessionID string
}

func (e EvReady) encode(sid string) ([]byte, error) {
	f := map[string]any{"protocolVersion": e.ProtocolVersion}
	if e.ResumedSessionID != "" {
		f["resumedSessionId"] = e.ResumedSessionID
	}
	return marshalEvent("host:ready", sid, f)
}

// EvTurnStart — turn:start.
type EvTurnStart struct {
	TurnID    string
	Role      TurnRole
	StartedAt int64
}

func (e EvTurnStart) encode(sid string) ([]byte, error) {
	return marshalEvent("turn:start", sid, map[string]any{
		"turnId":    e.TurnID,
		"role":      string(e.Role),
		"startedAt": e.StartedAt,
	})
}

// EvTurnToken — turn:token.
type EvTurnToken struct {
	TurnID string
	Chunk  string
}

func (e EvTurnToken) encode(sid string) ([]byte, error) {
	return marshalEvent("turn:token", sid, map[string]any{
		"turnId": e.TurnID,
		"chunk":  e.Chunk,
	})
}

// EvTurnEnd — turn:end. Outcome optional.
type EvTurnEnd struct {
	TurnID  string
	EndedAt int64
	Outcome TurnOutcomeClass
}

func (e EvTurnEnd) encode(sid string) ([]byte, error) {
	f := map[string]any{"turnId": e.TurnID, "endedAt": e.EndedAt}
	if e.Outcome != "" {
		f["outcome"] = string(e.Outcome)
	}
	return marshalEvent("turn:end", sid, f)
}

// EvToolStarted — tool:started. turnId optional.
type EvToolStarted struct {
	ToolCallID  string
	ToolID      string
	ArgsSummary string
	StartedAt   int64
	TurnID      string
	Danger      bool
}

func (e EvToolStarted) encode(sid string) ([]byte, error) {
	f := map[string]any{
		"toolCallId":  e.ToolCallID,
		"toolId":      e.ToolID,
		"argsSummary": e.ArgsSummary,
		"startedAt":   e.StartedAt,
		"danger":      e.Danger,
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("tool:started", sid, f)
}

// EvToolSettled — tool:settled. errorCode + turnId + asyncId optional.
type EvToolSettled struct {
	ToolCallID string
	ToolID     string
	DurationMs int64
	Result     AuditResult
	Severity   AuditSeverity
	ErrorCode  string
	TurnID     string
	// AsyncID marks an ACCEPTED-but-still-running async operation (asy_…): the
	// call settled but the work continues in the background, so a host must NOT
	// render it as a finished success (the cockpit shows it as a distinct yellow
	// pending state). Empty for every ordinary synchronous result.
	AsyncID string
}

func (e EvToolSettled) encode(sid string) ([]byte, error) {
	f := map[string]any{
		"toolCallId": e.ToolCallID,
		"toolId":     e.ToolID,
		"durationMs": e.DurationMs,
		"result":     string(e.Result),
		"severity":   string(e.Severity),
	}
	if e.ErrorCode != "" {
		f["errorCode"] = e.ErrorCode
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	if e.AsyncID != "" {
		f["asyncId"] = e.AsyncID
	}
	return marshalEvent("tool:settled", sid, f)
}

// EvApprovalRequested — approval:requested. turnId optional. riskClass,
// consequence, and argsSummary are optional display context (parity with a local
// cockpit approval); each is omitted from the wire object when empty.
type EvApprovalRequested struct {
	ApprovalID  string
	ToolID      string
	Summary     string
	RequestedAt int64
	TurnID      string
	RiskClass   domain.RiskClass
	Consequence string
	ArgsSummary string
}

func (e EvApprovalRequested) encode(sid string) ([]byte, error) {
	f := map[string]any{
		"approvalId":  e.ApprovalID,
		"toolId":      e.ToolID,
		"summary":     e.Summary,
		"requestedAt": e.RequestedAt,
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	if e.RiskClass != "" {
		f["riskClass"] = string(e.RiskClass)
	}
	if e.Consequence != "" {
		f["consequence"] = e.Consequence
	}
	if e.ArgsSummary != "" {
		f["argsSummary"] = e.ArgsSummary
	}
	return marshalEvent("approval:requested", sid, f)
}

// EvApprovalDecided — approval:decided.
type EvApprovalDecided struct {
	ApprovalID string
	Decision   ConfirmationDecision
	DecidedAt  int64
}

func (e EvApprovalDecided) encode(sid string) ([]byte, error) {
	return marshalEvent("approval:decided", sid, map[string]any{
		"approvalId": e.ApprovalID,
		"decision":   string(e.Decision),
		"decidedAt":  e.DecidedAt,
	})
}

// EvError — host:error.
type EvError struct {
	Code    string
	Message string
}

func (e EvError) encode(sid string) ([]byte, error) {
	return marshalEvent("host:error", sid, map[string]any{
		"code":    e.Code,
		"message": e.Message,
	})
}

// EvShutdown — host:shutdown. resumeSessionId optional. Emitted FIRST in teardown.
type EvShutdown struct {
	Reason          HostShutdownReason
	ResumeSessionID string
}

func (e EvShutdown) encode(sid string) ([]byte, error) {
	f := map[string]any{"reason": string(e.Reason)}
	if e.ResumeSessionID != "" {
		f["resumeSessionId"] = e.ResumeSessionID
	}
	return marshalEvent("host:shutdown", sid, f)
}
