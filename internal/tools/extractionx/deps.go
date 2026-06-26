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
//   - terminal.extract.async — risk "local"; runs the same poll+text-extract in the
//     background and publishes the outcome to the attention
//     queue, OUTLIVING the turn (so it must not carry the
//     turn's cancellation).
//
// The poll loop reuses the watcher DSL vocabulary; modelJudge conditions are
// intentionally unsupported here (they would re-run the classifier every tick).
package extractionx

import (
	"context"

	"github.com/daintreehq/daintree-assistant/internal/debuglog"
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
//   - Connected gates the call (MCP_UNAVAILABLE when down).
type TerminalReader interface {
	Connected() bool
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
//   - ExtractText  → terminal_extract_text.v1
//   - ExtractJSON  → terminal_extract_json.v1 (schema is an optional JSON-schema object)
//   - Verdict      → extraction_verdict.v1
//   - Judge        → terminal_judge.v1 (the shared yes/no terminal judge)
type Router interface {
	ExtractText(ctx context.Context, instruction string, terminalIDs []string, tail string) (text string, truncated bool, err error)
	ExtractJSON(ctx context.Context, instruction string, terminalIDs []string, tail string, schema map[string]any) (result any, err error)
	Verdict(ctx context.Context, result, condition string) (pass bool, reason string, err error)
	Judge(ctx context.Context, in JudgeInput) (domain.ModelJudgeAnswer, error)
}

// Queue is the slice of the attention queue terminal.extract.async publishes to.
type Queue interface {
	Publish(ctx context.Context, args domain.QueuePublishArgs) (domain.QueueEvent, error)
}

// Deps wires the extraction family.
type Deps struct {
	Reader TerminalReader
	Router Router
	Queue  Queue
	// BaseContext is the APP-SCOPED background context detached async work derives
	// from (terminal.extract.async OUTLIVES the turn, so it must not carry the
	// turn's cancellation — but it MUST stop when the App shuts down, instead of
	// leaking into closed deps). nil ⇒ context.Background() (tests / safe default).
	BaseContext context.Context
	// DebugLog routes the async-extraction trace (publish failures/drops) to the
	// global debug log. Zero value ⇒ disabled (a no-op).
	DebugLog debuglog.Config
}

// baseContext returns the app-scoped background context, falling back to
// context.Background() when none was wired (keeps the family usable in tests).
func (d Deps) baseContext() context.Context {
	if d.BaseContext != nil {
		return d.BaseContext
	}
	return context.Background()
}

// Tools returns the extraction family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newExtractTool(deps),
		newExtractJSONTool(deps),
		newExtractAsyncTool(deps),
		newAwaitAllTool(deps),
	}
}
