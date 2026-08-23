package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// tools.go is the MCP surface. Every tool takes and returns a typed struct, so the
// SDK generates both the input and the output schema from the Go types — a caller
// discovers the exact argument shape instead of guessing it, which is the single
// biggest cause of tool misuse we see in this system's own logs.
//
// Tool names are dotted and namespaced (`daintree.session.open`) so they cannot collide
// with the other servers a client has connected, and read like the assistant's own tool
// vocabulary.

// defaultPollEvents bounds a poll window. The caller is a language model paying context
// for every event, so poll returns a WINDOW and says how much it withheld rather than
// dumping a whole orchestration turn.
const defaultPollEvents = 40

// The server-side ceilings on everything a caller can ask for by the page.
//
// Every one of these existed only as a DEFAULT before, which is not a bound: a caller
// choosing maxEvents:100000 got them, and a caller choosing nothing got a resource that
// returned a whole orchestration turn. The distinction matters because the caller is a
// model whose request size can be steered by whatever it has been reading, and because
// the response has to be held in memory and encoded before anyone can decide it was too
// big. A default protects the caller that does not think about it; a maximum protects the
// server from the caller that does.
//
// They are clamped rather than refused. A caller that asked for more than the server will
// give gets what the server will give, plus the count it did not — which is a usable
// answer, where an error is one more round trip to reach the same page.
const (
	// MaxPollEvents caps one poll or transcript page.
	MaxPollEvents = 500
	// MaxAttentionItems caps one inbox read. The inbox is durable and unbounded: a
	// project left running overnight accumulates every watcher finding and async
	// completion, and the first read after that must not be the whole night.
	MaxAttentionItems = 200
	// MaxPendingApprovals caps the parked confirmations reported on a run. A turn cannot
	// realistically park more than a handful — the first one blocks it — so a list past
	// this means something is wrong, and truncating it is better than paying for it.
	MaxPendingApprovals = 50
	// MaxApprovalPreviewBytes caps the redacted argument preview on ONE approval. Args
	// can be an entire file's worth of content, and a caller deciding whether to allow a
	// call needs the shape of them, not all of them.
	MaxApprovalPreviewBytes = 4096
	// MaxContentBytes caps the assistant content carried on a run response. The full
	// text is always available from the run transcript resource; what this stops is a
	// single poll response carrying a megabyte of prose the caller did not ask for.
	MaxContentBytes = 64 << 10
	// MaxEventTextBytes caps the text on ONE event.
	//
	// A page count alone does not bound a response: 500 events whose text is unbounded
	// is unbounded. Event text carries a round's whole assistant answer, a flushed
	// prose buffer, or a tool result summary, so the per-event cap and the page cap are
	// both load-bearing — neither is redundant.
	MaxEventTextBytes = 8 << 10
	// MaxResponseTextBytes is the AGGREGATE budget across one response's events. Even
	// with a per-event cap, 500 × 8 KB is four megabytes; this stops the page early and
	// reports the rest as withheld rather than encoding all of it to find out.
	MaxResponseTextBytes = 256 << 10
	// MaxAsyncOperations caps the background-handle ledger reported on a run. It grows
	// for the life of a long orchestration turn and is re-sent on every poll.
	MaxAsyncOperations = 100
)

// clampPageSize folds a caller-supplied page size onto the server's default and maximum.
func clampPageSize(requested, def, max int) int {
	if requested <= 0 {
		requested = def
	}
	if requested > max {
		return max
	}
	return requested
}

// truncateBytes shortens a string to at most max BYTES, cutting on a rune boundary so the
// result is never invalid UTF-8, and says how much it dropped.
//
// Bytes rather than runes because the thing being bounded is the response size, and a
// rune count says nothing about that.
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// The marker is counted INSIDE max, not appended past it. A ceiling that the
	// truncation itself pushes you over is not a ceiling, and the overshoot compounds:
	// every event in a page can carry one.
	//
	// Its length varies with the byte count it reports, so it is rendered first and the
	// text is cut to whatever room is left. A max too small to hold even the marker
	// yields the marker alone — the honest answer, since there is no room to say
	// anything else.
	marker := func(dropped int) string {
		return fmt.Sprintf("\n…[%d more bytes truncated by this server]", dropped)
	}
	room := max - len(marker(len(s)))
	if room < 0 {
		room = 0
	}
	cut := room
	if cut > len(s) {
		cut = len(s)
	}
	// Walk back off a partial rune. A byte-exact cut through a multi-byte character is
	// invalid UTF-8, which a JSON encoder silently replaces — so the caller sees
	// corruption where it should see truncation.
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	out := s[:cut] + marker(len(s)-cut)
	if len(out) <= max {
		return out
	}
	// The marker alone does not fit. Cut IT rather than let the bound be exceeded — a
	// caller with a very small budget still needs to see that something was dropped,
	// and the ceiling has to hold whatever it is set to.
	trimmed := marker(len(s))
	if len(trimmed) > max {
		trimmed = trimmed[:max]
		// Drop a trailing PARTIAL rune. Testing RuneStart on the last byte is the wrong
		// check here — for a complete multi-byte rune that byte is a continuation byte,
		// so it would strip valid characters, and for a partial one it can still leave
		// an incomplete sequence behind. Decoding the last rune answers the actual
		// question: is the tail a whole character?
		for len(trimmed) > 0 {
			if r, size := utf8.DecodeLastRuneInString(trimmed); r == utf8.RuneError && size <= 1 {
				trimmed = trimmed[:len(trimmed)-1]
				continue
			}
			break
		}
	}
	return trimmed
}

// boundedQuestion is the ONE projection of a parked question that leaves this server.
//
// Same rule as boundedApproval, and applied at every exit for the same reason: the
// question text and the option text both come from the MODEL, so both are externally
// sourced and neither is bounded at its source. A question listing and a run's
// pendingQuestions must not be two different answers about how big a response can get.
func boundedQuestion(pq PendingQuestion) PendingQuestion {
	pq.Question = truncateBytes(pq.Question, MaxEventTextBytes)
	if len(pq.Options) > maxQuestionOptions {
		pq.Options = pq.Options[:maxQuestionOptions]
	}
	// Copied before mutating: the slice is shared with the broker's retained question,
	// and truncating in place would edit what the next reader sees.
	opts := make([]QuestionOption, 0, len(pq.Options))
	for _, o := range pq.Options {
		o.Text = truncateBytes(o.Text, maxQuestionOptionBytes)
		opts = append(opts, o)
	}
	pq.Options = opts
	return pq
}

// boundedQuestions projects a list, capped, reporting how many it left out. Silent
// truncation would tell a caller to answer everything shown while another blocker waited.
func boundedQuestions(in []PendingQuestion, max int) (out []PendingQuestion, remaining int) {
	if len(in) > max {
		remaining = len(in) - max
		in = in[:max]
	}
	out = make([]PendingQuestion, 0, len(in))
	for _, pq := range in {
		out = append(out, boundedQuestion(pq))
	}
	return out, remaining
}

// maxQuestionOptions and maxQuestionOptionBytes bound a question's option list. The tool
// schema already asks for 2–26 labelled options, so these are the ceiling for a model
// that ignores it rather than a limit anyone should meet.
const (
	maxQuestionOptions     = 26
	maxQuestionOptionBytes = 2 << 10
)

// boundedApproval is the ONE projection of a parked approval that leaves this server.
//
// It exists because the caps were being applied in one place and bypassed in two: the
// run projection truncated the argument preview while daintree.approvals returned the
// whole object, and the elicitation message interpolated the raw string. A 10 MB set of
// arguments therefore reached a caller through either of the other two paths regardless
// of MaxApprovalPreviewBytes. A bound that only one of three exits honours is not a bound.
func boundedApproval(pa PendingApproval) PendingApproval {
	pa.Args = truncateBytes(pa.Args, MaxApprovalPreviewBytes)
	pa.Consequence = truncateBytes(pa.Consequence, MaxEventTextBytes)
	pa.Summary = truncateBytes(pa.Summary, MaxEventTextBytes)
	return pa
}

// boundedApprovals projects a list, capped, reporting how many it left out.
func boundedApprovals(in []PendingApproval, max int) (out []PendingApproval, remaining int) {
	if len(in) > max {
		remaining = len(in) - max
		in = in[:max]
	}
	out = make([]PendingApproval, 0, len(in))
	for _, pa := range in {
		out = append(out, boundedApproval(pa))
	}
	return out, remaining
}

