package host

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/daintreehq/daintree-assistant/internal/agent"
	"github.com/daintreehq/daintree-assistant/internal/domain"
)

// Approval/redaction constants.
const (
	// DefaultApprovalTimeoutMs is the unanswered-confirm auto-timeout (5 min).
	DefaultApprovalTimeoutMs = 5 * 60_000 // 300000
	// argsSummaryMaxString collapses any string longer than this in redactArgs.
	argsSummaryMaxString = 80
)

// PostFunc is the transport sink the bridge writes events through. Injected so
// the bridge has no transport dependency (tests + the NDJSON owner share it).
type PostFunc func(HostEvent)

// RiskOfFunc looks up a tool's risk class for the danger hint. The bool is false
// when the tool is unknown.
type RiskOfFunc func(toolName string) (domain.RiskClass, bool)

// BridgeOptions configures a Bridge.
type BridgeOptions struct {
	SessionID         string
	Post              PostFunc
	RiskOf            RiskOfFunc   // default: always unknown
	Now               func() int64 // default: domain.NowMS
	ApprovalTimeoutMs int          // default: DefaultApprovalTimeoutMs (0 disables the timer)
}

// pendingApproval is one outstanding confirm: a channel the confirm() caller
// blocks on plus the auto-timeout timer.
type pendingApproval struct {
	resolve chan ConfirmationDecision
	timer   *time.Timer
}

// Bridge adapts the in-process agent EventSink + tool-confirm hook into wire
// HostEvents. It owns the single-turn lifecycle, approvals, redaction, and audit
// mapping. The agent loop runs Send() on another goroutine and calls the sink
// methods concurrently, so all mutable state is guarded by mu. No transport
// dependency — events go through the injected Post.
type Bridge struct {
	sessionID         string
	post              PostFunc
	riskOf            RiskOfFunc
	now               func() int64
	approvalTimeoutMs int

	mu               sync.Mutex
	activeTurnID     string
	interrupted      bool // latched until next startExchange
	pendingApprovals map[string]*pendingApproval
	toolStartedAt    map[string]int64
}

// NewBridge builds a Bridge with defaults filled.
func NewBridge(opts BridgeOptions) *Bridge {
	if opts.RiskOf == nil {
		opts.RiskOf = func(string) (domain.RiskClass, bool) { return "", false }
	}
	if opts.Now == nil {
		opts.Now = domain.NowMS
	}
	if opts.ApprovalTimeoutMs == 0 {
		opts.ApprovalTimeoutMs = DefaultApprovalTimeoutMs
	}
	return &Bridge{
		sessionID:         opts.SessionID,
		post:              opts.Post,
		riskOf:            opts.RiskOf,
		now:               opts.Now,
		approvalTimeoutMs: opts.ApprovalTimeoutMs,
		pendingApprovals:  make(map[string]*pendingApproval),
		toolStartedAt:     make(map[string]int64),
	}
}

// genID returns "<prefix>_<8 hex>" (e.g. turn_1a2b3c4d). domain.NewID expects a
// trailing-underscore prefix.
func genID(prefix string) string { return domain.NewID(prefix + "_") }

// ---------------------------------------------------------------------------
// EventSink — the agent.EventSink handed to the App via setHooks.
// ONE assistant turn spans a whole Session.Send() across model iterations + tool
// calls: AssistantStart fires once per round but only the FIRST opens the turn.
// ---------------------------------------------------------------------------

// Phase is live-only UI vocabulary with no host-protocol channel — dropped.
func (b *Bridge) Phase(domain.RunPhase) {}

func (b *Bridge) AssistantStart() {
	b.mu.Lock()
	if b.interrupted || b.activeTurnID != "" {
		b.mu.Unlock()
		return
	}
	turnID := genID("turn")
	b.activeTurnID = turnID
	now := b.now()
	b.mu.Unlock()
	b.post(EvTurnStart{TurnID: turnID, Role: RoleAssistant, StartedAt: now})
}

func (b *Bridge) AssistantToken(chunk string) {
	b.mu.Lock()
	if b.interrupted || b.activeTurnID == "" {
		b.mu.Unlock()
		return
	}
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvTurnToken{TurnID: turnID, Chunk: chunk})
}

