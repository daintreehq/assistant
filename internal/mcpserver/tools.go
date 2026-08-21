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
	Project     string `json:"project,omitempty" jsonschema:"Absolute path to the project the assistant should operate on. Defaults to the server process's working directory."`
	BackendURL  string `json:"backendUrl,omitempty" jsonschema:"Assistant backend endpoint, e.g. http://127.0.0.1:8473 for a local backend. Defaults to the environment or the deployed endpoint."`
	APIKeyFile  string `json:"apiKeyFile,omitempty" jsonschema:"Path to a file containing the API key. There is deliberately no way to pass the key inline."`
	Tier        string `json:"tier,omitempty" jsonschema:"Permission tier: supervisor, operator, or system."`
	McpURL      string `json:"mcpUrl,omitempty" jsonschema:"Daintree MCP endpoint. Without it the assistant runs in degraded local mode and every orchestration tool reports 'not connected'."`
	McpToken    string `json:"mcpToken,omitempty" jsonschema:"Daintree MCP bearer token. These expire roughly 12 minutes after minting."`
	StateDir    string `json:"stateDir,omitempty" jsonschema:"State and credentials root. Use a scratch path to isolate from the developer's real state."`
	LogDir      string `json:"logDir,omitempty" jsonschema:"Directory for the debug log."`
	AutoApprove *bool  `json:"autoApprove,omitempty" jsonschema:"Run mutating tools without confirmation. Without it every confirmation is auto-declined and mutating work is skipped."`
	DebugLog    *bool  `json:"debugLog,omitempty" jsonschema:"Write a structured session trace to the log directory. Strongly recommended: it is the only way to diagnose a bad run."`
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
	Events    []Event `json:"events,omitempty"`
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
	// PendingAsync lists async handles this run accepted. They settle OUTSIDE the run
	// and are reported through daintree.attention, never as a late event here.
	PendingAsync []string `json:"pendingAsync,omitempty"`
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

// SessionRefInput is the shape of every tool that only needs a session.
type SessionRefInput struct {
	SessionID string `json:"sessionId"`
}

// InjectInput steers a running turn.
type InjectInput struct {
	SessionID string `json:"sessionId"`
	Text      string `json:"text" jsonschema:"A message to fold into the RUNNING turn. The assistant picks it up at its next tool boundary."`
}

// AttentionInput reads the project inbox.
type AttentionInput struct {
	SessionID string `json:"sessionId"`
	// Acknowledge defaults to true via the handler, not the schema: a pointer keeps
	// "not supplied" distinguishable from an explicit false.
	Acknowledge *bool `json:"acknowledge,omitempty" jsonschema:"Mark the returned items delivered so the next call reports only new ones. Default true. Pass false to peek without consuming."`
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
			"All configuration is per-session, so repointing at a different backend or project is a close/open pair rather than a server restart. " +
			"Without mcpUrl/mcpToken the assistant runs in degraded local mode where it cannot see or drive terminals.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in OpenInput) (*mcp.CallToolResult, SessionOutput, error) {
		sess, err := reg.Open(ctx, OpenParams{
			Project: in.Project, BackendURL: in.BackendURL, APIKeyFile: in.APIKeyFile,
			Tier: in.Tier, McpURL: in.McpURL, McpToken: in.McpToken,
			StateDir: in.StateDir, LogDir: in.LogDir,
			AutoApprove: in.AutoApprove, DebugLog: in.DebugLog,
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
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in AskInput) (*mcp.CallToolResult, RunOutput, error) {
		if strings.TrimSpace(in.Prompt) == "" {
			return nil, RunOutput{}, errors.New("prompt is required")
		}
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, RunOutput{}, err
		}
		run, err := sess.Ask(lifetime, in.Prompt)
		if err != nil {
			return nil, RunOutput{}, err
		}
		if in.Wait {
			waitFor(ctx, run, in.WaitMs)
		}
		return nil, renderRun(run, 0, defaultPollEvents), nil
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
			waitFor(ctx, run, in.WaitMs)
		}
		max := in.MaxEvents
		if max <= 0 {
			max = defaultPollEvents
		}
		return nil, renderRun(run, in.SinceSeq, max), nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "daintree.inject",
		Description: "Steer the RUNNING turn by folding a message into it; the assistant picks it up at its next tool boundary. " +
			"Use this rather than a second ask, which would be rejected — a session runs one turn at a time.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in InjectInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		if strings.TrimSpace(in.Text) == "" {
			return nil, ActedOutput{}, errors.New("text is required")
		}
		if !sess.Inject(in.Text) {
			return nil, ActedOutput{Acted: false, Message: "no turn is running; use daintree.ask instead"}, nil
		}
		return nil, ActedOutput{Acted: true, Message: "folded into the running turn"}, nil
	})

	mcp.AddTool(s, &mcp.Tool{
		Name:        "daintree.interrupt",
		Description: "Cancel the running turn. The session stays open and the conversation is kept, so you can ask again.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, in SessionRefInput) (*mcp.CallToolResult, ActedOutput, error) {
		sess, err := reg.Get(in.SessionID)
		if err != nil {
			return nil, ActedOutput{}, err
		}
		if !sess.Interrupt() {
			return nil, ActedOutput{Acted: false, Message: "no turn is running"}, nil
		}
		return nil, ActedOutput{Acted: true, Message: "cancelling the running turn"}, nil
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
		ack := in.Acknowledge == nil || *in.Acknowledge
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
	return out
}

// waitFor blocks until the run settles, the caller gives up, or the (capped) budget
// expires. A run that is still going is NOT an error — the caller polls it.
//
// It selects on the REQUEST context, which matters for shutdown: the SDK waits for
// in-flight handlers before Run returns, so a wait that ignored cancellation would hold
// the server open — and every session's project lease with it — for up to the full
// budget after the client had already dropped the pipe.
func waitFor(ctx context.Context, run *Run, waitMs int) {
	budget := time.Duration(waitMs) * time.Millisecond
	if budget <= 0 || budget > maxBlockWait {
		budget = maxBlockWait
	}
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-run.Done():
	case <-timer.C:
	case <-ctx.Done():
	}
}

// renderRun projects a run into its tool response.
func renderRun(run *Run, sinceSeq, maxEvents int) RunOutput {
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
	for _, e := range evs {
		if e.Async != "" {
			out.PendingAsync = append(out.PendingAsync, e.Async)
		}
	}
	if endedAt > 0 {
		out.DurationMs = int(endedAt - startedAt)
	} else {
		out.DurationMs = int(domain.NowMS() - startedAt)
	}
	out.NextAction = nextAction(out)
	return out
}

// nextAction is the one-line instruction attached to every run response.
func nextAction(out RunOutput) string {
	switch RunStatus(out.Status) {
	case RunRunning:
		// Naming waitMs is the point: without it a model polls in a tight loop, which
		// costs it context and tells it nothing new each time.
		return fmt.Sprintf(
			"Still running after %ds. Call daintree.poll with sinceSeq:%d and waitMs (e.g. 60000) to wait for progress rather than polling repeatedly.",
			out.DurationMs/1000, out.NextSeq)
	case RunSucceeded:
		if len(out.PendingAsync) > 0 {
			return "Finished. It started background work that has NOT completed — poll daintree.attention for those completions; they never arrive on this run."
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
