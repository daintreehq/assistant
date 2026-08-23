package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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
	Approvals string `json:"approvals,omitempty" jsonschema:"How to answer a mutating tool. decline: skip it and carry on (the safe default). ask: park it for you to decide with daintree.approve. auto: never ask. Choose ask only if you will actually poll for approvals - a parked call blocks the whole turn until it is answered or times out."`
	// ApprovalTimeoutMs bounds a parked approval so a forgotten one cannot pin the turn.
	ApprovalTimeoutMs int `json:"approvalTimeoutMs,omitempty" jsonschema:"How long a parked approval waits before it is denied, in milliseconds. Default 300000 (5 minutes). Only meaningful when approvals is ask."`
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
	Busy      bool         `json:"busy"`
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
}

// RunOutput is the state of one run: its outcome so far plus a window of its events.
type RunOutput struct {
	RunID     string  `json:"runId"`
	SessionID string  `json:"sessionId"`
	Status    string  `json:"status" jsonschema:"running, success, error, or cancelled."`
	Events    []Event `json:"events"`
	// NextSeq is what to pass as sinceSeq on the next poll to get only new events.
	NextSeq int `json:"nextSeq"`
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
	MaxEvents int    `json:"maxEvents,omitempty" jsonschema:"Cap on events in this response. Default 40."`
	WaitMs    int    `json:"waitMs,omitempty" jsonschema:"Block up to this long for the run to settle before responding. Capped at 120000. Use it to avoid a tight polling loop."`
}

// ApprovalsOutput lists what a session is parked on.
type ApprovalsOutput struct {
	Mode    string            `json:"mode" jsonschema:"decline, ask, or auto."`
	Pending []PendingApproval `json:"pending"`
	Count   int               `json:"count"`
	// Note explains an empty list when the reason is the MODE rather than the absence
	// of mutating work — otherwise "0 pending" reads as "nothing wanted approval".
	Note string `json:"note,omitempty"`
}

