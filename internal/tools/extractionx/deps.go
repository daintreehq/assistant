// Package extractionx is the on-demand terminal-extraction family. Each tool points
// the small model at one or more Daintree terminals, reads a bounded tail, and pulls
// out caller-specified content — so raw scrollback never enters the main agent's
// context, only the extracted result. Output shape is fixed PER TOOL (not a
// conditional arg), so the model can't reach for JSON and forget its schema:
//   - terminal.extract       — risk "read"; inline plain-TEXT extract (reads once, or
//     polls a `wait` condition then extracts; omit
//     `instruction` for a finished/condition gate).
//   - terminal.extract.json  — risk "read"; inline STRUCTURED extract; `instruction`
//     and `jsonSchema` are both required.
//
// There is deliberately NO async variant. terminal.extract.async used to run the same
// poll+extract on a detached goroutine and publish to the attention queue, but it was a
// bare `go func()` on an app-scoped context: no durable ledger row, no adoption by the
// next owner, no exactly-once publish, and it never appeared in the turn's
// async_operations block. The runtime-owned async futures (terminal.run.async /
// terminal.await.async + the asyncwork coordinator) supersede it on every one of those
// axes — do the wait there, then read with terminal.extract.
//
// The poll loop reuses the watcher DSL vocabulary; modelJudge conditions are
// intentionally unsupported here (they would re-run the classifier every tick).
package extractionx

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// TerminalStatusEntry is the per-terminal status the reader returns for one read.
// recentOutput may be absent (nil) per entry — callers fall back to a deep read.
type TerminalStatusEntry struct {
	AgentState    string
	WaitingReason string  // "question" | "prompt" | "" (meaningful only while waiting)
	RecentOutput  *string // nil when absent; "" is a valid "no output yet"
	ExitCode      *int
	// NotFound marks Daintree's per-entry "Terminal not found" shape: an unknown
	// id does NOT abort the batched terminal.getStatus and is NOT omitted from the
	// response — it comes back as a present entry with a per-entry error and a null
	// agentState. Waits must treat such an entry as GONE (closed/removed), exactly
	// like a roster-confirmed absence; matching on the empty AgentState alone would
	// instead poll forever (the FSM never settles on "") and strand the cohort.
	NotFound bool
}

// StatusReadResult is the outcome of one readStatuses across the target terminals.
// OK is true on a clean read; ByID maps terminalId → entry (an entry may be
// missing when that terminal has gone but OTHER terminals were returned).
type StatusReadResult struct {
	OK   bool
	ByID map[string]TerminalStatusEntry
}

// OutputReadResult is the outcome of one deep terminal.getOutput read.
type OutputReadResult struct {
	OK    bool
	Value string
}

// TerminalReader is the slice of the terminal-read surface this family needs. It
// is a LOCAL consumer-defined interface (NOT internal/daemon, which we must not
// import) — the wiring layer adapts the watcher engine's read helpers to it.
//
//   - ReadStatuses fetches agentState/recentOutput/exitCode for each id;
//     includeOutput requests the inline recentOutput tail.
//   - ReadOutput is the deep terminal.getOutput tail (capped to tailBytes chars).
//   - ListTerminals enumerates the live roster (terminal.list) so a truncated/prefix id
//     can be resolved to its canonical form; ok=false on an unreadable roster so callers
//     FAIL OPEN (a discovery hiccup must never block a wait).
//   - Connected gates the call (MCP_UNAVAILABLE when down).
type TerminalReader interface {
	Connected() bool
	ListTerminals(ctx context.Context) (ids []string, ok bool)
	ReadStatuses(ctx context.Context, terminalIDs []string, includeOutput bool) StatusReadResult
	ReadOutput(ctx context.Context, terminalID string, tailBytes int) OutputReadResult
}

// JudgeInput carries the terminal signals fed to the shared yes/no judge. It is a
// LOCAL mirror of the daemon's judge input (extractionx must not import
// internal/daemon) so the settle gate can ask the SAME finished-confirmation
// question the watcher asks, through the same byte-stable prompt and tier.
type JudgeInput struct {
	Tier          domain.ModelTier
	Question      string
	Goal          string
	AgentState    string
	RuntimeStatus string
	WaitingReason string
	LastOutputAt  string
	Tail          string
}

// Router is the slice of model access this family uses, each method mapping to a server-owned
// backend task (the CLI sends only structured data, never prompts):
//   - ExtractText  → terminal_extract_text
//   - ExtractJSON  → terminal_extract_json (schema is an optional JSON-schema object)
//   - Verdict      → extraction_verdict
//   - Judge        → terminal_judge (the shared yes/no terminal judge)
type Router interface {
	ExtractText(ctx context.Context, instruction string, terminalIDs []string, tail string) (text string, truncated bool, err error)
	ExtractJSON(ctx context.Context, instruction string, terminalIDs []string, tail string, schema map[string]any) (result any, err error)
	Verdict(ctx context.Context, result, condition string) (pass bool, reason string, err error)
	Judge(ctx context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error)
}

// Observations is the session-scoped, cross-call settle memory (see
// internal/tools/terminalobs) this family READS to seed the working→waiting
// settle gate and WRITES its live working observations into. Without it every
// wait starts from zero evidence, so a RE-await of an agent that finished
// between two waits cannot settle before the full FinishSettleGraceMS (20s) —
// even when a previous await in the same session watched that agent work.
// SeenWorkingSinceLastCommand is the only trustworthy read: raw "was it ever
// working" would settle a wait on evidence that predates a newly injected
// command (the pre-start false positive the grace exists to prevent), so the
// send wrappers invalidate the latch on every input injection. nil disables
// both directions (tests / stripped tool sets keep the per-call behaviour).
type Observations interface {
	MarkWorking(terminalID string, at int64)
	SeenWorkingSinceLastCommand(terminalID string) bool
}

// SupervisorRetirer retires the spawn-attached supervisor watcher(s) of a terminal
// whose completion the main turn just consumed DIRECTLY — an in-turn settle
// (terminal.awaitAll, or a terminal.extract wait that resolved). Once the model
// holds the completion in-hand, a watcher whose whole job was "announce when this
// agent is done" is redundant: left alive it re-announces minutes later as a stale
// attention event the model must then resolve by hand. settledStatus is the
// in-turn verdict (domain.SettleStatusFinished | SettleStatusFailed) so the
// implementation can advance the linked workflow ledger honestly. Returns the
// number of watchers retired. Implementations are BEST-EFFORT and must swallow
// their own errors (retirement is supervision bookkeeping — it must never break
// the wait tool that triggered it); nil disables retirement (tests).
type SupervisorRetirer interface {
	RetireForTerminal(ctx context.Context, terminalID, settledStatus string) int
}

// Deps wires the extraction family.
type Deps struct {
	Reader TerminalReader
	Router Router
	// InjectionsPending lets the IN-TURN cohort wait (terminal.awaitAll) notice that
	// the human typed a message mid-wait and break early, handing control back so the
	// message is acted on now instead of after the whole wait elapses. Set per-call
	// from the ToolContext; nil ⇒ never interrupted (tests, non-interactive actors).
	InjectionsPending func() bool
	// Supervisors retires a settled terminal's supervisor watcher(s) once an in-turn
	// wait consumed the completion (see SupervisorRetirer). nil ⇒ no retirement.
	Supervisors SupervisorRetirer
	// Observations is the cross-call settle memory (see the interface above). nil ⇒
	// every wait latches seenWorking per call only, as before.
	Observations Observations
}

// Tools returns the extraction family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newExtractTool(deps),
		newExtractJSONTool(deps),
		newAwaitAllTool(deps),
	}
}
