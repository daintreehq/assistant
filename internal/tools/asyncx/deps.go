// Package asyncx is the asynchronous-operations tool family: the model-facing
// surface of the runtime-owned durable futures in internal/asyncwork.
//
//   - terminal.run.async   — risk "terminal"; send a command to ONE terminal and
//     watch it to completion asynchronously (the send confirms like
//     terminal.sendCommand).
//   - terminal.await.async — risk "local"; watch already-running agent
//     terminal(s) to completion asynchronously — the out-of-turn twin of
//     terminal.awaitAll.
//   - async.list           — risk "read"; the invocation ledger.
//   - async.cancel         — risk "local"; stop tracking (never kills anything).
//
// The async contract, enforced here and owned by the coordinator: the tool
// returns an IMMEDIATE "accepted" result carrying an async handle (asy_…); the
// completion arrives later as an attention-queue event that wakes the model —
// NEVER as a late tool result for the original call.
package asyncx

import (
	"context"

	"github.com/daintreehq/assistant/internal/domain"
	"github.com/daintreehq/assistant/internal/tools"
)

// TerminalReader is the id-resolution slice of the terminal read surface
// (mirrors extractionx.TerminalReader's roster read; consumer-defined so the
// family compiles in isolation).
type TerminalReader interface {
	Connected() bool
	ListTerminals(ctx context.Context) (ids []string, ok bool)
}

// CommandSender performs the ONE mutating side effect this family owns: typing
// a command into a terminal (the Daintree terminal.sendCommand MCP tool,
// deliberately never retried — the transport force-disables retry for
// mutations, and an ambiguous failure is surfaced, not repeated).
type CommandSender interface {
	SendCommand(ctx context.Context, terminalID, command string) error
}

// SendRejectedError marks a send the server DEFINITIVELY rejected (a tool-level
// error result): the command did NOT run, so the failure text may safely invite
// a corrected retry. Any other send error is AMBIGUOUS — a transport drop or
// timeout can land AFTER Daintree accepted the command — and the failure text
// must forbid a blind re-send. The adapter wraps tool-level errors in this type
// so the handler can phrase the two outcomes differently.
type SendRejectedError struct{ Msg string }

func (e SendRejectedError) Error() string { return e.Msg }

// CommandObserver receives a mark when terminal.run.async injects its command
// (mirrors mcpx.CommandObserver): it feeds the session's cross-call settle
// memory so an in-turn wait never settles on working evidence that predates the
// newly injected command. nil disables recording.
type CommandObserver interface {
	MarkCommandSent(terminalID string, at int64)
}

// Coordinator is the runtime owner of registered invocations.
type Coordinator interface {
	Started() bool
	Register(rec domain.AsyncInvocationRecord, terminalIDs []string) error
	Deregister(id string)
}

// Store is the async-invocation ledger slice this family needs.
type Store interface {
	InsertAsyncInvocation(rec domain.AsyncInvocationRecord) (domain.AsyncInvocationRecord, error)
	GetAsyncInvocation(id string) (*domain.AsyncInvocationRecord, error)
	ListAsyncInvocations(status string) ([]domain.AsyncInvocationRecord, error)
	ListLiveAsyncInvocations() ([]domain.AsyncInvocationRecord, error)
	CountLiveAsyncInvocations() (int, error)
	ClaimLiveAsyncInvocation(id string, patch map[string]any) (bool, error)
}

// Deps wires the async tool family.
type Deps struct {
	Reader      TerminalReader
	Sender      CommandSender
	Coordinator Coordinator
	Store       Store
	SessionID   string
	// Observer records the run.async input injection into the shared settle
	// memory (see CommandObserver). nil ⇒ no recording.
	Observer CommandObserver
	// Now seams the clock; nil ⇒ domain.NowMS.
	Now func() int64
}

func (d Deps) now() int64 {
	if d.Now != nil {
		return d.Now()
	}
	return domain.NowMS()
}

// Tools returns the async family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newRunAsyncTool(deps),
		newAwaitAsyncTool(deps),
		newListTool(deps),
		newCancelTool(deps),
	}
}
