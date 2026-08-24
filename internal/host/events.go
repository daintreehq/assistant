package host

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/domain"
)

// HostEvent is the outbound (host → Daintree) wire union. Every event carries a
// "type" discriminator, a "sessionId", and a monotonic "seq"; the Go encoder writes
// each as one NDJSON line on stdout. Concrete event structs implement encode(),
// which injects all three so callers never repeat them. Field names are verbatim wire.
type HostEvent interface {
	// encode returns the full NDJSON object (type + sessionId + seq) for stdout.
	encode(sessionID string, seq uint64) ([]byte, error)
}

// shrinkable is implemented by an event whose payload can be reduced to fit the frame
// cap. See transport.stampAndEnqueue: without it, an oversize frame is simply not sent.
//
// Only events with an unbounded field need it, and only turn:end genuinely matters —
// it is the TERMINAL frame, so a host that never receives it never learns the turn
// ended and waits forever on a turn that is over.
type shrinkable interface {
	// shrink returns a copy whose payload fits within budget bytes of content, and
	// whether shrinking was possible at all.
	shrink(budget int) (HostEvent, bool)
}

// encodeSeq is the transport's entry point: it stamps seq onto ev. Kept as a free
// function so the transport does not need to know the concrete event type.
func encodeSeq(ev HostEvent, sessionID string, seq uint64) ([]byte, error) {
	return ev.encode(sessionID, seq)
}

// marshalEvent serializes a flat map as one JSON object. Centralizes the "+type
// +sessionId +seq" injection so every event encodes identically.
//
// seq is monotonic from 1 across the whole session and is the v3 contract that makes
// a lost frame DETECTABLE: v2 dropped frames silently under load with no way for a
// consumer to notice, which is unusable for a rendered transcript. A consumer that
// sees a gap knows its transcript is incomplete and can say so instead of showing
// corrupted prose as if it were the answer.
func marshalEvent(typ, sessionID string, seq uint64, fields map[string]any) ([]byte, error) {
	obj := make(map[string]any, len(fields)+3)
	obj["type"] = typ
	obj["sessionId"] = sessionID
	obj["seq"] = seq
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
//
// Version is the ENGINE build (`daintree-assistant --version`), distinct from
// ProtocolVersion. A host needs both and they answer different questions: the
// protocol version decides whether the two peers can talk at all, while the build
// version is what a "your assistant is out of date" prompt, a bug report, and any
// feature gate finer-grained than a protocol bump have to key on. Daintree previously
// had to shell out to `--version` separately to learn it.
//
// AutoApprove reports that this session runs mutating tools with no confirmation
// (DAINTREE_ASSISTANT_AUTO_APPROVE). It was previously mentioned only on stderr, which
// a protocol-only consumer never reads — so a host had no way to show that approvals
// are switched off, which is exactly the state a user most needs to see.
// The masthead fields below carry what the cockpit's own masthead stated, already
// resolved (see masthead.go). They exist for the same reason AutoApprove does: they are
// session facts that a protocol-only consumer has no other way to learn. Tier and
// TierGloss say what this session is permitted to do; Backend says which endpoint
// answers a turn, and is the ONLY readout of that since sign-in went away; Routing says
// what privacy/selection policy was requested; LogFile says where the trace goes, which
// is unanswerable from outside because the engine picks the filename. Empty means "the
// default, which needs no announcement" — except LogFile, where empty means logging is
// off.
type EvReady struct {
	ProtocolVersion  int
	ResumedSessionID string
	Version          string
	AutoApprove      bool
	Tier             string
	TierGloss        string
	Backend          string
	Routing          string
	LogFile          string
	// Commands is the engine's command catalog, so an embedded surface can offer the
	// SAME set the CLI documents. Sent once, at ready: a host that hardcoded its own
	// list would drift the first time a command was added or renamed, and would offer
	// the user something the engine refuses.
	Commands []CommandMeta
}

// CommandMeta is one entry in the command catalog.
type CommandMeta struct {
	Name    string
	Syntax  string
	Palette string
}

func (e EvReady) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{
		"protocolVersion": e.ProtocolVersion,
		"autoApprove":     e.AutoApprove,
	}
	// Omitted when empty rather than sent as "": an absent key is how a consumer tells
	// "the default" from "a value that happens to be blank", and it keeps the frame
	// small for the common case where every one of these is the default.
	for k, v := range map[string]string{
		"tier":      e.Tier,
		"tierGloss": e.TierGloss,
		"backend":   e.Backend,
		"routing":   e.Routing,
		"logFile":   e.LogFile,
	} {
		if v != "" {
			f[k] = v
		}
	}
	if len(e.Commands) > 0 {
		cmds := make([]map[string]any, 0, len(e.Commands))
		for _, c := range e.Commands {
			cmds = append(cmds, map[string]any{
				"name":    c.Name,
				"syntax":  c.Syntax,
				"palette": c.Palette,
			})
		}
		f["commands"] = cmds
	}
	if e.ResumedSessionID != "" {
		f["resumedSessionId"] = e.ResumedSessionID
	}
	if e.Version != "" {
		f["version"] = e.Version
	}
	return marshalEvent("host:ready", sid, seq, f)
}

