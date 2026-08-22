// Package host implements the embedded assistant-host protocol.
//
// The transport is stdio NDJSON — one JSON object per line: stdin carries the
// inbound command stream (first line = SessionDescriptor, every subsequent line =
// a HostCommand); stdout carries the outbound HostEvent stream; stderr carries
// human-readable diagnostics. Protocol JSON is NEVER written to stderr (and
// diagnostics are NEVER written to stdout) — Daintree Zod-validates stdout
// line-by-line and rejects unknown shapes.
//
// This file is the pure protocol layer: the version constant, the verbatim string
// vocabularies, the wire event/command types with their JSON shapes, the severity
// map, and the descriptor/command parsers. No I/O.
//
// The wire contract — event names, command names, field names, vocabulary values,
// PROTOCOL_VERSION — is the one hard external contract and must stay byte-for-byte
// with Daintree.
package host

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// PROTOCOL_VERSION is the wire-format version. MUST equal Daintree's
// ASSISTANT_HOST_PROTOCOL_VERSION; Daintree Zod-rejects an unrecognized version.
//
// It is 3. v2 described a TERMINAL SESSION for a parent that drew an activity strip
// beside an xterm; v3 describes a CONVERSATION for a parent that renders the whole
// thing. Three changes make it breaking rather than additive:
//
//  1. Every event carries a monotonic `seq`. v2 dropped frames silently when the
//     writer queue filled, with no way for a consumer to notice — unusable once the
//     transcript IS the product. v3 applies backpressure to stream traffic instead,
//     and `seq` makes any residual gap detectable rather than invisible.
//  2. `turn:end` carries the authoritative final `content`. v2 carried only an
//     outcome class, so a lost token frame could never be repaired.
//  3. The event set covers what the runtime actually produces — phase, reasoning,
//     interjections, the whole tool batch, tool state and progress, usage, cost, and
//     notices — instead of the subset a strip needed.
const ProtocolVersion = 3

// BuildVersion is the engine build string reported in host:ready, injected by the
// CLI layer at startup (main's -ldflags value). Package-level rather than an App
// interface method because it is a build constant, not session state.
var BuildVersion = "dev"

// ---------------------------------------------------------------------------
// Vocabularies (verbatim wire strings). Validated on decode.
// ---------------------------------------------------------------------------

// AuditResult is the settled-tool result classification (7 values).
type AuditResult string

const (
	AuditSuccess             AuditResult = "success"
	AuditError               AuditResult = "error"
	AuditConfirmationPending AuditResult = "confirmation-pending"
	AuditUnauthorized        AuditResult = "unauthorized"
	AuditDedup               AuditResult = "dedup"
	AuditCollision           AuditResult = "collision"
	AuditRateLimited         AuditResult = "rate_limited"
)

// AuditSeverity is the severity bucket for an audited result (5 values). Note
// "critical" is in the set for Daintree parity but is never produced by
// SeverityForResult.
type AuditSeverity string

const (
	SeverityInfo     AuditSeverity = "info"
	SeverityNotice   AuditSeverity = "notice"
	SeverityWarning  AuditSeverity = "warning"
	SeverityErrorSev AuditSeverity = "error"
	SeverityCritical AuditSeverity = "critical"
)

// ConfirmationDecision is the outcome of an approval (3 values).
type ConfirmationDecision string

const (
	DecisionApproved ConfirmationDecision = "approved"
	DecisionRejected ConfirmationDecision = "rejected"
	DecisionTimeout  ConfirmationDecision = "timeout"
)

// TurnOutcomeClass classifies how a turn ended (12 values).
type TurnOutcomeClass string

const (
	OutcomeAnswered             TurnOutcomeClass = "answered"
	OutcomeHedged               TurnOutcomeClass = "hedged"
	OutcomeRefused              TurnOutcomeClass = "refused"
	OutcomeDocsEmpty            TurnOutcomeClass = "docs-empty"
	OutcomeTierRejected         TurnOutcomeClass = "tier-rejected"
	OutcomeMcpNotReady          TurnOutcomeClass = "mcp-not-ready"
	OutcomeAgentStuck           TurnOutcomeClass = "agent-stuck"
	OutcomeToolError            TurnOutcomeClass = "tool-error"
	OutcomeReasoningLoop        TurnOutcomeClass = "reasoning-loop"
	OutcomeHibernateResumeStale TurnOutcomeClass = "hibernate-resume-stale"
	OutcomeCancelled            TurnOutcomeClass = "cancelled"
	OutcomeUnknown              TurnOutcomeClass = "unknown"
)

// TurnRole distinguishes the user prompt turn from the assistant reply turn.
type TurnRole string

const (
	RoleUser      TurnRole = "user"
	RoleAssistant TurnRole = "assistant"
)