// DefaultRunDeadline bounds a turn that names no timeoutMs of its own.
//
// Generous relative to a real orchestration turn, which spawns agents and waits on them —
// the bound exists to clear a WEDGED run, not to hurry a slow one. A run that is
// genuinely still working past this is doing something the caller should be told about
// rather than left waiting on indefinitely.
const DefaultRunDeadline = 30 * time.Minute

// MaxRunDeadline is the ceiling on a caller-supplied timeoutMs. The deadline is the only
// thing that reclaims a session from a run that will never finish, so a caller must not
// be able to stretch it to the point where it stops being one.
const MaxRunDeadline = 2 * time.Hour

// resolveRunDeadline folds a caller's timeoutMs onto this server's bounds.
//
// A non-positive value takes the default rather than meaning "no limit": "unbounded" is
// not a thing this surface offers, for the same reason the approval timeout has no off
// switch — the run holds the session, and with it the project's owner lease.
//
// The millisecond value is validated as an INTEGER before it becomes a Duration.
// time.Duration is int64 NANOSECONDS, so multiplying a large millisecond count overflows
// and wraps negative, which then reads as "non-positive" and silently becomes the
// default — a caller asking for an enormous timeout would get 30 minutes and no
// indication that its number had been thrown away.
func resolveRunDeadline(timeoutMs int) (time.Duration, error) {
	if timeoutMs < 0 {
		return 0, fmt.Errorf("timeoutMs must not be negative (got %d)", timeoutMs)
	}
	if timeoutMs == 0 {
		return DefaultRunDeadline, nil
	}
	const maxMs = int64(MaxRunDeadline / time.Millisecond)
	if int64(timeoutMs) > maxMs {
		return 0, fmt.Errorf(
			"timeoutMs %d exceeds this server's maximum of %d (%s); a run holds the session and its project lease, "+
				"so there is no unbounded option", timeoutMs, maxMs, MaxRunDeadline)
	}
	return time.Duration(timeoutMs) * time.Millisecond, nil
}

// resolveApprovalTimeout folds a caller's approvalTimeoutMs onto a Duration, validating
// the INTEGER first.
//
// Same overflow as the run deadline, with a worse ending. time.Duration is int64
// NANOSECONDS, so a large millisecond count wraps negative — and NewApprovals reads a
// non-positive timeout as "use the default". A caller asking for an enormous approval
// window would silently get five minutes, which is the one direction that matters here:
// the parked dispatch it thought it had an hour to answer is denied while it is still
// deciding.
func resolveApprovalTimeout(timeoutMs int) (time.Duration, error) {
	if timeoutMs < 0 {
		return 0, fmt.Errorf(
			"approvalTimeoutMs must not be negative (got %d) — the timeout is the only thing that bounds a parked approval",
			timeoutMs)
	}
	if timeoutMs == 0 {
		return 0, nil // NewApprovals substitutes DefaultApprovalTimeout.
	}
	const maxMs = int64(MaxApprovalTimeout / time.Millisecond)
	if int64(timeoutMs) > maxMs {
		return 0, fmt.Errorf(
			"approvalTimeoutMs %d exceeds this server's maximum of %d (%s); a parked approval holds the whole turn, "+
				"so there is no unbounded option", timeoutMs, maxMs, MaxApprovalTimeout)
	}
	return time.Duration(timeoutMs) * time.Millisecond, nil
}

// resolveQuestionTimeout folds a caller's questionTimeoutMs onto a Duration, validating
// the integer first — same overflow class as the approval timeout, where a large
// millisecond count wraps int64 nanoseconds negative and then reads as "use the default".
func resolveQuestionTimeout(timeoutMs int) (time.Duration, error) {
	if timeoutMs < 0 {
		return 0, fmt.Errorf(
			"questionTimeoutMs must not be negative (got %d) — the timeout is the only thing that bounds a parked question",
			timeoutMs)
	}
	if timeoutMs == 0 {
		return 0, nil // NewQuestions substitutes DefaultQuestionTimeout.
	}
	const maxMs = int64(MaxQuestionTimeout / time.Millisecond)
	if int64(timeoutMs) > maxMs {
		return 0, fmt.Errorf(
			"questionTimeoutMs %d exceeds this server's maximum of %d (%s); a parked question holds the whole turn, "+
				"so there is no unbounded option", timeoutMs, maxMs, MaxQuestionTimeout)
	}
	return time.Duration(timeoutMs) * time.Millisecond, nil
}

// maxBlockWait caps `ask` in block mode. It is deliberately short relative to a real
// orchestration turn: block mode is for quick questions, and anything longer must go
// async or the MCP client's own request timeout decides the outcome instead of us.
const maxBlockWait = 2 * time.Minute

// OpenInput is the argument shape of daintree.session.open.
type OpenInput struct {
	Project    string `json:"project,omitempty" jsonschema:"Absolute path to the project the assistant should operate on. Defaults to the server process's working directory."`
	BackendURL string `json:"backendUrl,omitempty" jsonschema:"Assistant backend endpoint, e.g. http://127.0.0.1:8473 for a local backend. Most servers PIN this at launch and reject an override - omit it to use the endpoint the operator configured."`
	APIKeyFile string `json:"apiKeyFile,omitempty" jsonschema:"Path to a file containing the API key. There is deliberately no way to pass the key inline, and most servers reject a session-chosen credential file outright - omit it to use the credential the operator gave this process."`
	Tier       string `json:"tier,omitempty" jsonschema:"Permission tier: supervisor, operator, or system."`
	McpURL     string `json:"mcpUrl,omitempty" jsonschema:"Daintree MCP endpoint. Most servers PIN this at launch and reject an override; omit it to inherit DAINTREE_MCP_URL from the server process. Without any MCP endpoint the assistant runs in degraded local mode and every orchestration tool reports 'not connected'."`
	// A PATH, never the token itself — exactly the rule apiKeyFile already follows, and
	// for a stronger reason. This bearer authorizes system-tier Daintree actions for its
	// whole validity window, and an inline argument is chosen by a model that may be
	// steered by repository text, echoed back by a prompt injection, logged by the MCP
	// client, and captured by traces outside this repository. The runtime already
	// stopped writing this token to its own debug log for the same reason; accepting it
	// as a model-callable string would put it right back in circulation.
	McpTokenFile string `json:"mcpTokenFile,omitempty" jsonschema:"Path to a file containing the Daintree MCP bearer token. There is deliberately no way to pass the token inline - it authorizes system-tier Daintree actions and must not travel through a model-callable argument. Most servers reject a session-chosen credential file outright; omit it to inherit DAINTREE_MCP_TOKEN from the server process. These tokens expire roughly 12 minutes after minting."`
	StateDir     string `json:"stateDir,omitempty" jsonschema:"State root - the conversation database, artifacts and the owner lease. Use a scratch path to isolate from the developer's real state."`
	LogDir       string `json:"logDir,omitempty" jsonschema:"Directory for the debug log."`
	// Project identity. This surface exists so a client that cannot restart the process
	// can repoint it, and identity is exactly the thing worth repointing: projectId
	// scopes the state directory into a per-project subdirectory, so it isolates a
	// session's database and lease as a side effect of naming the project.
	ProjectID string `json:"projectId,omitempty" jsonschema:"Daintree project id. It scopes the DEFAULT state root into a per-project subdirectory, so sessions naming different projects get separate databases and leases - but only when stateDir is left unset, since an explicit stateDir wins outright. To guarantee isolation, give each session its own stateDir."`
	WindowID  string `json:"windowId,omitempty" jsonschema:"Daintree window id. Identity only: it is reported by status and carried in config, and has no effect on where state is stored or on how a headless session behaves."`
	DebugLog  *bool  `json:"debugLog,omitempty" jsonschema:"Write a structured session trace to the log directory. Strongly recommended: it is the only way to diagnose a bad run."`
	// Approvals is a tri-state rather than a bool because the two obvious answers are
	// both wrong on their own: always approving lets the assistant push and run
	// commands unwatched, always declining means a session can never do the mutating
	// work it exists for.
	Approvals string `json:"approvals,omitempty" jsonschema:"How to answer a mutating tool. decline: skip it and carry on. delegate: park it for YOU - the calling agent - to settle with daintree.approve; this is delegation, not human authorization, and no human is guaranteed to see the request. auto: never ask. OMITTING this field INHERITS the launch configuration - it becomes auto on a server launched with auto-approve, and decline otherwise - so pass decline explicitly if you need fail-closed behaviour whatever the server was launched with. Choose delegate only if you will actually poll for approvals, since a parked call blocks the whole turn until it is answered or times out. The server's launch policy may refuse delegate or auto outright."`
	// ApprovalTimeoutMs bounds a parked approval so a forgotten one cannot pin the turn.
	ApprovalTimeoutMs int `json:"approvalTimeoutMs,omitempty" jsonschema:"How long a parked APPROVAL waits before it is denied, in milliseconds. Default 300000 (5 minutes). Only meaningful when approvals is delegate; questions have their own questionTimeoutMs."`
	// Questions is separate from Approvals on purpose. Answering a question authorises
	// nothing — it picks among options the assistant itself proposed — so a session that
	// declines every mutation can still answer planning questions, which is exactly the
	// shape a read-mostly harness wants and was impossible while the two were coupled.
	Questions string `json:"questions,omitempty" jsonschema:"How to answer the assistant's multiple-choice questions. decline (the default): the question is refused and the asking tool call fails, so the turn proceeds without it. delegate: park it for YOU to answer with daintree.question.answer. Independent of approvals - answering a question grants no authority, it only picks among options the assistant proposed, so declining every mutation and still answering questions is a valid combination."`
	// QuestionTimeoutMs bounds a parked question.
	QuestionTimeoutMs int `json:"questionTimeoutMs,omitempty" jsonschema:"How long a parked QUESTION waits before it is CANCELLED, in milliseconds. Default 300000 (5 minutes). Note cancelled, not defaulted: there is no safe default answer to 'which of these did you mean?'."`
	// Skills is the MCP twin of the CLI's repeatable --skill. The two headless surfaces
	// must not drift: a runbook you can pin from argv you must be able to pin here.
	//
	// A NON-NIL empty array is meaningful — it clears any process-level --skill this
	// server was launched with — which is why the merge below tests nil rather than
	// length. Omitting the field inherits those defaults.
	Skills []string `json:"skills,omitempty" jsonschema:"Backend skill ids to load on every turn of this session, whatever the backend's own selector picks. Run 'daintree-assistant --list-skills' to see the ids. When the backend advertises a catalog an unknown id fails this open rather than running unpinned; when it accepts pins but advertises no catalog, the open succeeds with a warning and the backend reports the bad id on the first turn. A backend that does not accept pins at all fails the open whatever the ids are. Pass an empty array to clear a server-level default."`
}

