// Package contextx is the read-only orchestration-helper family: compact
// main-thread snapshots (context.snapshot) and cheap terminal reads/summaries
// (terminal.read VERBATIM, terminal.summarize via the small model). All risk
// "read". These keep the main model's context clean by collapsing Daintree state
// and raw scrollback into terse digests instead of dumping everything inline.
// context.snapshot is best-effort and must NEVER throw even when Daintree MCP is
// down; terminal.summarize/read fail cleanly when MCP is unavailable.
package contextx

import (
	"context"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// MCPStatus mirrors the Daintree MCP connection status (mcp.status()).
type MCPStatus struct {
	Connected bool   `json:"connected"`
	Transport string `json:"transport,omitempty"`
	ToolCount *int   `json:"toolCount,omitempty"`
	Error     string `json:"error,omitempty"`
}

// MCPCallResult is the Daintree MCP call envelope.
type MCPCallResult struct {
	Text              string `json:"text"`
	StructuredContent any    `json:"structuredContent,omitempty"`
	IsError           bool   `json:"isError"`
}

// MCPClient is the slice of the Daintree MCP transport this family reaches.
type MCPClient interface {
	Connected() bool
	Status() MCPStatus
	CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error)
}

// Router is the model access this family uses. Summarize maps to the backend's
// terminal_summarize.v1 task (the backend owns the prompt); the CLI sends only the
// purpose + terminal tail.
type Router interface {
	Summarize(ctx context.Context, purpose, tail string) (string, error)
}

// Queue is the slice of the attention queue context.snapshot reads (the open
// inbox). Digest returns the events; Format renders them to a human digest.
type Queue interface {
	Digest(opts domain.QueueDigestOptions) []domain.QueueEvent
	Format(events []domain.QueueEvent) string
}

// Deps wires the context family.
type Deps struct {
	MCP    MCPClient
	Router Router
	Queue  Queue
}

const (
	codeMCPUnavailable = "MCP_UNAVAILABLE"
	codeCancelled      = "CANCELLED"
	codeTerminalOutput = "TERMINAL_OUTPUT"
	codeSummarize      = "SUMMARIZE"
)

// Tools returns the context family.
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newSnapshotTool(deps),
		newSummarizeTool(deps),
		newReadTool(deps),
	}
}

/* ------------------------------- helpers --------------------------------- */

// tryCall is a best-effort MCP read: returns (result, true) on a clean call, or
// (_, false) when disconnected / errored / aborted. context.snapshot must never
// throw, so every failure degrades this read to "unavailable".
func tryCall(ctx context.Context, mcp MCPClient, name string, args map[string]any) (MCPCallResult, bool) {
	if mcp == nil || !mcp.Connected() {
		return MCPCallResult{}, false
	}
	res, err := mcp.CallTool(ctx, name, args)
	if err != nil || res.IsError {
		return MCPCallResult{}, false
	}
	return res, true
}

// readTerminalTail reads a terminal's scrollback tail via terminal.getOutput (no
// model). Scrollback may arrive in structuredContent.content OR the raw text body
// (Daintree uses the latter) — read both, falling back to raw text.
func readTerminalTail(ctx context.Context, mcp MCPClient, terminalID string, maxLines int) (string, error) {
	out, err := mcp.CallTool(ctx, "terminal.getOutput", map[string]any{"terminalId": terminalID, "maxLines": maxLines})
	if err != nil {
		return "", err
	}
	if out.IsError {
		msg := out.Text
		if msg == "" {
			msg = "terminal returned an error"
		}
		return "", fmt.Errorf("%s", msg)
	}
	if sc, ok := out.StructuredContent.(map[string]any); ok {
		if content, ok := sc["content"].(string); ok {
			return content, nil
		}
	}
	return out.Text, nil
}

func contentOrNil(res MCPCallResult, ok bool) any {
	if !ok {
		return nil
	}
	return map[string]any{"text": res.Text, "structuredContent": res.StructuredContent}
}