// EvTurnStart — turn:start.
type EvTurnStart struct {
	TurnID    string
	Role      TurnRole
	StartedAt int64
	// Wake marks a turn the assistant started on its OWN, from an attention burst
	// rather than from something the user sent.
	//
	// A host needs it because such a turn is not interruptible: `interrupt` aborts
	// command turns only, since a wake has already claimed its attention events and
	// aborting would strand them. Without this the host offers a Stop control that
	// cannot do anything.
	Wake bool
}

func (e EvTurnStart) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{
		"turnId":    e.TurnID,
		"role":      string(e.Role),
		"startedAt": e.StartedAt,
	}
	if e.Wake {
		f["wake"] = true
	}
	return marshalEvent("turn:start", sid, seq, f)
}

// EvTurnToken — turn:token.
type EvTurnToken struct {
	TurnID string
	Chunk  string
}

func (e EvTurnToken) encode(sid string, seq uint64) ([]byte, error) {
	return marshalEvent("turn:token", sid, seq, map[string]any{
		"turnId": e.TurnID,
		"chunk":  e.Chunk,
	})
}

// EvTurnEnd — turn:end. Outcome optional.
//
// Content is the AUTHORITATIVE final text of the turn, and it is what makes the
// transcript repairable. A consumer accumulates turn:token for liveness, then
// REPLACES its buffer with this on turn:end — so a token frame lost to a wedged
// stdout, a mid-stream reconnect, or a consumer's own dropped update self-heals
// instead of leaving mangled prose on screen forever. v2 had no such field: its
// only authority was the token stream it could not guarantee.
//
// It is omitted (not "") when the turn produced no visible text at all — a cancel
// before first token, or a tool-only round — so a host can tell "nothing was said"
// from "the answer was empty".
type EvTurnEnd struct {
	TurnID     string
	EndedAt    int64
	Outcome    TurnOutcomeClass
	Content    string
	HasContent bool
}

func (e EvTurnEnd) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"turnId": e.TurnID, "endedAt": e.EndedAt}
	if e.Outcome != "" {
		f["outcome"] = string(e.Outcome)
	}
	if e.HasContent {
		f["content"] = e.Content
	}
	return marshalEvent("turn:end", sid, seq, f)
}