// HostShutdownReason is why teardown ran (4 values). "revoke" is kept for
// Daintree parity even though index/teardown never emits it.
type HostShutdownReason string

const (
	ShutdownHibernate HostShutdownReason = "hibernate"
	ShutdownRevoke    HostShutdownReason = "revoke"
	ShutdownError     HostShutdownReason = "error"
	ShutdownExit      HostShutdownReason = "exit"
)

// severityByResult mirrors Daintree's SEVERITY_BY_RESULT. It never maps to
// "critical".
var severityByResult = map[AuditResult]AuditSeverity{
	AuditSuccess:             SeverityInfo,
	AuditDedup:               SeverityInfo,
	AuditConfirmationPending: SeverityNotice,
	AuditUnauthorized:        SeverityWarning,
	AuditRateLimited:         SeverityWarning,
	AuditCollision:           SeverityWarning,
	AuditError:               SeverityErrorSev,
}

// SeverityForResult maps an AuditResult to its AuditSeverity (unknown → error).
func SeverityForResult(r AuditResult) AuditSeverity {
	if sev, ok := severityByResult[r]; ok {
		return sev
	}
	return SeverityErrorSev
}

// ---------------------------------------------------------------------------
// SessionDescriptor (first inbound message). Carries NO secret — the bearer
// token + MCP URL arrive via env, so a leaked stdin line can't carry the secret.
// ---------------------------------------------------------------------------

// SessionDescriptor is the non-secret handshake Daintree sends as the very first
// inbound line. windowId/projectId/tier are validated here but the live binding
// values come from env via loadConfig — a leaked descriptor cannot re-bind.
type SessionDescriptor struct {
	SessionID       string `json:"sessionId"`
	WindowID        int64  `json:"windowId"`
	ProjectID       string `json:"projectId"`
	Cwd             string `json:"cwd"`
	Tier            string `json:"tier"`
	ProtocolVersion int    `json:"protocolVersion"`
	ResumeSessionID string `json:"resumeSessionId,omitempty"`
}

// ParseDescriptor decodes + validates a descriptor line: the six required fields
// must be present with the right JSON types; resumeSessionId is optional and NOT
// type-checked. A failing line yields an error (caller emits host:error code
// bad-descriptor + teardown).
func ParseDescriptor(line []byte) (SessionDescriptor, error) {
	// Decode into a loose map first so we can assert presence+type exactly (a
	// missing field must fail, not default to zero).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return SessionDescriptor{}, fmt.Errorf("descriptor is not a JSON object: %w", err)
	}
	var d SessionDescriptor
	if err := wantString(raw, "sessionId", &d.SessionID); err != nil {
		return SessionDescriptor{}, err
	}
	if err := wantString(raw, "projectId", &d.ProjectID); err != nil {
		return SessionDescriptor{}, err
	}
	if err := wantString(raw, "cwd", &d.Cwd); err != nil {
		return SessionDescriptor{}, err
	}
	if err := wantString(raw, "tier", &d.Tier); err != nil {
		return SessionDescriptor{}, err
	}
	if err := wantNumber(raw, "windowId", &d.WindowID); err != nil {
		return SessionDescriptor{}, err
	}
	var pv int64
	if err := wantIntegralNumber(raw, "protocolVersion", &pv); err != nil {
		return SessionDescriptor{}, err
	}
	d.ProtocolVersion = int(pv)
	// resumeSessionId is optional — decode if present, ignore type mismatches by
	// only reading a JSON string (matches "NOT checked" — absence is fine).
	if v, ok := raw["resumeSessionId"]; ok {
		_ = json.Unmarshal(v, &d.ResumeSessionID)
	}
	return d, nil
}

func wantString(raw map[string]json.RawMessage, key string, dst *string) error {
	v, ok := raw[key]
	if !ok {
		return fmt.Errorf("descriptor missing required string field %q", key)
	}
	if err := json.Unmarshal(v, dst); err != nil {
		return fmt.Errorf("descriptor field %q must be a string", key)
	}
	return nil
}

func wantNumber(raw map[string]json.RawMessage, key string, dst *int64) error {
	v, ok := raw[key]
	if !ok {
		return fmt.Errorf("descriptor missing required number field %q", key)
	}
	// A JSON number token starts with '-' or a digit. json.Number (a string kind)
	// will happily decode a QUOTED number `"7"`, so we must reject a leading quote
	// ourselves to honor the number-type gate (a quoted number is a string, not a
	// number).
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return fmt.Errorf("descriptor field %q must be a number", key)
	}
	var num json.Number
	if err := json.Unmarshal(v, &num); err != nil {
		return fmt.Errorf("descriptor field %q must be a number", key)
	}
	n, err := num.Int64()
	if err != nil {
		// Accept a float wire value (e.g. 1.0) by truncating, matching JS numbers.
		f, ferr := num.Float64()
		if ferr != nil {
			return fmt.Errorf("descriptor field %q must be a number", key)
		}
		n = int64(f)
	}
	*dst = n
	return nil
}