// SessionOutput describes an open session.
type SessionOutput struct {
	SessionID string       `json:"sessionId"`
	Facts     RuntimeFacts `json:"facts"`
	// State is open, closing, or close-failed. A closing session cannot take work; a
	// close-failed one may still be holding the project's owner lease.
	State string `json:"state" jsonschema:"open, closing, or close-failed. close-failed means the project lease may still be held."`
	Busy  bool   `json:"busy"`
	// CurrentRunID names the turn in flight, or "" when idle. It is here for RESPONSE
	// LOSS: an ask whose response never arrived leaves a caller knowing only that the
	// session is busy, and a retry returns the same unhelpful refusal. This is the fact
	// that turns "something is running" back into "poll this".
	CurrentRunID string `json:"currentRunId,omitempty" jsonschema:"The runId of the turn in flight, if any. Use it to recover a handle you lost."`
	// RecentRuns recovers a handle AFTER the run finished, which is the case a fast run
	// lands in: currentRunId is already empty, and a retried ask on an idle session is
	// accepted and simply does the work twice.
	RecentRuns []RunSummary `json:"recentRuns" jsonschema:"The most recent runs, newest first, with a short echo of each prompt. Use this to find a runId whose ask response you never received."`
	// CloseStartedAt is when teardown began on a closing/close-failed session, so a
	// caller can tell "just started" from "stuck".
	CloseStartedAt int64 `json:"closeStartedAt,omitempty"`
	// CloseError is why a close-failed session did not tear down.
	CloseError string `json:"closeError,omitempty"`
	// Warnings surface conditions that will silently ruin a run if unnoticed —
	// principally a degraded MCP connection and a binary that has been rebuilt since
	// this server started.
	Warnings []string `json:"warnings,omitempty"`
	// Server is the same structured process state session.list reports, carried here
	// too so a caller that only ever opens a session still learns the binary went
	// stale without having to parse the warning prose.
	Server ServerInfo `json:"server"`
}

// AskInput is the argument shape of daintree.ask.
type AskInput struct {
	SessionID string `json:"sessionId" jsonschema:"The session to run the turn in, from daintree.session.open."`
	Prompt    string `json:"prompt" jsonschema:"What to ask the assistant."`
	Wait      bool   `json:"wait,omitempty" jsonschema:"Block until the turn finishes instead of returning a handle. Only for quick questions - an orchestration turn takes minutes and will exceed the wait cap. Default false."`
	WaitMs    int    `json:"waitMs,omitempty" jsonschema:"How long to block when wait is true, in milliseconds. Capped at 120000. On expiry the run keeps going and you poll it."`
	// TimeoutMs bounds the RUN, which is a different thing from WaitMs bounding this
	// CALL. Letting the wait expire leaves the turn going; letting the deadline expire
	// cancels it.
	TimeoutMs int `json:"timeoutMs,omitempty" jsonschema:"How long this RUN may take before it is cancelled, in milliseconds. Not the same as waitMs, which only bounds how long this call blocks. Defaults to 1800000 (30 minutes) and is capped by the server; there is no unbounded option, because a run holds the session and its project lease."`
}

// RunOutput is the state of one run: its outcome so far plus a window of its events.
type RunOutput struct {
	RunID     string  `json:"runId"`
	SessionID string  `json:"sessionId"`
	Status    string  `json:"status" jsonschema:"running, success, error, or cancelled."`
	Events    []Event `json:"events"`
	// NextSeq is what to pass as sinceSeq on the next poll to get only new events.
	NextSeq int `json:"nextSeq"`
	// TotalEvents is the run's full retained length, taken in the SAME lock hold as the
	// events, so it cannot describe a longer run than the page it sits beside.
	TotalEvents int `json:"totalEvents"`
	// WithheldEvents is how many events past the window were dropped from this
	// response. Never silently truncate: a caller that cannot see this would read a
	// partial timeline as the whole one.
	WithheldEvents int                 `json:"withheldEvents,omitempty"`
	Content        string              `json:"content,omitempty" jsonschema:"The assistant's answer. Empty until the run settles."`
	Error          string              `json:"error,omitempty"`
	Stats          domain.JsonRunStats `json:"stats"`
	DurationMs     int                 `json:"durationMs"`
	// AsyncOperations is this run's ledger of background handles it accepted. It comes
	// from the run itself, not from the events in this poll window: the old field was
	// derived by scanning the window, so the handles vanished the moment the caller
	// advanced sinceSeq and were missed entirely when the accepting event fell outside
	// maxEvents. Status is "accepted", never "finished" — these settle OUTSIDE the run
	// and are reported through daintree.attention, never as a late event here.
	AsyncOperations []AsyncOperation `json:"asyncOperations"`
	// WithheldAsyncOperations and WithheldApprovals report what these two lists left
	// out. Neither is ever silently short: a caller reading a truncated async ledger
	// would conclude work it started had never been accepted.
	WithheldAsyncOperations int `json:"withheldAsyncOperations,omitempty"`
	WithheldApprovals       int `json:"withheldApprovals,omitempty"`
	WithheldQuestions       int `json:"withheldQuestions,omitempty"`
	// PendingQuestions are multiple-choice questions this run is PARKED on. Reported
	// beside the approvals for the same reason: a run showing either is not slow, it is
	// STOPPED, and a caller polling harder will never move it.
	PendingQuestions []PendingQuestion `json:"pendingQuestions,omitempty"`
	// PendingApprovals are confirmations this session is PARKED on. A run showing these
	// is not merely slow — it is stopped until they are answered, which is invisible
	// from `status` alone.
	PendingApprovals []PendingApproval `json:"pendingApprovals"`
	// NextAction spells out what to do with this response. It exists because the two
	// pathologies a polling surface invites — hammering poll in a tight loop, and
	// treating a still-running turn as a finished one — are both prevented by saying
	// the next step out loud rather than leaving a model to infer it from a status
	// string.
	NextAction string `json:"nextAction"`
}