// shrink cuts the authoritative content down so the terminal frame still FITS.
//
// The alternative, which is what happened before, is that the frame is refused for being
// oversize and never sent at all — and turn:end is the frame that says the turn is over.
// A host that does not receive it shows a turn running forever, over a conversation that
// finished. Losing the tail of a very long answer is bad; losing the fact that the answer
// exists is worse, and it is unrecoverable without restarting the session.
//
// The loss is NOT silent: the marker is inside the rendered content, where the person
// reading the answer sees it. It goes in the content rather than a new field because the
// frame shape is a cross-repository contract — Daintree validates these lines and a field
// it does not know is a rejected frame, which would put us back to not delivering it.
func (e EvTurnEnd) shrink(budget int) (HostEvent, bool) {
	if !e.HasContent || len(e.Content) <= budget {
		return e, false
	}
	// budget 0 is the last resort: emit the frame with the content REMOVED rather than
	// not at all. HasContent stays true so the field is still present as an empty
	// string — a host can then tell "the answer did not fit" from "the turn said
	// nothing", which is the distinction HasContent exists to carry.
	if budget <= 0 {
		e.Content = truncationNote
		return e, true
	}
	room := budget - len(truncationNote)
	if room < 0 {
		room = 0
	}
	cut := room
	if cut > len(e.Content) {
		cut = len(e.Content)
	}
	// Land on a rune boundary: a byte-exact cut through a multi-byte character is
	// invalid UTF-8, and the JSON encoder would replace it rather than carry it.
	for cut > 0 && !utf8.RuneStart(e.Content[cut]) {
		cut--
	}
	e.Content = e.Content[:cut] + truncationNote
	return e, true
}

// truncationNote is what a reader sees where the rest of an over-large answer would be.
// It goes in the CONTENT rather than a new field because the frame shape is a
// cross-repository contract, and a field Daintree does not know is a rejected frame —
// which would put us back to not delivering the terminal frame at all.
const truncationNote = "\n\n[This answer was too large for one protocol frame and has been cut here. " +
	"The full text is in the run transcript.]"

// EvToolStarted — tool:started. turnId optional.
type EvToolStarted struct {
	ToolCallID  string
	ToolID      string
	ArgsSummary string
	StartedAt   int64
	TurnID      string
	Danger      bool
}

func (e EvToolStarted) encode(sid string, seq uint64) ([]byte, error) {
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
	return marshalEvent("tool:started", sid, seq, f)
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
	// render it as a finished success (the host shows it as a distinct yellow
	// pending state). Empty for every ordinary synchronous result.
	AsyncID string
	// AsyncTitle names the work an accepted async call handed off ("migrate the
	// schema in wt_db"), so a host can say WHAT is running in the background rather
	// than only that something is.
	AsyncTitle string
	// Summary is the tool's OWN human-readable line for what it did ("Pushed 3
	// commits to origin/main"). Engine-authored, never raw arguments, so it is safe to
	// carry across a UI boundary — and it is what the terminal cockpit showed instead
	// of a bare tool id. Without it a host can only display the identifier and hope
	// the user knows what it means.
	Summary string
	// ErrorMessage is the human sentence behind ErrorCode. A code alone tells a user
	// that something failed, not what.
	ErrorMessage string
}

func (e EvToolSettled) encode(sid string, seq uint64) ([]byte, error) {
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
	if e.AsyncTitle != "" {
		f["asyncTitle"] = e.AsyncTitle
	}
	if e.Summary != "" {
		f["summary"] = e.Summary
	}
	if e.ErrorMessage != "" {
		f["errorMessage"] = e.ErrorMessage
	}
	return marshalEvent("tool:settled", sid, seq, f)
}

// EvApprovalRequested — approval:requested. turnId optional. riskClass,
// consequence, and argsSummary are optional display context (parity with a local
// host approval); each is omitted from the wire object when empty.
// EvMcpStatus — mcp:status. Whether the Daintree control plane is reachable, and how
// many tools it is offering.
//
// Emitted at boot and again after any reconnect. Without it a host can only report
// that the ENGINE is up, which says nothing about whether it can actually do anything:
// a session that answers questions but cannot spawn an agent, while its status line
// reads "Connected", is the most misleading state this protocol can produce.
type EvMcpStatus struct {
	Connected bool
	// ToolCount is nil when the catalog has not been fetched yet.
	ToolCount *int
	// Error is the reason it is not connected, when there is one.
	Error string
}

