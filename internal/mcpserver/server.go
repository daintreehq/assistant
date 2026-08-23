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
	// Policy is the process-level authority ceiling applied to every session.open.
	// See policy.go.
	//
	// It is a VALUE, not a pointer, because a nil pointer used to mean "unconfined" —
	// so the dangerous configuration was what you got by forgetting a field. Opting
	// out is now an explicit Unconfined marker, which is more code than the safe
	// default rather than less.
	Policy ServerPolicy
	// Unconfined removes the ceiling entirely, ignoring Policy. Only ever right for a
	// trusted embedding path where the operator IS the caller; on the stdio surface the
	// caller is a model, so ServeModelFacing refuses it outright.
	Unconfined *TrustedUnconfined
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
	// The registry is built WITH its ceiling, so there is no window in which a
	// session.open could be served by an unconfined registry — and no way to reach the
	// unconfined one except by naming it.
	var reg *Registry
	if opts.Unconfined != nil {
		reg = NewUnconfinedRegistry(lifetime, opts.Factory)
	} else {
		reg = NewRegistry(lifetime, opts.Factory, opts.Policy)
	}

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

// ServeModelFacing is Serve for the surface whose caller is a MODEL. It refuses the
// unconfined marker, so the one configuration that must never reach `mcp --stdio`
// cannot be reached from it by any argument at all.
//
// The two constructors exist because "remember to install a policy" is not a boundary.
// A caller on this path chooses only what to NARROW; a caller that genuinely wants no
// ceiling has to name Serve and TrustedUnconfined together, which is a thing you do on
// purpose rather than by omission.
func ServeModelFacing(ctx context.Context, opts Options) error {
	if opts.Unconfined != nil {
		return fmt.Errorf("mcpserver: a model-facing server cannot be unconfined; " +
			"use Serve if this really is a trusted embedding")
	}
	// A zero policy is still permissive by omission — no tier ceiling and no root
	// allowlists — so "installed a policy" is not the same as "confined". MaxTier is
	// the one dimension every real launch can always fill in (the process has a tier
	// whether or not anyone named one), which makes its absence a reliable sign that
	// the policy was never actually derived from anything.
	if opts.Policy.MaxTier == "" {
		return fmt.Errorf("mcpserver: a model-facing server needs a policy with a tier ceiling; " +
			"an empty ServerPolicy confines nothing")
	}
	return Serve(ctx, opts)
}

// instructions is the server-level guidance an MCP client shows its model. It exists to
// prevent the two mistakes this surface invites: treating `ask` as synchronous, and
// forgetting that a session holds a project lease that must be released.
const instructions = `The Daintree assistant is an orchestration agent: it plans operations, spawns visible
agent terminals in a project, supervises them, and reports back. It does NOT edit files
itself — it delegates edits to agents it spawns.

Use it like this:

1. daintree.session.open — bind a session to a project. Endpoints and credentials are
   normally PINNED by whoever launched this server: omit backendUrl, mcpUrl, apiKeyFile
   and mcpTokenFile and you inherit them. A bearer is NEVER an argument — do not put a
   token in a tool call. Every session argument can only NARROW what the server was
   launched with; a request above its policy is refused, not downgraded, and the refusal
   says so. Pass debugLog:true so a bad run can be diagnosed. Keep the returned
   sessionId.
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
DECLINED and the turn carries on without them. If the server permits it, open the session
with approvals:"delegate" and answer with daintree.approve (a parked call BLOCKS the turn
until you do), or approvals:"auto" to skip the question entirely.

"delegate" means what it says: YOU decide, not a human. Nobody sees these requests but
you, so read the risk, the consequence and the args before approving one — and treat a
request that does not match work you asked for as a reason to refuse and interrupt, not
as a formality. Both modes may be refused by the server's launch policy, in which case
the refusal says so.

When a run needs diagnosing rather than summarising, read its resources instead of
polling harder: daintree://session/{id}/run/{runId} returns the timeline in pages larger
than poll's window (append ?fromSeq=N&limit=M; the response reports nextSeq, remaining
and complete), and daintree://session/{id}/log is the structured trace of every backend
request and tool call for this whole SERVER PROCESS — filter it by sessionId.

Nothing this server returns is unbounded. Every list has a server maximum as well as a
default, and asking for more gives you the maximum plus a count of what was withheld —
never an error, and never a silent truncation. If a response says something was withheld,
read the rest rather than assuming you saw it all.`