// PollInput is the argument shape of daintree.poll.
type PollInput struct {
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId"`
	SinceSeq  int    `json:"sinceSeq,omitempty" jsonschema:"Return only events with seq >= this. Pass the previous response's nextSeq to read incrementally."`
	MaxEvents int    `json:"maxEvents,omitempty" jsonschema:"Cap on events in this response. Default 40, clamped to the server maximum of 500. Ask for more and you get 500 plus a withheldEvents count, not an error."`
	WaitMs    int    `json:"waitMs,omitempty" jsonschema:"Block up to this long for the run to settle before responding. Capped at 120000. Use it to avoid a tight polling loop."`
}

// ApprovalsOutput lists what a session is parked on.
type ApprovalsOutput struct {
	Mode string `json:"mode" jsonschema:"decline, delegate, or auto."`
	// DecisionAuthority repeats, at the list level, whose decision these are. A caller
	// reading a pending list needs to know it is looking at its OWN queue, not a
	// human's.
	DecisionAuthority string            `json:"decisionAuthority" jsonschema:"Who settles these approvals. caller-agent means you do - this is not a human safety boundary."`
	Pending           []PendingApproval `json:"pending"`
	Count             int               `json:"count"`
	// Remaining is parked approvals beyond this page. A turn stops at its FIRST
	// approval, so a list this long means something is wrong — but truncating it
	// silently would hide that rather than report it.
	Remaining int `json:"remaining,omitempty" jsonschema:"Parked approvals beyond this response. Answer these, then list again."`
	// Note explains an empty list when the reason is the MODE rather than the absence
	// of mutating work — otherwise "0 pending" reads as "nothing wanted approval".
	Note string `json:"note,omitempty"`
}

// ApproveInput answers one parked confirmation.
type ApproveInput struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId" jsonschema:"The id from daintree.approvals or from a run's pendingApprovals."`
	// RunID is the turn the caller believed it was approving for. Same reasoning as
	// InjectInput.RunID and one step sharper: over a slow pipe a decision written while
	// watching one turn can arrive after that turn ended, and releasing a mutating call
	// on the strength of a judgement made about DIFFERENT work is the one approval
	// outcome nobody wants. Approval ids are unique, so a stale one simply will not be
	// pending — but a caller that names the run gets told which turn it was actually
	// looking at instead of a bare "not found".
	RunID   string `json:"runId,omitempty" jsonschema:"The runId you were watching when you decided, from daintree.ask. Recommended: a decision that arrives after its turn ended is rejected rather than applied to a successor."`
	Approve bool   `json:"approve" jsonschema:"true to allow the tool call, false to refuse it. Refusing lets the turn continue without that call."`
}

// CloseOutput is what session.close reports.
//
// It carries a STATE rather than only a boolean because the three outcomes a caller must
// tell apart — I closed it, someone already had, it would not close — are not the same
// answer with a different flag. The third one means a project lease is still held.
type CloseOutput struct {
	// Acted is true only for the call that performed the teardown.
	Acted bool `json:"acted"`
	// State is "closed", "already-closed", "closing", or "close-failed".
	State   string `json:"state" jsonschema:"closed, already-closed, closing (teardown is still running; it stays listed until it finishes), or close-failed (the project lease may still be held and only restarting this server releases it)."`
	Message string `json:"message,omitempty"`
}

// QuestionsOutput lists what a session is parked on.
type QuestionsOutput struct {
	Mode string `json:"mode" jsonschema:"decline or delegate."`
	// DecisionAuthority repeats, at the list level, whose answers these are.
	DecisionAuthority string            `json:"decisionAuthority" jsonschema:"Who answers these. caller-agent means you do."`
	Pending           []PendingQuestion `json:"pending"`
	Count             int               `json:"count"`
	// Remaining is parked questions beyond this response. A turn stops at its FIRST
	// question, so a list this long means something is wrong — and truncating it
	// silently would hide that rather than report it.
	Remaining int    `json:"remaining,omitempty" jsonschema:"Parked questions beyond this response. Answer these, then list again."`
	Note      string `json:"note,omitempty"`
}

// AnswerQuestionInput answers one parked question.
type AnswerQuestionInput struct {
	SessionID  string `json:"sessionId"`
	QuestionID string `json:"questionId" jsonschema:"The id from daintree.questions or from a run's pendingQuestions."`
	// Choice is an INDEX into the question's options, not a label. An out-of-range value
	// cancels the call rather than being clamped — see the tool description.
	Choice int    `json:"choice" jsonschema:"The 0-based index of the option you are choosing, from the question's options array. An out-of-range index CANCELS the tool call rather than being clamped to the nearest option, because guessing an answer and then acting on it is worse than not answering."`
	RunID  string `json:"runId,omitempty" jsonschema:"The runId you were watching when you decided, from daintree.ask. Recommended: an answer that arrives after its turn ended is rejected rather than applied to a successor."`
}

// SessionRefInput is the shape of every tool that only needs a session.
type SessionRefInput struct {
	SessionID string `json:"sessionId"`
}

// InjectInput steers a running turn.
type InjectInput struct {
	SessionID string `json:"sessionId"`
	// RunID is the turn the caller MEANT to steer. Optional but strongly recommended:
	// without it a message written for one turn lands in whichever turn happens to be
	// current when the request arrives, which over a slow pipe is a different turn.
	RunID string `json:"runId,omitempty" jsonschema:"The runId this message is meant for, from daintree.ask. Strongly recommended - without it the message folds into whatever turn is running when it arrives, which may not be the one you were watching. A stale runId is rejected and names the live run."`
	Text  string `json:"text" jsonschema:"A message to fold into the RUNNING turn. The assistant picks it up at its next tool boundary."`
}

// RunRefInput addresses one run of a session, for calls that must not act on a turn the
// caller did not mean.
type RunRefInput struct {
	SessionID string `json:"sessionId"`
	RunID     string `json:"runId,omitempty" jsonschema:"The runId to act on, from daintree.ask. Strongly recommended - omitting it acts on whatever turn is running now. A stale runId is rejected and names the live run."`
}

// AttentionInput reads the project inbox.
type AttentionInput struct {
	SessionID string `json:"sessionId"`
	// Acknowledge defaults to FALSE. Acknowledging inside the read makes delivery
	// at-most-once: the rows are marked notified before this response is known to have
	// reached the caller, so a dropped connection loses them permanently — and an
	// attention row is precisely the report of background work that arrives nowhere
	// else. Peeking by default plus an explicit daintree.attention.ack makes it
	// at-least-once instead, and a duplicate is trivial for a caller to drop by id.
	//
	// It stays a pointer so "not supplied" remains distinguishable from an explicit
	// false, which keeps the field free to change meaning without silently flipping.
	Acknowledge *bool `json:"acknowledge,omitempty" jsonschema:"Mark the returned items delivered in the same call. Default false. Prefer leaving this unset and calling daintree.attention.ack once you have acted on the items - acknowledging inside the read loses them if this response never arrives."`
	// Limit bounds the page. The inbox is DURABLE and unbounded — a project left running
	// overnight accumulates every watcher finding and async completion — so the first
	// read after a long detachment could otherwise be the whole night in one response.
	Limit int `json:"limit,omitempty" jsonschema:"Maximum items to return. Default 50, clamped to the server maximum of 200. Items beyond the page stay in the inbox unacknowledged: acknowledge what you got, then read again. Safe to combine with acknowledge - only the items this page actually carried are marked delivered."`
}

// AttentionAckInput acknowledges inbox items the caller has actually processed.
type AttentionAckInput struct {
	SessionID string `json:"sessionId"`
	// EventIDs is deliberately required. An "ack everything" call would re-introduce
	// exactly the loss this split exists to prevent, for rows the caller never saw.
	EventIDs []string `json:"eventIds" jsonschema:"The ids of the attention items you have processed, from daintree.attention. Acknowledged items are not reported again."`
}

// AttentionAckOutput reports what an acknowledgement actually consumed.
type AttentionAckOutput struct {
	Acknowledged int `json:"acknowledged"`
	// Unknown lists ids that matched nothing - already acknowledged, or never real.
	// Reported rather than errored: a retry after an ambiguous transport failure is the
	// EXPECTED path here, and it must be idempotent.
	Unknown []string `json:"unknown,omitempty"`
	Message string   `json:"message,omitempty"`
}