func (e EvMcpStatus) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"connected": e.Connected}
	if e.ToolCount != nil {
		f["toolCount"] = *e.ToolCount
	}
	if e.Error != "" {
		f["error"] = e.Error
	}
	return marshalEvent("mcp:status", sid, seq, f)
}

// EvCommandResult — command:result. The output of a slash command the host routed
// through the engine, as plain text.
//
// Commands are not conversation. Sending `/status` to the model as prose produces an
// answer about the WORD status, spends a turn doing it, and leaves the user believing
// they ran something. This event is how an embedded surface gets the same answer the
// REPL prints.
type EvCommandResult struct {
	Command string
	Text    string
	// Quit reports that the command asked the session to end (/quit, /exit).
	Quit bool
	// Unknown reports that the line looked like a command but names none that exists,
	// so the host can say so instead of silently doing nothing.
	Unknown bool
	// ConversationCleared is the AUTHORITATIVE outcome of /clear: true only when the
	// engine actually cleared. A host must gate its transcript reset on this and never
	// on the command text — see CommandOutcome.ConversationCleared for what happens
	// when it does not. Same name and JSON key as the --multi-turn surface's
	// domain.JsonCommandResultPayload, so the two protocols cannot drift.
	ConversationCleared bool
	TurnID              string
}

