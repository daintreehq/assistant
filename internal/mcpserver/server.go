package mcpserver

import (
	"context"
	"fmt"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ServerName is the implementation name reported in the MCP handshake.
const ServerName = "daintree-assistant"

// Options configures a server run.
type Options struct {
	// Version is the build version, reported in the handshake and used to detect a
	// rebuilt binary.
	Version string
	// Factory builds a runtime per session. Required.
	Factory RuntimeFactory
	// Diagnostics receives human-readable lines. It must NOT be stdout: stdio is the
	// protocol transport and a stray byte there breaks the client's parser.
	Diagnostics io.Writer
}

// Serve runs the MCP server over stdio until the context is cancelled or the client
// closes the pipe.
//
// Shutdown tears every session down, which matters more here than in a one-shot: each
// open session holds a project's owner lease, and a server that exited without
// releasing them would leave every project it had touched refusing to open until the
// flocks were reaped.
func Serve(ctx context.Context, opts Options) error {
	if opts.Factory == nil {
		return fmt.Errorf("mcpserver: a runtime factory is required")
	}
	version := opts.Version
	if version == "" {
		version = "dev"
	}

	// The server's own context, not a per-call one. Everything that outlives a single
	// tool call hangs off this: the turns `ask` accepts (the entire point of the async
	// surface), and each session's scheduler and async coordinator. The SDK cancels a
	// request context as soon as its response is sent, so anything using one would die
	// the instant the call that created it returned.
	lifetime, stop := context.WithCancel(ctx)
	reg := NewRegistry(lifetime, opts.Factory)

	// Defers run LIFO, so these two are registered in the order OPPOSITE to how they
	// run. CloseAll is registered first and therefore runs LAST: cancellation reaches
	// every in-flight turn before any session tears its App down. CloseAll then waits
	// for those turns to unwind and releases each project lease — the one thing this
	// process must never skip, since a leaked flock blocks the project until it is
	// reaped.
	defer reg.CloseAll()
	defer stop()

	s := mcp.NewServer(&mcp.Implementation{
		Name:    ServerName,
		Version: version,
		Title:   "Daintree Assistant",
	}, &mcp.ServerOptions{
		Instructions: instructions,
	})
	Register(s, reg, NewBinaryInfo(version), lifetime)
	RegisterResources(s, reg)

	if opts.Diagnostics != nil {
		fmt.Fprintf(opts.Diagnostics, "daintree-assistant %s: serving MCP over stdio\n", version)
	}
	return s.Run(ctx, &mcp.StdioTransport{})
}

// instructions is the server-level guidance an MCP client shows its model. It exists to
// prevent the two mistakes this surface invites: treating `ask` as synchronous, and
// forgetting that a session holds a project lease that must be released.
const instructions = `The Daintree assistant is an orchestration agent: it plans operations, spawns visible
agent terminals in a project, supervises them, and reports back. It does NOT edit files
itself — it delegates edits to agents it spawns.

Use it like this:

1. daintree.session.open — bind a session to a project. Pass mcpUrl/mcpToken if you want
   it to actually drive terminals; without them it runs in degraded local mode. Pass
   debugLog:true so a bad run can be diagnosed. Keep the returned sessionId.
2. daintree.ask — ask for work. It returns a runId immediately; a real orchestration turn
   takes MINUTES. Do not set wait:true for anything that spawns agents.
3. daintree.poll — read progress. Pass the previous nextSeq as sinceSeq to read only what
   is new, and waitMs to wait rather than poll tightly.
4. daintree.attention — read what settled in the background. Asynchronous work never
   reports back on the run that started it.
5. daintree.session.close — ALWAYS close what you opened. An open session holds the
   project's owner lease and blocks other processes from opening the same project.

One turn runs at a time per session. To steer a turn already running, use
daintree.inject rather than a second ask; to abandon it, daintree.interrupt.

Mutating tools — terminal commands, git operations — need approval. By default they are
DECLINED and the turn carries on without them, so if you want the assistant to actually
change something, open the session with approvals:"ask" and answer with daintree.approve
(a parked call blocks the turn), or approvals:"auto" to skip asking entirely.

When a run needs diagnosing rather than summarising, read its resources instead of
polling harder: daintree://session/{id}/run/{runId} is the complete timeline poll
truncates, and daintree://session/{id}/log is the structured trace of every backend
request and tool call.`
