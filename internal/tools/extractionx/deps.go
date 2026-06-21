// Package extractionx is the on-demand terminal-extraction family
// (terminal.extract — risk "read"; terminal.extract.async — risk "local"). Both
// point the small model at one or more Daintree terminals, read a bounded tail,
// and pull out caller-specified content as plain text or structured JSON. Raw
// scrollback never enters the main agent's context; only the extracted result
// does. terminal.extract is inline (reads once, or polls a `wait` condition then
// extracts); terminal.extract.async runs the same poll+extract in the background
// and publishes the outcome to the attention queue, OUTLIVING the turn (so it must
// not carry the turn's cancellation). The poll loop reuses the watcher DSL
// vocabulary; modelJudge conditions are intentionally unsupported here (they would
// re-run the classifier every tick). Spec: docs/port/tools-families.md §4.9.
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
	AgentState   string
	RecentOutput *string // nil when absent; "" is a valid "no output yet"
	ExitCode     *int
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

// ChatMessage / ChatResult mirror the small-model chat surface.
type ChatMessage struct {
	Role    string
	Content string
}

// ChatResult carries the content plus finishReason ("length" ⇒ token-cap truncation).
type ChatResult struct {
	Content      string
	FinishReason string
}

// Router is the slice of model access this family uses: Chat (text extraction +
// verdict) and JSON (structured extraction). JSON returns the parsed `result`
// value (the model emits {"result": <value>}); the caller serializes it.
type Router interface {
	Chat(ctx context.Context, tier domain.ModelTier, messages []ChatMessage, maxTokens int) (ChatResult, error)
	JSON(ctx context.Context, tier domain.ModelTier, messages []ChatMessage, maxTokens int) (any, error)
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
		newExtractAsyncTool(deps),
	}
}