func (e EvCommandResult) encode(sid string, seq uint64) ([]byte, error) {
	// ALWAYS present, unlike the omit-when-false booleans below. This one gates a
	// destructive UI action, so an absent field has to mean "an engine too old to know"
	// and nothing else; leaving it out on the false path would make the safe answer and
	// the unknown answer identical on the wire.
	f := map[string]any{"command": e.Command, "text": e.Text, "conversationCleared": e.ConversationCleared}
	if e.Quit {
		f["quit"] = true
	}
	if e.Unknown {
		f["unknown"] = true
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("command:result", sid, seq, f)
}

type EvApprovalRequested struct {
	ApprovalID  string
	ToolID      string
	Summary     string
	RequestedAt int64
	TurnID      string
	RiskClass   domain.RiskClass
	Consequence string
	ArgsSummary string
	// NeedsTypedConfirm is the safety layer's OWN verdict that this action is
	// irreversible and must not be approvable by a single click — the host is expected
	// to demand a typed phrase instead.
	//
	// It is carried explicitly rather than left for the host to re-derive from
	// RiskClass. A host that reimplements "which risk classes are irreversible" has
	// forked a security rule into a second codebase, where it can drift silently and
	// in the permissive direction. safety.NeedsTypedConfirm stays the single source of
	// truth; this field is its answer.
	NeedsTypedConfirm bool
	// Rememberable is the engine's verdict that this risk class MAY be added to a
	// session "don't ask again" list. The highest classes (git, system) never can.
	//
	// Carried rather than left for the host to re-derive, for the same reason
	// NeedsTypedConfirm is: a host that reimplements "which risks are safe to remember"
	// has forked a security rule into a second codebase, where it can drift silently
	// and in the permissive direction.
	Rememberable bool
	// ToolKey is the effective identity the gates were applied to — a composite id for
	// a dynamic tool. A host that remembers an approval must key on THIS, not on
	// ToolID: two different actions can present the same display name, and a standing
	// approval given for one would otherwise cover the other.
	ToolKey string
}

func (e EvApprovalRequested) encode(sid string, seq uint64) ([]byte, error) {
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
	// Always present, never omitted-when-false: a host must be able to tell "this
	// action does not need typed confirmation" from "this peer is too old to say".
	f["needsTypedConfirm"] = e.NeedsTypedConfirm
	if e.Rememberable {
		f["rememberable"] = true
	}
	if e.ToolKey != "" {
		f["toolKey"] = e.ToolKey
	}
	return marshalEvent("approval:requested", sid, seq, f)
}

// EvApprovalDecided — approval:decided.
type EvApprovalDecided struct {
	ApprovalID string
	Decision   ConfirmationDecision
	DecidedAt  int64
}

func (e EvApprovalDecided) encode(sid string, seq uint64) ([]byte, error) {
	return marshalEvent("approval:decided", sid, seq, map[string]any{
		"approvalId": e.ApprovalID,
		"decision":   string(e.Decision),
		"decidedAt":  e.DecidedAt,
	})
}

// EvQuestionRequested — question:requested. The model needs a finite user decision
// (user.askMultipleChoice) and the turn is BLOCKED until the host answers.
//
// It is a separate event from approval:requested because the two decisions are not the
// same shape. An approval is "may I do this?" and has one safe default — no. A question
// is "which of these did you mean?" and has no safe default at all: inventing one
// answers on the user's behalf. A host that had only the approval frames would have to
// render a yes/no sheet for a question with four answers, or the runtime would fall back
// to prose and end the turn — which is exactly what the headless surfaces did before
// this event existed.
type EvQuestionRequested struct {
	QuestionID  string
	Question    string
	Options     []QuestionOption
	Default     int
	RequestedAt int64
	TurnID      string
	ToolCallID  string
}

// QuestionOption is one labelled choice. The label (A, B, C…) is assigned by the CLI,
// never by the model, so the model cannot collide with or misspell them.
type QuestionOption struct {
	Label string `json:"label"`
	Text  string `json:"text"`
}

func (e EvQuestionRequested) encode(sid string, seq uint64) ([]byte, error) {
	opts := make([]map[string]any, 0, len(e.Options))
	for _, o := range e.Options {
		opts = append(opts, map[string]any{"label": o.Label, "text": o.Text})
	}
	f := map[string]any{
		"questionId":  e.QuestionID,
		"question":    e.Question,
		"options":     opts,
		"default":     e.Default,
		"requestedAt": e.RequestedAt,
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	if e.ToolCallID != "" {
		f["toolCallId"] = e.ToolCallID
	}
	return marshalEvent("question:requested", sid, seq, f)
}

// EvQuestionAnswered — question:answered. Mirrors approval:decided so a host can
// reconcile its own sheet against what the runtime actually recorded.
//
// A negative choiceIndex means the question was dismissed without an answer — the tool
// call is cancelled, not answered. Reported rather than inferred, so a host cannot
// mistake "the user closed the sheet" for "the user picked the first option".
type EvQuestionAnswered struct {
	QuestionID  string
	ChoiceIndex int
	Label       string
	Text        string
	AnsweredAt  int64
	Cancelled   bool
}

func (e EvQuestionAnswered) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{
		"questionId":  e.QuestionID,
		"choiceIndex": e.ChoiceIndex,
		"answeredAt":  e.AnsweredAt,
		"cancelled":   e.Cancelled,
	}
	if e.Label != "" {
		f["label"] = e.Label
	}
	if e.Text != "" {
		f["text"] = e.Text
	}
	return marshalEvent("question:answered", sid, seq, f)
}

// EvError — host:error.
type EvError struct {
	Code    string
	Message string
}

func (e EvError) encode(sid string, seq uint64) ([]byte, error) {
	return marshalEvent("host:error", sid, seq, map[string]any{
		"code":    e.Code,
		"message": e.Message,
	})
}

// EvShutdown — host:shutdown. resumeSessionId optional. Emitted FIRST in teardown.
type EvShutdown struct {
	Reason          HostShutdownReason
	ResumeSessionID string
}

func (e EvShutdown) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"reason": string(e.Reason)}
	if e.ResumeSessionID != "" {
		f["resumeSessionId"] = e.ResumeSessionID
	}
	return marshalEvent("host:shutdown", sid, seq, f)
}

// ---------------------------------------------------------------------------
// Protocol v3 additions.
//
// Every event below carries information the RUNTIME already produced and the v2
// bridge threw away, because a Bubble Tea cockpit rendered it locally and the
// parent only needed enough to draw an activity strip. Daintree now renders the
// whole conversation, so anything the runtime knows and a reader would want has to
// reach the wire. None of this is new instrumentation — it is forwarding.
// ---------------------------------------------------------------------------