// AssistantEnd closes the turn: "answered" if content is non-blank else "unknown".
// (reasoning is not forwarded over the host protocol.)
func (b *Bridge) AssistantEnd(content, _ string) {
	outcome := OutcomeUnknown
	if trimNonEmpty(content) {
		outcome = OutcomeAnswered
	}
	b.closeTurn(outcome)
}

func (b *Bridge) AssistantCancelled(string) { b.closeTurn(OutcomeCancelled) }

// ToolBatch/ToolState/ToolProgress are live-footer-only in the loop; the host
// protocol keys off the concrete tool:started/tool:settled events, so the
// in-tool substep stream has no host channel and is dropped.
func (b *Bridge) ToolBatch([]agent.BatchedToolCall) {}
func (b *Bridge) ToolState(string, agent.ToolState) {}
func (b *Bridge) ToolProgress(string, string)       {}

func (b *Bridge) ToolCall(ev agent.ToolCallEvent) {
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	b.toolStartedAt[ev.ID] = ev.StartedAt
	turnID := b.activeTurnID
	b.mu.Unlock()
	b.post(EvToolStarted{
		ToolCallID:  ev.ID,
		ToolID:      ev.Name,
		ArgsSummary: redactArgs(ev.Args),
		StartedAt:   ev.StartedAt,
		TurnID:      turnID,
		Danger:      b.isDanger(ev.Name),
	})
}

func (b *Bridge) ToolResult(ev agent.ToolResultEvent) {
	b.mu.Lock()
	if b.interrupted {
		b.mu.Unlock()
		return
	}
	startedAt, hadStart := b.toolStartedAt[ev.ID]
	delete(b.toolStartedAt, ev.ID)
	turnID := b.activeTurnID
	b.mu.Unlock()

	var durationMs int64
	if hadStart {
		d := ev.EndedAt - startedAt
		if d > 0 {
			durationMs = d
		}
	}
	result, severity, errorCode := resultToAudit(ev.Result)
	b.post(EvToolSettled{
		ToolCallID: ev.ID,
		ToolID:     ev.Name,
		DurationMs: durationMs,
		Result:     result,
		Severity:   severity,
		ErrorCode:  errorCode,
		TurnID:     turnID,
	})
}

func (b *Bridge) Error(message string) {
	b.post(EvError{Code: "turn-error", Message: message})
	b.closeTurn(OutcomeUnknown)
}

// Info has no protocol channel — intentionally dropped.
func (b *Bridge) Info(string) {}

// Usage is not forwarded over the host protocol (token/cost stays in-process).
func (b *Bridge) Usage(agent.UsageEvent) {}

// TurnPrompt has no host-protocol channel — Daintree originated the prompt, so the
// bridge drops it (it's persisted for /explain by the run-event sink).
func (b *Bridge) TurnPrompt(string) {}

// ModelRateLimited is a live cockpit health cue with no host-protocol channel —
// dropped. The "Model rate-limited" reply still flows through the normal turn text.
func (b *Bridge) ModelRateLimited() {}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// StartExchange resets per-turn state and emits a zero-duration user turn
// (start+end at the same ts). Prompt text is NOT carried — Daintree originated it.
func (b *Bridge) StartExchange() {
	b.mu.Lock()
	b.interrupted = false
	b.activeTurnID = ""
	b.mu.Unlock()
	turnID := genID("turn")
	now := b.now()
	b.post(EvTurnStart{TurnID: turnID, Role: RoleUser, StartedAt: now})
	b.post(EvTurnEnd{TurnID: turnID, EndedAt: now})
}

// SettleTurn closes any dangling assistant turn (no-op if already closed). Called
// in the prompt/wake finally.
func (b *Bridge) SettleTurn(outcome TurnOutcomeClass) {
	if outcome == "" {
		outcome = OutcomeUnknown
	}
	b.closeTurn(outcome)
}

// Interrupt is the display side of an interrupt: latch interrupted, stop
// forwarding the in-flight turn, close it "agent-stuck". No-op without an active
// turn.
func (b *Bridge) Interrupt() {
	b.mu.Lock()
	if b.activeTurnID == "" {
		b.mu.Unlock()
		return
	}
	b.interrupted = true
	b.mu.Unlock()
	b.closeTurn(OutcomeAgentStuck)
}