// wantIntegralNumber is wantNumber but rejects any non-integral value: a wire
// `2.9` must NOT silently truncate to 2 and slip past the protocol-version ==
// check. A JS number is a float, so a fractional protocolVersion is a real
// mismatch (or a malformed peer) and must be refused. Whole floats like 2.0 are
// accepted (they equal their integer).
func wantIntegralNumber(raw map[string]json.RawMessage, key string, dst *int64) error {
	v, ok := raw[key]
	if !ok {
		return fmt.Errorf("descriptor missing required number field %q", key)
	}
	trimmed := bytes.TrimSpace(v)
	if len(trimmed) == 0 || trimmed[0] == '"' {
		return fmt.Errorf("descriptor field %q must be a number", key)
	}
	var num json.Number
	if err := json.Unmarshal(v, &num); err != nil {
		return fmt.Errorf("descriptor field %q must be a number", key)
	}
	if n, err := num.Int64(); err == nil {
		*dst = n
		return nil
	}
	// Not an int64: accept only an exact integral float (2.0), reject fractions.
	f, ferr := num.Float64()
	if ferr != nil {
		return fmt.Errorf("descriptor field %q must be a number", key)
	}
	if f != float64(int64(f)) {
		return fmt.Errorf("descriptor field %q must be an integer, got %s", key, num.String())
	}
	*dst = int64(f)
	return nil
}

// ---------------------------------------------------------------------------
// HostCommand (inbound, after the descriptor).
// ---------------------------------------------------------------------------

// HostCommandType is the inbound command discriminator.
type HostCommandType string

const (
	CmdPrompt         HostCommandType = "prompt"
	CmdApprovalDecide HostCommandType = "approval:decide"
	CmdInterrupt      HostCommandType = "interrupt"
	CmdHibernate      HostCommandType = "hibernate"
	CmdShutdown       HostCommandType = "shutdown"
)

// HostCommand is a decoded inbound command. Only the fields relevant to the arm
// are populated; the rest are zero. SessionID is always required.
type HostCommand struct {
	Type      HostCommandType
	SessionID string
	// prompt
	Text string
	// approval:decide
	ApprovalID string
	Decision   string
}

// errNotCommand signals a line that is not a recognizable command — the running
// state machine silently DROPS these (foreign/garbled), never emitting an error.
var errNotCommand = errors.New("host: not a valid command")

// normalizeDecision coerces an inbound decision string to the closed
// ConfirmationDecision vocabulary. Unknown → rejected (fail-safe: an unparseable
// decision must never be treated as an approval).
func normalizeDecision(s string) ConfirmationDecision {
	switch ConfirmationDecision(s) {
	case DecisionApproved:
		return DecisionApproved
	case DecisionRejected:
		return DecisionRejected
	case DecisionTimeout:
		return DecisionTimeout
	default:
		return DecisionRejected
	}
}

// ParseCommand decodes + validates an inbound command line: object with a string
// sessionId, then a per-type field check. An unknown type or a missing required
// field → errNotCommand (drop).
func ParseCommand(line []byte) (HostCommand, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return HostCommand{}, errNotCommand
	}
	var cmd HostCommand
	if err := wantString(raw, "sessionId", &cmd.SessionID); err != nil {
		return HostCommand{}, errNotCommand
	}
	var typ string
	if err := wantString(raw, "type", &typ); err != nil {
		return HostCommand{}, errNotCommand
	}
	cmd.Type = HostCommandType(typ)
	switch cmd.Type {
	case CmdPrompt:
		if err := wantString(raw, "text", &cmd.Text); err != nil {
			return HostCommand{}, errNotCommand
		}
	case CmdApprovalDecide:
		if err := wantString(raw, "approvalId", &cmd.ApprovalID); err != nil {
			return HostCommand{}, errNotCommand
		}
		if err := wantString(raw, "decision", &cmd.Decision); err != nil {
			return HostCommand{}, errNotCommand
		}
		// Normalize the decision to the closed vocabulary (approved|rejected|timeout).
		// An unknown value (e.g. "wat") must NOT be echoed back verbatim as an invalid
		// approval:decided — collapse it to the safe default (rejected) so a parked
		// dispatch unblocks declined rather than emitting an off-contract decision.
		cmd.Decision = string(normalizeDecision(cmd.Decision))
	case CmdInterrupt, CmdHibernate, CmdShutdown:
		// no extra fields
	default:
		return HostCommand{}, errNotCommand
	}
	return cmd, nil
}