// EvTurnPhase — turn:phase. The explicit run lifecycle (domain.RunPhase), which is
// how a consumer shows liveness WITHOUT inferring it from "is the text still empty".
// v2 dropped this outright ("live-only UI vocabulary with no host-protocol channel").
type EvTurnPhase struct {
	TurnID string
	Phase  string
	// Wake marks a phase belonging to a turn the assistant started ITSELF.
	//
	// Carried here as well as on turn:start because a wake emits its first phase
	// BEFORE the turn opens (the session reports "analyzing" while it decides what to
	// do). A host that learned this only from turn:start therefore had a window where
	// it knew work was happening but not that Stop could not reach it, and offered the
	// control anyway.
	Wake bool
}

func (e EvTurnPhase) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"phase": e.Phase}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	if e.Wake {
		f["wake"] = true
	}
	return marshalEvent("turn:phase", sid, seq, f)
}

// EvTurnReasoning — turn:reasoning. The model's thinking for the round, forwarded
// once at turn end (the runtime receives it as a whole, not as a stream). Separate
// from turn:token so a host can collapse it behind a disclosure rather than mixing
// it into the answer.
type EvTurnReasoning struct {
	TurnID string
	Text   string
}

func (e EvTurnReasoning) encode(sid string, seq uint64) ([]byte, error) {
	return marshalEvent("turn:reasoning", sid, seq, map[string]any{
		"turnId": e.TurnID,
		"text":   e.Text,
	})
}

// EvInterjectRetracted — interject:retracted. Answers an interject:retract command
// with the text taken back, so the host can put it where the user can edit it.
//
// The cockpit bound this to Escape on an empty composer: a follow-up typed mid-turn is
// BUFFERED, not sent, until the running turn folds it in at its next tool boundary, so
// there is a real window in which it can still be pulled back. `retracted` is false when
// that window has closed — already folded in, or nothing was typed — and the host must
// then leave the running turn alone rather than pretending it took something back.
type EvInterjectRetracted struct {
	Retracted bool
	Text      string
}

func (e EvInterjectRetracted) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"retracted": e.Retracted}
	if e.Retracted {
		// Echoed verbatim: it is the user's own text coming back to their composer, and
		// redacting it would hand them a masked version of what they just typed.
		f["text"] = e.Text
	}
	return marshalEvent("interject:retracted", sid, seq, f)
}

// EvTurnInterjection — turn:interjection. A message the user typed WHILE the turn
// was running, emitted at the moment the loop folds it into history. The host sent
// the text, but only the runtime knows WHEN it landed, and a transcript that shows
// the steer in the wrong place misrepresents what the model actually saw.
type EvTurnInterjection struct {
	TurnID string
	Text   string
}

func (e EvTurnInterjection) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"text": e.Text}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("turn:interjection", sid, seq, f)
}

// BatchedCall is one entry of a tool:batch announcement.
type BatchedCall struct {
	ToolCallID  string `json:"toolCallId"`
	ToolID      string `json:"toolId"`
	ArgsSummary string `json:"argsSummary"`
	Danger      bool   `json:"danger"`
	// Verb is the settled human label ("Read", "Delegated"); ActiveVerb is its
	// in-progress form ("Delegating") for the tools that visibly block for many
	// seconds, empty when the settled label reads correctly either way. Target is the
	// verb's object, pulled from the raw args and redacted. All three are empty for a
	// tool the presentation table does not know, which is the signal to fall back to
	// the raw tool id — never to invent a label. See presentation.go.
	Verb       string `json:"verb,omitempty"`
	ActiveVerb string `json:"activeVerb,omitempty"`
	Target     string `json:"target,omitempty"`
}