// closeTurn nulls the active turn and emits turn:end. Idempotent (guarded by the
// active turn id) so AssistantEnd + AssistantCancelled can both fire — the first
// closes, the second no-ops.
func (b *Bridge) closeTurn(outcome TurnOutcomeClass) {
	b.mu.Lock()
	turnID := b.activeTurnID
	if turnID == "" {
		b.mu.Unlock()
		return
	}
	b.activeTurnID = ""
	now := b.now()
	b.mu.Unlock()
	b.post(EvTurnEnd{TurnID: turnID, EndedAt: now, Outcome: outcome})
}

// Confirm is the tool-confirm hook: mint an approval, emit approval:requested,
// and block until decided / timed out / drained. Returns true iff approved. Runs
// on the agent's dispatch goroutine; the command loop resolves via decide/drain.
func (b *Bridge) Confirm(ctx context.Context, toolName, summary string) bool {
	approvalID := genID("apr")
	resolve := make(chan ConfirmationDecision, 1)

	b.mu.Lock()
	turnID := b.activeTurnID
	now := b.now()
	pa := &pendingApproval{resolve: resolve}
	if b.approvalTimeoutMs > 0 {
		// Auto-timeout an unanswered confirm. Go timers don't keep the process
		// alive, so no unref() analog is needed.
		pa.timer = time.AfterFunc(time.Duration(b.approvalTimeoutMs)*time.Millisecond, func() {
			b.ResolveApproval(approvalID, DecisionTimeout)
		})
	}
	b.pendingApprovals[approvalID] = pa
	b.mu.Unlock()

	b.post(EvApprovalRequested{
		ApprovalID:  approvalID,
		ToolID:      toolName,
		Summary:     summary,
		RequestedAt: now,
		TurnID:      turnID,
	})

	// Block on the decision. ctx cancellation (turn abort) also frees the dispatch:
	// resolve as rejected so a parked dispatch returns USER_DECLINED.
	select {
	case d := <-resolve:
		return d == DecisionApproved
	case <-ctx.Done():
		b.ResolveApproval(approvalID, DecisionRejected)
		return false
	}
}

// ResolveApproval settles an outstanding approval (decide / timeout / drain).
// No-op if not pending. Emits approval:decided and unblocks the Confirm caller.
func (b *Bridge) ResolveApproval(approvalID string, decision ConfirmationDecision) {
	b.mu.Lock()
	pa, ok := b.pendingApprovals[approvalID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.pendingApprovals, approvalID)
	if pa.timer != nil {
		pa.timer.Stop()
	}
	now := b.now()
	b.mu.Unlock()

	b.post(EvApprovalDecided{ApprovalID: approvalID, Decision: decision, DecidedAt: now})
	// Non-blocking: the channel is buffered(1) and only ever receives once.
	select {
	case pa.resolve <- decision:
	default:
	}
}

// SettlePendingApprovals rejects (default) every outstanding approval. Used on
// interrupt + teardown drain so a parked dispatch never strands busy.
func (b *Bridge) SettlePendingApprovals(decision ConfirmationDecision) {
	if decision == "" {
		decision = DecisionRejected
	}
	b.mu.Lock()
	ids := make([]string, 0, len(b.pendingApprovals))
	for id := range b.pendingApprovals {
		ids = append(ids, id)
	}
	b.mu.Unlock()
	for _, id := range ids {
		b.ResolveApproval(id, decision)
	}
}

// isDanger: any non-read risk class = danger. Unknown tool → not danger.
func (b *Bridge) isDanger(toolName string) bool {
	risk, ok := b.riskOf(toolName)
	return ok && risk != domain.RiskRead
}

// ---------------------------------------------------------------------------
// Audit mapping + redaction
// ---------------------------------------------------------------------------

// errorCodeToResult maps a tool error code → AuditResult.
var errorCodeToResult = map[string]AuditResult{
	"CONFIRMATION_REQUIRED": AuditConfirmationPending,
	"UNAUTHORIZED":          AuditUnauthorized,
	"TIER_REJECTED":         AuditUnauthorized,
	"FORBIDDEN":             AuditUnauthorized,
	"RATE_LIMITED":          AuditRateLimited,
	"DEDUP":                 AuditDedup,
	"DUPLICATE":             AuditDedup,
	"COLLISION":             AuditCollision,
}