// ApproveInput answers one parked confirmation.
type ApproveInput struct {
	SessionID  string `json:"sessionId"`
	ApprovalID string `json:"approvalId" jsonschema:"The id from daintree.approvals or from a run's pendingApprovals."`
	Approve    bool   `json:"approve" jsonschema:"true to allow the tool call, false to refuse it. Refusing lets the turn continue without that call."`
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
	Count int             `json:"count"`
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
			"Without an MCP URL and token the assistant runs in degraded local mode where it cannot see or drive terminals; " +
			"the token is inherited from the server process's environment, and is never passed inline.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OpenInput) (*mcp.CallToolResult, SessionOutput, error) {
		mode := ApprovalMode(strings.TrimSpace(in.Approvals))
		if mode != "" && !mode.Valid() {
			return nil, SessionOutput{}, fmt.Errorf(
				"unknown approvals mode %q — use \"decline\", \"ask\" or \"auto\"", in.Approvals)
		}
		if in.ApprovalTimeoutMs < 0 {
			return nil, SessionOutput{}, fmt.Errorf(
				"approvalTimeoutMs must not be negative (got %d) — the timeout is the only thing that bounds a parked approval",
				in.ApprovalTimeoutMs)
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
			ApprovalTimeout: time.Duration(in.ApprovalTimeoutMs) * time.Millisecond,
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
			"Always close a session you opened — the lease blocks other processes from opening the same project.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, ActedOutput, error) {
		if err := reg.Close(in.SessionID); err != nil {
			return nil, ActedOutput{}, err
		}
		return nil, ActedOutput{Acted: true, Message: "session closed"}, nil
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
		sess.Approvals().SetNotify(elicitNotifier(req.Session, sess.Approvals(), sess.ApprovalTimeout()))
		run, err := sess.Ask(lifetime, in.Prompt)
		if err != nil {
			return nil, RunOutput{}, err
		}
		if in.Wait {
			waitForSettle(ctx, run, in.WaitMs)
		}
		return nil, renderRun(run, 0, defaultPollEvents, sess.Approvals()), nil
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
			if !hasPendingApproval(run, sess.Approvals()) {
				// Revision captured BEFORE the wait: a change that lands between here
				// and the select must not be slept through either.
				waitForChange(ctx, run, in.SinceSeq, run.Revision(), in.WaitMs)
			}
		}
		max := in.MaxEvents
		if max <= 0 {
			max = defaultPollEvents
		}
		return nil, renderRun(run, in.SinceSeq, max, sess.Approvals()), nil
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
			"Only sessions opened with approvals:\"ask\" ever park; the default declines and carries on.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, ApprovalsOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ApprovalsOutput{}, err
		}
		pending := sess.Approvals().Pending()
		out := ApprovalsOutput{
			Mode:    string(sess.Approvals().Mode()),
			Pending: pending,
			Count:   len(pending),
		}
		if out.Pending == nil {
			out.Pending = []PendingApproval{}
		}
		if len(pending) == 0 && out.Mode != string(ApprovalAsk) {
			out.Note = "This session does not park approvals — mode is " + out.Mode +
				". Open a session with approvals:\"ask\" if you want to decide each mutating call."
		}
		return nil, out, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.approve",
		Description: "Answer one parked confirmation, releasing (or refusing) the blocked tool call. " +
			"Read its risk, consequence and args first — approving is how this assistant is allowed to change anything.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in ApproveInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		decision := DecisionRejected
		if in.Approve {
			decision = DecisionApproved
		}
		if sess.Approvals().Resolve(in.ApprovalID, decision) {
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
		events, err := sess.Attention(ctx, ack)
		if err != nil {
			return nil, AttentionOutput{}, err
		}
		out := AttentionOutput{Count: len(events), Items: make([]AttentionItem, 0, len(events))}
		for _, e := range events {
			item := AttentionItem{
				ID:       e.ID,
				Severity: string(e.Severity),
				Source:   string(e.Source),
				Title:    e.Title,
				Summary:  e.Summary,
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
	out := SessionOutput{SessionID: s.ID, Facts: facts, Busy: s.Busy(), Server: snap}
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

// waitBudget clamps a caller's wait to something this server is willing to hold a
// request open for.
func waitBudget(waitMs int) time.Duration {
	budget := time.Duration(waitMs) * time.Millisecond
	if budget <= 0 || budget > maxBlockWait {
		budget = maxBlockWait
	}
	return budget
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

// hasPendingApproval reports whether this run is currently blocked on a confirmation.
// approvals may be nil (tests).
func hasPendingApproval(run *Run, approvals *Approvals) bool {
	if approvals == nil {
		return false
	}
	for _, pa := range approvals.Pending() {
		if pa.RunID == run.ID {
			return true
		}
	}
	return false
}

// renderRun projects a run into its tool response. approvals may be nil (tests).
func renderRun(run *Run, sinceSeq, maxEvents int, approvals *Approvals) RunOutput {
	evs, withheld, status, content, errMsg, stats, startedAt, endedAt := run.Snapshot(sinceSeq, maxEvents)
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
	// end of the timeline.
	out.NextSeq = sinceSeq
	if len(evs) > 0 {
		out.NextSeq = evs[len(evs)-1].Seq + 1
	}
	out.AsyncOperations = run.AsyncOperations()
	if endedAt > 0 {
		out.DurationMs = int(endedAt - startedAt)
	} else {
		out.DurationMs = int(domain.NowMS() - startedAt)
	}
	if approvals != nil {
		// Only this run's approvals: a stale one from an abandoned turn would read as a
		// blocker on work that is not actually waiting.
		// This run's approvals ONLY. A blanket match would report every completed run in
		// the session as BLOCKED while any turn was parked.
		for _, pa := range approvals.Pending() {
			if pa.RunID == run.ID {
				out.PendingApprovals = append(out.PendingApprovals, pa)
			}
		}
	}
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