// EvToolBatch — tool:batch. The WHOLE batch announced as queued before sequential
// dispatch begins. Without it a host can only reveal calls one at a time as each
// starts, which reads as "the assistant keeps thinking of new things to do" instead
// of "it planned five steps and is on the second".
type EvToolBatch struct {
	TurnID string
	Calls  []BatchedCall
}

func (e EvToolBatch) encode(sid string, seq uint64) ([]byte, error) {
	calls := e.Calls
	if calls == nil {
		calls = []BatchedCall{}
	}
	f := map[string]any{"calls": calls}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("tool:batch", sid, seq, f)
}

// EvToolState — tool:state. Promotes one announced call through
// queued → active → waiting → done/failed. "waiting" means awaiting approval and is
// the one a host must render distinctly: it is blocked on the USER, not on the tool.
type EvToolState struct {
	ToolCallID string
	State      string
	TurnID     string
}

func (e EvToolState) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"toolCallId": e.ToolCallID, "state": e.State}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("tool:state", sid, seq, f)
}

// EvToolProgress — tool:progress. An in-tool substep ("launching terminal") so a
// long call does not look frozen. Message is "" when a beat carries only liveness;
// a host keeps the prior message in that case rather than blanking the row.
type EvToolProgress struct {
	ToolCallID string
	Message    string
	TurnID     string
}

func (e EvToolProgress) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"toolCallId": e.ToolCallID, "message": e.Message}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("tool:progress", sid, seq, f)
}

// EvUsage — usage. Per-round token accounting, including the two fields that drive a
// context meter: ContextTokens against ContextWindow, and ContextThreshold (the
// auto-compact trigger). Pointer fields are omitted when the provider reported
// nothing, so a host shows "no data" rather than a misleading zero.
type EvUsage struct {
	TurnID           string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CachedTokens     *int
	CacheHitRatio    *float64
	ContextTokens    int
	ContextThreshold int
	ContextWindow    int
}

func (e EvUsage) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{
		"promptTokens":     e.PromptTokens,
		"completionTokens": e.CompletionTokens,
		"totalTokens":      e.TotalTokens,
		"contextTokens":    e.ContextTokens,
		"contextThreshold": e.ContextThreshold,
		"contextWindow":    e.ContextWindow,
	}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	if e.CachedTokens != nil {
		f["cachedTokens"] = *e.CachedTokens
	}
	if e.CacheHitRatio != nil {
		f["cacheHitRatio"] = *e.CacheHitRatio
	}
	return marshalEvent("usage", sid, seq, f)
}

// EvCost — cost. What the session has spent, in the provider's own figures.
//
// Two rules the wire preserves rather than flattening, because getting either wrong
// under-reports spend while looking like a receipt: `complete:false` means Total is a
// FLOOR (a call ran whose cost could not be measured), and an ABSENT cost event means
// unknown, never free. A host renders an incomplete total as "≥ $x".
type EvCost struct {
	TurnID   string
	Total    float64
	Complete bool
}

func (e EvCost) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"total": e.Total, "complete": e.Complete}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("cost", sid, seq, f)
}

// EvNotice — notice. Non-fatal info/warning the runtime surfaces (a repeating tool
// failure, a pinned runbook the backend could not honour, a degraded MCP). v2 had no
// channel for these at all, so they reached nobody once the local renderer was gone.
type EvNotice struct {
	Level   string // "info" | "warning"
	Message string
	TurnID  string
}

func (e EvNotice) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{"level": e.Level, "message": e.Message}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("notice", sid, seq, f)
}

// EvModelRateLimited — model:rate-limited. The provider throttled us after the retry
// budget was exhausted. A health cue, not a turn failure: it clears on the next usage.
type EvModelRateLimited struct {
	TurnID string
}

func (e EvModelRateLimited) encode(sid string, seq uint64) ([]byte, error) {
	f := map[string]any{}
	if e.TurnID != "" {
		f["turnId"] = e.TurnID
	}
	return marshalEvent("model:rate-limited", sid, seq, f)
}