// resultToAudit maps a ToolResult to {result, severity, errorCode}. Success → no
// errorCode. Otherwise the error code drives the result class (unknown → error).
func resultToAudit(res domain.ToolResult) (AuditResult, AuditSeverity, string) {
	if res.Ok {
		return AuditSuccess, SeverityInfo, ""
	}
	code := ""
	if res.Error != nil {
		code = res.Error.Code
	}
	result, ok := errorCodeToResult[code]
	if !ok {
		result = AuditError
	}
	return result, SeverityForResult(result), code
}

// trimNonEmpty reports whether s has any non-whitespace content (TS content.trim()).
func trimNonEmpty(s string) bool {
	for _, r := range s {
		if r != ' ' && r != '\t' && r != '\n' && r != '\r' && r != '\v' && r != '\f' {
			return true
		}
	}
	return false
}

// strLen returns the JS String.length (UTF-16 code-unit count) of s. The TS
// redactArgs collapses on `.length > 80`, which is UTF-16 units; we match it
// exactly so the "<string: N chars>" count and the 80-char cap agree with
// Daintree's MCP-audit redaction for non-BMP input. (Rune count would diverge on
// astral-plane characters; UTF-16 parity is the deliberate choice.)
func strLen(s string) int { return len(utf16.Encode([]rune(s))) }

// redactArgs builds a single-level, redacted JSON view of tool args for the
// timeline. Raw values may carry file/terminal/prompt content and must never
// cross verbatim. args is the raw JSON arguments string the model emitted.
//
// Quirk preserved: a top-level JSON array is iterated by key (Object.entries),
// so its indices become string keys and it serializes as an object {"0":…}, not
// an array.
func redactArgs(rawArgs string) string {
	if rawArgs == "" {
		return ""
	}
	var v any
	if err := json.Unmarshal([]byte(rawArgs), &v); err != nil {
		// Not valid JSON: treat the raw string itself as the top-level string value.
		return redactTopLevelString(rawArgs)
	}
	switch val := v.(type) {
	case nil:
		return "" // null → empty string
	case string:
		return redactTopLevelString(val)
	case map[string]any:
		return redactObject(val)
	case []any:
		// Object.entries on an array → string-keyed object {"0":…}.
		obj := make(map[string]any, len(val))
		for i, item := range val {
			obj[fmt.Sprintf("%d", i)] = redactValue(item)
		}
		return marshalRedacted(obj)
	default:
		// number / bool → JSON.stringify(v).
		b, err := json.Marshal(val)
		if err != nil {
			return "<unserializable>"
		}
		return string(b)
	}
}

// redactTopLevelString: >80 → "<string: N chars>" else the quoted string. Uses
// no-HTML-escape marshaling so a short string containing <, >, & is emitted
// verbatim rather than escaped.
func redactTopLevelString(s string) string {
	if strLen(s) > argsSummaryMaxString {
		return fmt.Sprintf("<string: %d chars>", strLen(s))
	}
	if q, ok := jsonNoEscape(s); ok {
		return q
	}
	return "<unserializable>"
}

func redactObject(m map[string]any) string {
	out := make(map[string]any, len(m))
	for k, item := range m {
		out[k] = redactValue(item)
	}
	return marshalRedacted(out)
}

// redactValue collapses a per-key value: long string → marker, short string
// as-is, array → "<array>", object → "<object>", else (number/bool/null) as-is.
func redactValue(v any) any {
	switch val := v.(type) {
	case string:
		if strLen(val) > argsSummaryMaxString {
			return fmt.Sprintf("<string: %d chars>", strLen(val))
		}
		return val
	case []any:
		return "<array>"
	case map[string]any:
		return "<object>"
	default:
		// number / bool / null pass through as-is.
		return val
	}
}

// jsonNoEscape marshals v WITHOUT Go's default HTML escaping of <, >, &. The TS
// redactArgs uses JSON.stringify, which leaves "<string: N chars>"/"<array>"
// literal; Go would emit <… and diverge from the wire string. The trailing
// newline encoder.Encode appends is trimmed.
func jsonNoEscape(v any) (string, bool) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return strings.TrimRight(buf.String(), "\n"), true
}

func marshalRedacted(v any) string {
	if s, ok := jsonNoEscape(v); ok {
		return s
	}
	return "<unserializable>"
}