// AttentionItem is one inbox entry, flattened for a reader. Evidence and the recommended
// actions are deliberately dropped: the caller is a model that will decide for itself,
// and the inbox's own recommendations are tuned for the assistant's prompt, not for a
// second agent reasoning over it.
type AttentionItem struct {
	ID       string `json:"id"`
	Severity string `json:"severity"`
	Source   string `json:"source"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	// Count is how many occurrences this row coalesces (the inbox dedupes by key), so a
	// reader can tell one stuck agent from twenty.
	Count int `json:"count"`
	// Target names what the event is about — a terminal or a worktree — when it has one.
	Target string `json:"target,omitempty"`
	// AsyncID links a completion back to the `asy_…` handle a run reported in
	// pendingAsync, which is what lets a caller match background work to the turn that
	// started it.
	AsyncID string `json:"asyncId,omitempty"`
}

// AttentionOutput is the inbox digest.
type AttentionOutput struct {
	Items []AttentionItem `json:"items"`
	// More says another page is waiting. A COUNT would need a second query over the
	// whole inbox; whether one more row exists costs one extra row on the query that
	// was already running. Never truncate silently: an inbox read is the only report
	// background work ever makes, and a caller that cannot see it was paged would read a
	// partial inbox as an empty one.
	More  bool `json:"more" jsonschema:"Another page of unacknowledged items is waiting. Acknowledge what you received, then read again."`
	Count int  `json:"count"`
	// Note says what the caller still owes: unacknowledged items are reported again.
	Note string `json:"note,omitempty"`
}

// ListOutput enumerates open sessions.
type ListOutput struct {
	Sessions []SessionOutput `json:"sessions"`
	// Server describes the process itself, including whether its binary is stale.
	Server ServerInfo `json:"server"`
}

// ActedOutput is the result of a fire-and-forget action.
type ActedOutput struct {
	Acted   bool   `json:"acted"`
	Message string `json:"message"`
}

// Register wires every tool onto an MCP server. lifetime is the SERVER's context: turns
// outlive the tool call that started them, so they must not be bound to the call.
func Register(s *mcp.Server, reg *Registry, info *BinaryInfo, lifetime context.Context) {
	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.session.open",
		Description: "Open an assistant session bound to a project. Returns a sessionId used by every other tool. " +
			"Session arguments NARROW what the server was launched with and can never widen it: a request above the server's " +
			"policy is refused rather than quietly downgraded, and endpoints and credentials are normally pinned at launch. " +
			"An OMITTED argument inherits the launch value rather than a fixed default — approvals most importantly, which " +
			"becomes auto on a server launched with auto-approve. Pass approvals:\"decline\" if you need that guaranteed. " +
			"Without an MCP URL and token the assistant runs in degraded local mode where it cannot see or drive terminals; " +
			"the token is inherited from the server process's environment, and is never passed inline.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OpenInput) (*mcp.CallToolResult, SessionOutput, error) {
		mode := ApprovalMode(strings.TrimSpace(in.Approvals))
		if mode != "" && !mode.Valid() {
			return nil, SessionOutput{}, fmt.Errorf(
				"unknown approvals mode %q — use \"decline\", \"delegate\" or \"auto\"", in.Approvals)
		}
		approvalTimeout, aerr := resolveApprovalTimeout(in.ApprovalTimeoutMs)
		if aerr != nil {
			return nil, SessionOutput{}, aerr
		}
		questionMode := QuestionMode(strings.TrimSpace(in.Questions))
		if questionMode != "" && !questionMode.Valid() {
			return nil, SessionOutput{}, fmt.Errorf(
				"unknown questions mode %q — use \"decline\" or \"delegate\"", in.Questions)
		}
		questionTimeout, qerr := resolveQuestionTimeout(in.QuestionTimeoutMs)
		if qerr != nil {
			return nil, SessionOutput{}, qerr
		}
		// Rejected here rather than trimmed away: an empty entry means the caller built
		// the array from something that came back blank, and silently dropping it opens a
		// session pinned to less than was asked for — the same silent-underrun --skill
		// exists to prevent.
		for i, id := range in.Skills {
			if strings.TrimSpace(id) == "" {
				return nil, SessionOutput{}, fmt.Errorf("skills[%d] is empty — remove it, or omit skills entirely to let the backend's selector choose", i)
			}
		}
		sess, err := reg.Open(ctx, OpenParams{
			Project: in.Project, BackendURL: in.BackendURL, APIKeyFile: in.APIKeyFile,
			Tier: in.Tier, McpURL: in.McpURL, McpTokenFile: in.McpTokenFile,
			StateDir: in.StateDir, LogDir: in.LogDir, DebugLog: in.DebugLog,
			ProjectID: in.ProjectID, WindowID: in.WindowID,
			Approvals:       mode,
			ApprovalTimeout: approvalTimeout,
			Questions:       questionMode,
			QuestionTimeout: questionTimeout,
			Skills:          in.Skills,
		})
		if err != nil {
			return nil, SessionOutput{}, err
		}
		return nil, describe(sess, info), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.session.list",
		Description: "List the open assistant sessions in this server, and report whether the assistant binary has been rebuilt " +
			"since the server started (in which case the server is running stale code and should be reconnected).",
	}, func(_ context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, ListOutput, error) {
		// A non-nil empty slice, not nil: nil marshals to `null`, and a caller checking
		// `sessions.length` on null gets a type error rather than "none open".
		out := ListOutput{Server: info.Snapshot(), Sessions: []SessionOutput{}}
		for _, sess := range reg.List() {
			out.Sessions = append(out.Sessions, describe(sess, info))
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.session.close",
		Description: "Close a session: cancel any running turn, tear down the runtime, release the project lease. " +
			"Always close a session you opened — the lease blocks other processes from opening the same project. " +
			"Safe to retry: closing an already-closed session reports acted:false rather than failing, so a lost response " +
			"costs a duplicate call and not a stuck lease.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, CloseOutput, error) {
		res, err := reg.Close(ctx, in.SessionID)
		out := CloseOutput{Acted: res.Acted, State: res.State, Message: res.Message}
		if err != nil {
			// IsError with a NIL Go error, not a returned error. The SDK short-circuits
			// on a handler error and never marshals the typed output — so returning one
			// would have thrown away the very fields that say a project lease is stuck,
			// leaving the caller a text blob to parse. Flag it as a failure AND hand back
			// the structured state.
			out.Message = err.Error()
			return &mcp.CallToolResult{IsError: true}, out, nil
		}
		if out.Message == "" {
			out.Message = "session closed and its project lease released"
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.ask",
		Description: "Ask the assistant something. Returns a runId IMMEDIATELY by default — an orchestration turn spawns agents " +
			"and waits on them, which takes minutes. Poll the runId with daintree.poll. Set wait:true only for a quick question.",
	}, func(ctx context.Context, req *mcp.CallToolRequest, in AskInput) (*mcp.CallToolResult, RunOutput, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return nil, RunOutput{}, errors.New("prompt is required")
		}
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		// Bind approvals to the client session that is driving this turn, so a parked
		// confirmation can be PUSHED to it rather than only waiting to be polled. Re-bound
		// per ask because that client is the one currently asking; harmless when the
		// client cannot elicit, since Elicit then errors and the approval stays parked.
		// Bound by BOTH the server lifetime and this session's. SetNotify hands back a
		// context cancelled when the broker is torn down, so an elicitation dies with
		// the session that raised it; the server lifetime covers the case where the
		// process stops first. Either alone leaves a stale question on someone's screen.
		approvalLife := sess.Approvals().SetNotify(nil)
		sess.Approvals().SetNotify(elicitNotifier(
			joinContexts(lifetime, approvalLife), req.Session, sess.Approvals(), sess.ApprovalTimeout()))
		deadline, derr := resolveRunDeadline(in.TimeoutMs)
		if derr != nil {
			return nil, RunOutput{}, derr
		}
		run, err := sess.Ask(lifetime, in.Prompt, deadline)
		if err != nil {
			return nil, RunOutput{}, err
		}
		if in.Wait {
			waitForSettle(ctx, run, in.WaitMs)
		}
		return nil, renderRunWith(run, 0, defaultPollEvents, sess.Approvals(), sess.Questions()), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.poll",
		Description: "Read a run's progress and outcome. Pass the previous response's nextSeq as sinceSeq to read incrementally " +
			"instead of re-reading the whole timeline. Use waitMs to wait for the run to settle rather than polling tightly.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in PollInput) (*mcp.CallToolResult, RunOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		run, err := sess.Run(in.RunID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		if in.WaitMs > 0 {
			// A run PARKED on an approval is the case a long poll must never sleep
			// through, and the revision alone cannot catch it: if the approval parked
			// BETWEEN two polls, this handler captures an already-advanced revision,
			// sinceSeq is at the event tail, and nothing further will ever be signalled
			// — so the caller would wait out its whole budget on a turn that is stopped,
			// possibly past the approval's own timeout. Checking for a pending approval
			// before waiting covers parked-before-capture, parked-between, and
			// parked-after alike.
			if !hasPendingDecision(run, sess.Approvals(), sess.Questions()) {
				// Revision captured BEFORE the wait: a change that lands between here
				// and the select must not be slept through either.
				waitForChange(ctx, run, in.SinceSeq, run.Revision(), in.WaitMs)
			}
		}
		return nil, renderRunWith(run, in.SinceSeq, clampPageSize(in.MaxEvents, defaultPollEvents, MaxPollEvents), sess.Approvals(), sess.Questions()), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.inject",
		Description: "Steer the RUNNING turn by folding a message into it; the assistant picks it up at its next tool boundary. " +
			"Use this rather than a second ask, which would be rejected — a session runs one turn at a time. " +
			"Pass the runId you meant to steer: without it the message lands in whatever turn is running when the call arrives.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in InjectInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, ActedOutput{}, errors.New("text is required")
		}
		switch err := sess.Inject(in.RunID, in.Text); {
		case err == nil:
			return nil, ActedOutput{Acted: true, Message: "folded into the running turn"}, nil
		case errors.Is(err, ErrNoActiveRun):
			return nil, ActedOutput{Acted: false, Message: "no turn is running; use daintree.ask instead"}, nil
		default:
			// A run mismatch is an ERROR, not an acted:false. Steering the wrong turn is
			// the failure this argument exists to prevent, so it must not read as a
			// benign no-op the caller can ignore.
			return nil, ActedOutput{}, err
		}
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.interrupt",
		Description: "Cancel the running turn. The session stays open and the conversation is kept, so you can ask again. " +
			"Pass the runId you meant to stop: without it this cancels whatever is running when the call lands.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in RunRefInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		switch err := sess.Interrupt(in.RunID); {
		case err == nil:
			return nil, ActedOutput{Acted: true, Message: "cancelling the running turn"}, nil
		case errors.Is(err, ErrNoActiveRun):
			// Idempotent: nothing to cancel is the state the caller wanted.
			return nil, ActedOutput{Acted: false, Message: "no turn is running"}, nil
		default:
			return nil, ActedOutput{}, err
		}
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.approvals",
		Description: "List the confirmations this session is PARKED on. A mutating tool (a terminal command, a git operation) " +
			"blocks the whole turn until it is answered, so a run that seems slow may simply be waiting here. " +
			"Only sessions opened with approvals:\"delegate\" ever park; the default declines and carries on. " +
			"These are YOUR decisions to make — no human sees them — so read risk, consequence and args before answering.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, ApprovalsOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ApprovalsOutput{}, err
		}
		mode := sess.Approvals().Mode()
		all := sess.Approvals().Pending()
		pending, remaining := boundedApprovals(all, MaxPendingApprovals)
		out := ApprovalsOutput{
			Mode:              string(mode),
			DecisionAuthority: mode.DecisionAuthority(),
			Pending:           pending,
			Count:             len(pending),
			Remaining:         remaining,
		}
		if out.Pending == nil {
			out.Pending = []PendingApproval{}
		}
		if len(pending) == 0 && mode != ApprovalDelegate {
			out.Note = "This session does not park approvals — mode is " + out.Mode +
				". Open a session with approvals:\"delegate\" if you want to settle each mutating call yourself, " +
				"if this server's launch policy permits it."
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.approve",
		Description: "Answer one parked confirmation, releasing (or refusing) the blocked tool call. " +
			"Read its risk, consequence and args first — approving is how this assistant is allowed to change anything, " +
			"and YOU are the decision, not a human reviewing behind you. Pass the runId you were watching so a decision " +
			"that arrives after its turn ended is rejected rather than applied to a successor.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in ApproveInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		decision := DecisionRejected
		if in.Approve {
			decision = DecisionApproved
		}
		// Correlation and settlement happen TOGETHER, under the broker's lock. Checking
		// first and resolving after leaves a window in which the approval that passed
		// the check settles and a different one is inserted — and ids are eight hex
		// characters, so "that cannot be the same id" is an assumption rather than a
		// guarantee. One operation removes the window instead of arguing about how
		// unlikely it is.
		settled, mismatch := sess.Approvals().ResolveForRun(in.ApprovalID, in.RunID, decision)
		if mismatch != nil {
			return nil, ActedOutput{}, mismatch
		}
		if settled {
			return nil, ActedOutput{Acted: true, Message: "approval " + in.ApprovalID + " " + string(decision)}, nil
		}
		// Not pending: either already settled (very likely a timeout, if the caller was
		// slow) or never real. Say which — "not found" alone sends a caller hunting.
		if prior, ok := sess.Approvals().Outcome(in.ApprovalID); ok {
			return nil, ActedOutput{Acted: false, Message: "approval " + in.ApprovalID +
				" was already settled as " + string(prior) + "; the tool call has moved on. Ask again if you still want the work done."}, nil
		}
		return nil, ActedOutput{}, fmt.Errorf(
			"no approval %q is pending in this session — call daintree.approvals to see what is waiting", in.ApprovalID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.questions",
		Description: "List the multiple-choice QUESTIONS this session is parked on. A question is not an approval: " +
			"it is the assistant asking which of several options you meant, and it BLOCKS the turn until answered. " +
			"An unanswered one is cancelled on a timer rather than defaulted, because there is no safe default for " +
			"'which did you mean?'.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, QuestionsOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, QuestionsOutput{}, err
		}
		qs := sess.Questions()
		pending, remaining := boundedQuestions(qs.Pending(), MaxPendingApprovals)
		out := QuestionsOutput{
			Mode:              string(qs.Mode()),
			DecisionAuthority: "caller-agent",
			Pending:           pending,
			Count:             len(pending),
			Remaining:         remaining,
		}
		if out.Pending == nil {
			out.Pending = []PendingQuestion{}
		}
		if qs.Mode() != QuestionDelegate {
			out.DecisionAuthority = "none"
			if len(pending) == 0 {
				out.Note = "This session does not park questions — the assistant's multiple-choice questions are " +
					"declined and the tool call fails. Open a session with approvals:\"delegate\" to answer them yourself."
			}
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.question.answer",
		Description: "Answer one parked multiple-choice question by the INDEX of the option you are choosing, " +
			"releasing the blocked tool call. Read the options first. An out-of-range index cancels the call " +
			"instead of picking the nearest option — an invented answer the turn then acts on is worse than no answer.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in AnswerQuestionInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		// Correlation and settlement in ONE call under the broker's lock — checking
		// first and answering after leaves a window in which the checked question
		// settles and another is inserted.
		settled, rangeErr := sess.Questions().AnswerForRun(in.QuestionID, in.RunID, in.Choice)
		var mismatch *QuestionRunMismatchError
		if errors.As(rangeErr, &mismatch) {
			return nil, ActedOutput{}, rangeErr
		}
		if rangeErr != nil {
			// The question WAS settled — as a cancellation. Reported as an error so the
			// caller sees that its answer did not land, with the outcome named.
			return nil, ActedOutput{}, rangeErr
		}
		if settled {
			return nil, ActedOutput{Acted: true, Message: "question " + in.QuestionID + " answered"}, nil
		}
		if prior, ok := sess.Questions().Outcome(in.QuestionID); ok {
			return nil, ActedOutput{Acted: false, Message: "question " + in.QuestionID +
				" was already settled (" + prior + "); the tool call has moved on"}, nil
		}
		return nil, ActedOutput{}, fmt.Errorf(
			"no question %q is pending in this session — call daintree.questions to see what is waiting", in.QuestionID)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.attention",
		Description: "Read the project's attention inbox: completions from asynchronous work, watcher findings, timer fires — " +
			"everything that settled OUTSIDE a turn. This is how background work reports back; it never arrives as a late event on a run.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in AttentionInput) (*mcp.CallToolResult, AttentionOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, AttentionOutput{}, err
		}
		// Default is PEEK. See AttentionInput.Acknowledge: acknowledging inside the read
		// is at-most-once delivery, and these rows are the only report background work
		// ever makes.
		ack := in.Acknowledge != nil && *in.Acknowledge
		// ALWAYS PEEK FIRST, then page, then acknowledge only what the page carried.
		//
		// Runtime.Attention(ctx, true) marks everything it RETURNED as notified, and it
		// cannot know this handler is about to drop a page's worth — so asking it to
		// acknowledge a read that is then paged would stamp items this response never
		// carried, permanently losing exactly the reports that arrive nowhere else.
		// Splitting the read from the acknowledgement lets both bounds hold: the
		// response is paged, and only the ids actually delivered are consumed.
		// The page is pushed INTO the runtime, not applied to what it returns. Paging
		// afterwards would still materialize a night's worth of rows, and — worse —
		// acknowledgement is version-conditional on the exact rows read, so marking a
		// fetch the handler then truncated would stamp rows nobody received.
		limit := clampPageSize(in.Limit, 50, MaxAttentionItems)
		events, more, err := sess.Attention(ctx, limit, ack)
		if err != nil {
			return nil, AttentionOutput{}, err
		}
		out := AttentionOutput{Count: len(events), More: more, Items: make([]AttentionItem, 0, len(events))}
		for _, e := range events {
			// Title and Summary are written by whatever published the row — a watcher, a
			// tool, a backend — so they are externally sourced and bounded like every
			// other such string that reaches a response.
			item := AttentionItem{
				ID:       e.ID,
				Severity: string(e.Severity),
				Source:   string(e.Source),
				Title:    truncateBytes(e.Title, MaxEventTextBytes),
				Summary:  truncateBytes(e.Summary, MaxEventTextBytes),
				Count:    e.Count,
			}
			if e.Target != nil {
				item.Target = targetLabel(e.Target)
				item.AsyncID = e.Target.AsyncInvocationID
			}
			out.Items = append(out.Items, item)
		}
		if !ack && len(out.Items) > 0 {
			out.Note = "These items are still unacknowledged and WILL be reported again. Call daintree.attention.ack with their ids once you have acted on them."
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.attention.ack",
		Description: "Acknowledge attention items you have acted on, so they stop being reported. " +
			"Read them with daintree.attention first — acknowledging is what makes delivery reliable: " +
			"an item stays pending until you say you have it, so a dropped response costs you a duplicate rather than the item.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in AttentionAckInput) (*mcp.CallToolResult, AttentionAckOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, AttentionAckOutput{}, err
		}
		if len(in.EventIDs) == 0 {
			return nil, AttentionAckOutput{}, errors.New(
				"eventIds is required — there is deliberately no acknowledge-everything, because it would consume rows you never read")
		}
		acked, unknown, err := sess.AcknowledgeAttention(ctx, in.EventIDs)
		if err != nil {
			return nil, AttentionAckOutput{}, err
		}
		out := AttentionAckOutput{Acknowledged: acked, Unknown: unknown}
		switch {
		case len(unknown) == 0:
			out.Message = fmt.Sprintf("acknowledged %d item(s)", acked)
		default:
			// Not an error: retrying an ack after an ambiguous transport failure is the
			// expected path, and the second attempt finds them already gone.
			out.Message = fmt.Sprintf(
				"acknowledged %d item(s); %d id(s) matched nothing (already acknowledged, or never real)", acked, len(unknown))
		}
		return nil, out, nil
	})
}

// describe renders a session for a tool response, attaching the warnings a caller must
// not miss.
func describe(s *Session, info *BinaryInfo) SessionOutput {
	facts := s.Facts()
	snap := info.Snapshot()
	state := s.State()
	// ONE snapshot: reading busy and the current run separately could report busy:true
	// with no current run (or the reverse) when a turn settles between the two reads —
	// a state the session was never actually in.
	live := s.Live()
	out := SessionOutput{
		SessionID:      s.ID,
		Facts:          facts,
		State:          string(state),
		Busy:           live.Busy,
		CurrentRunID:   live.CurrentRunID,
		RecentRuns:     live.Recent,
		CloseStartedAt: s.CloseStartedAt(),
		Server:         snap,
	}
	if out.RecentRuns == nil {
		// Stable empty array, never null — a caller loops over this.
		out.RecentRuns = []RunSummary{}
	}
	if err := s.CloseError(); err != nil {
		out.CloseError = err.Error()
		out.Warnings = append(out.Warnings,
			"This session did not close cleanly, so its project owner lease may still be held and other processes "+
				"cannot open the project. Retry daintree.session.close; if it keeps failing, restart the MCP server.")
	}
	if !facts.MCPConnected {
		out.Warnings = append(out.Warnings,
			"MCP is not connected: this session runs in degraded local mode and every terminal/orchestration tool will report 'not connected'.")
	}
	if facts.LogPath == "" {
		out.Warnings = append(out.Warnings,
			"Debug logging is off, so a bad run cannot be diagnosed afterwards. Pass debugLog:true when opening a session.")
	}
	if snap.Stale {
		out.Warnings = append(out.Warnings, snap.StaleMessage())
	}
	// The non-fatal half of the pin preflight (the backend accepts pins but serves no
	// catalog, so the ids could not be checked before the session opened). The fatal
	// half never reaches here — it fails the open.
	if facts.PinPreflightWarning != "" {
		out.Warnings = append(out.Warnings, facts.PinPreflightWarning)
	}
	return out
}

// boundEventText caps each event's text and stops the page once the aggregate budget is
// spent, returning the events that fit and how many were dropped for the budget.
//
// It trims rather than refuses, and it stops rather than encoding the rest to discover
// the response is too big. The dropped count is folded into withheldEvents, which the
// caller already reads — so a page shortened for size is indistinguishable, to a
// consumer, from one shortened for count, and both say how much is left.
func boundEventText(evs []Event) ([]Event, int) {
	budget := MaxResponseTextBytes
	for i := range evs {
		if budget <= 0 {
			// Everything from here on is dropped. Reported, never silent.
			return evs[:i], len(evs) - i
		}
		evs[i].Text = truncateBytes(evs[i].Text, MaxEventTextBytes)
		evs[i].Summary = truncateBytes(evs[i].Summary, MaxEventTextBytes)
		evs[i].Error = truncateBytes(evs[i].Error, MaxEventTextBytes)
		budget -= len(evs[i].Text) + len(evs[i].Summary) + len(evs[i].Error)
	}
	return evs, 0
}

// joinContexts returns a context cancelled when EITHER parent is.
//
// context has no built-in join, and the alternative — picking one parent — is wrong in
// both directions here: the server lifetime misses a session closing under a live
// elicitation, and the session lifetime misses the process stopping. The goroutine ends
// with the derived context, so it cannot outlive what it is bound to.
func joinContexts(a, b context.Context) context.Context {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	ctx, cancel := context.WithCancel(a)
	go func() {
		defer cancel()
		select {
		case <-b.Done():
		case <-ctx.Done():
		}
	}()
	return ctx
}

// waitBudget clamps a caller's wait to something this server is willing to hold a
// request open for.
func waitBudget(waitMs int) time.Duration {
	// Bounded as an INTEGER first. Converting a large millisecond count straight to a
	// Duration wraps int64 nanoseconds negative, which then reads as non-positive and
	// takes the same branch as "unset" — so an absurd wait and no wait at all produced
	// identical behaviour. Here they still converge on the cap, but by a route that says
	// so rather than by an accident of arithmetic.
	const maxMs = int64(maxBlockWait / time.Millisecond)
	if waitMs <= 0 || int64(waitMs) > maxMs {
		return maxBlockWait
	}
	return time.Duration(waitMs) * time.Millisecond
}

// waitForSettle blocks until the run finishes, the caller gives up, or the (capped)
// budget expires. This is `ask`'s block mode, where the caller has said it wants the
// ANSWER and progress is of no use to it.
//
// It selects on the REQUEST context, which matters for shutdown: the SDK waits for
// in-flight handlers before Run returns, so a wait that ignored cancellation would hold
// the server open — and every session's project lease with it — for up to the full
// budget after the client had already dropped the pipe.
func waitForSettle(ctx context.Context, run *Run, waitMs int) {
	timer := time.NewTimer(waitBudget(waitMs))
	defer timer.Stop()
	select {
	case <-run.Done():
	case <-timer.C:
	case <-ctx.Done():
	}
}

// waitForChange is `poll`'s long wait: it returns as soon as the run has something NEW
// to say past sinceSeq, not only when it finishes. Waiting for completion alone made a
// 60s poll sit through arriving content, a tool starting and finishing, and — worst —
// the turn becoming BLOCKED on an approval, reporting none of it until the budget
// expired. A caller that wants the whole run should block on ask instead.
func waitForChange(ctx context.Context, run *Run, sinceSeq int, sinceRev uint64, waitMs int) {
	run.WaitForChange(ctx, sinceSeq, sinceRev, waitBudget(waitMs))
}

// hasPendingDecision reports whether this run is currently STOPPED waiting on a caller —
// on a confirmation or on a multiple-choice question. Either broker may be nil (tests).
//
// A long poll must never sleep through this. The revision counter alone cannot catch a
// decision that parked BETWEEN two polls: the handler then captures an already-advanced
// revision, sinceSeq is at the event tail, and nothing further will ever be signalled —
// so the caller waits out its whole budget on a turn that is stopped, possibly past the
// decision's own timeout. Checking for a parked decision before waiting covers
// parked-before-capture, parked-between, and parked-after alike.
func hasPendingDecision(run *Run, approvals *Approvals, questions *Questions) bool {
	if approvals != nil {
		for _, pa := range approvals.Pending() {
			if pa.RunID == run.ID {
				return true
			}
		}
	}
	if questions != nil {
		for _, pq := range questions.Pending() {
			if pq.RunID == run.ID {
				return true
			}
		}
	}
	return false
}

// renderRun projects a run into its tool response. approvals may be nil (tests).
func renderRun(run *Run, sinceSeq, maxEvents int, approvals *Approvals) RunOutput {
	return renderRunWith(run, sinceSeq, maxEvents, approvals, nil)
}

// renderRunWith is renderRun plus the question broker. Split rather than widened at every
// call site because most callers have no question broker to hand and a nil one behaves
// exactly as before.
func renderRunWith(run *Run, sinceSeq, maxEvents int, approvals *Approvals, questions *Questions) RunOutput {
	// ONE lock hold for the events, the total, the outcome and the async ledger — see
	// RunSnapshot. Reading them separately produced responses describing a run state
	// that never existed.
	snap := run.SnapshotFull(sinceSeq, maxEvents)
	evs, withheld := snap.Events, snap.Remaining
	status, content, errMsg := snap.Status, snap.Content, snap.Error
	stats, startedAt, endedAt := snap.Stats, snap.StartedAt, snap.EndedAt
	// Bound the events themselves, not just how many of them there are. A page count
	// alone is not a size bound when each event's text can be a whole round's answer.
	evs, droppedForBudget := boundEventText(evs)
	withheld += droppedForBudget
	out := RunOutput{
		RunID:          run.ID,
		SessionID:      run.SessionID,
		Status:         string(status),
		Events:         evs,
		WithheldEvents: withheld,
		Content:        content,
		Error:          errMsg,
		Stats:          stats,
	}
	// nextSeq is the seq AFTER the last event returned, so an incremental caller never
	// re-reads nor skips. With a withheld tail it is the next withheld event, not the
	// end of the timeline. It comes from the snapshot, normalized to the tail — echoing
	// a caller's out-of-range cursor back told it to continue at a point every future
	// event would fall below, so it would skip the whole rest of the run.
	out.NextSeq = snap.NextSeq
	if droppedForBudget > 0 {
		// The size budget shortened the page below the count the snapshot cursor
		// assumed, so the cursor has to follow the events actually returned.
		out.NextSeq = snap.NextSeq - droppedForBudget
	}
	out.TotalEvents = snap.TotalEvents
	out.AsyncOperations = snap.Async
	if endedAt > 0 {
		out.DurationMs = int(endedAt - startedAt)
	} else {
		out.DurationMs = int(domain.NowMS() - startedAt)
	}
	if approvals != nil {
		// This run's approvals ONLY. A blanket match would report every completed run in
		// the session as BLOCKED while any turn was parked, and a stale one from an
		// abandoned turn would read as a blocker on work that is not actually waiting.
		mine := make([]PendingApproval, 0, 4)
		for _, pa := range approvals.Pending() {
			if pa.RunID == run.ID {
				mine = append(mine, pa)
			}
		}
		var withheldApprovals int
		out.PendingApprovals, withheldApprovals = boundedApprovals(mine, MaxPendingApprovals)
		out.WithheldApprovals = withheldApprovals
	}
	// Externally-sourced strings, all of which can be arbitrarily large: a backend can
	// return a multi-megabyte error, and the async ledger accumulates for the life of a
	// long run. Bounding the events alone left three ways to blow the same response up.
	out.Error = truncateBytes(out.Error, MaxEventTextBytes)
	if len(out.AsyncOperations) > MaxAsyncOperations {
		out.WithheldAsyncOperations = len(out.AsyncOperations) - MaxAsyncOperations
		out.AsyncOperations = out.AsyncOperations[:MaxAsyncOperations]
	}
	if questions != nil {
		// This run's questions ONLY, same as the approvals above: a question from an
		// abandoned turn would read as a blocker on work that is not waiting.
		mineQ := make([]PendingQuestion, 0, 2)
		for _, pq := range questions.Pending() {
			if pq.RunID == run.ID {
				mineQ = append(mineQ, pq)
			}
		}
		out.PendingQuestions, out.WithheldQuestions = boundedQuestions(mineQ, MaxPendingApprovals)
	}
	// The full text is always available from the run transcript resource. What this
	// stops is one poll response carrying a megabyte of prose nobody asked for.
	out.Content = truncateBytes(out.Content, MaxContentBytes)
	// Stable empty arrays, never null. A caller loops over these; `omitempty` turning
	// an empty list into a missing key is a needless special case in every consumer.
	if out.Events == nil {
		out.Events = []Event{}
	}
	if out.PendingApprovals == nil {
		out.PendingApprovals = []PendingApproval{}
	}
	out.NextAction = nextAction(out)
	return out
}

// nextAction is the one-line instruction attached to every run response.
func nextAction(out RunOutput) string {
	// A parked approval outranks everything else this could say: the run is not slow,
	// it is STOPPED, and polling harder will never move it.
	if n := len(out.PendingApprovals); n > 0 {
		names := make([]string, 0, n)
		for _, pa := range out.PendingApprovals {
			names = append(names, pa.Tool)
		}
		return fmt.Sprintf(
			"BLOCKED on %d approval(s) for %s. The turn cannot proceed until you call daintree.approve for each id in pendingApprovals; unanswered ones are denied on a timer.",
			n, strings.Join(names, ", "))
	}
	if n := len(out.PendingQuestions); n > 0 {
		// Named separately from an approval, because the caller does something
		// different: it picks an option rather than allowing or refusing an action, and
		// an unanswered one is CANCELLED rather than denied.
		return fmt.Sprintf(
			"BLOCKED on %d question(s). The turn cannot proceed until you call daintree.question.answer with the "+
				"index of the option you choose, for each id in pendingQuestions; unanswered ones are cancelled on a timer.",
			n)
	}
	switch RunStatus(out.Status) {
	case RunRunning:
		// Naming waitMs is the point: without it a model polls in a tight loop, which
		// costs it context and tells it nothing new each time.
		return fmt.Sprintf(
			"Still running after %ds. Call daintree.poll with sinceSeq:%d and waitMs (e.g. 60000) to wait for progress rather than polling repeatedly.",
			out.DurationMs/1000, out.NextSeq)
	case RunSucceeded:
		if n := len(out.AsyncOperations); n > 0 {
			// Deliberately "accepted", not "has not completed": this run saw the handles
			// issued and will never see them settle, so it cannot honestly claim they
			// are still outstanding. The inbox is the only place that knows.
			return fmt.Sprintf(
				"Finished. It accepted %d background operation(s) whose outcome this run will never carry — read daintree.attention for their completions, then acknowledge them with daintree.attention.ack.", n)
		}
		return "Finished. `content` is the answer; nothing further is needed for this run."
	case RunCancelled:
		return "Cancelled. The session is still usable — call daintree.ask again when you know what you want instead."
	case RunFailed:
		// Explicitly discourage the retry loop: the assistant's own logs are full of
		// models re-issuing a call whose failure was unrecoverable.
		return "Failed. Read `error` before retrying — re-asking the same thing will usually fail the same way. The session is still usable."
	default:
		return ""
	}
}

// targetLabel renders an event target compactly. Terminal wins over worktree, matching
// the inbox's own precedence (queue.Format).
func targetLabel(t *domain.EventTarget) string {
	switch {
	case t.TerminalID != "":
		return "terminal " + t.TerminalID
	case t.WorktreeID != "":
		return "worktree " + t.WorktreeID
	default:
		return ""
	}
}
