// Package mcpx is the Daintree MCP tool family: discovery (daintree.status,
// daintree.listTools, tool.search), the raw passthrough escape hatch
// (daintree.call), and the typed MCP wrappers — terminal
// focus/input/arming, agent focus, and copyTree. Each wrapper carries the risk
// class Daintree gates the action at, so reads/UI-focus run without the
// system-tier confirmation the raw escape hatch always forces.
package mcpx

import (
	"context"
	"encoding/json"

	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// noArgs is the shared empty-object JSON Schema for no-argument tools (the raw
// form of tools.NoArgs, since Tool.Schema is json.RawMessage).
var noArgs = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)

// MCPStatus mirrors the Daintree MCP connection status (mcp.status()). Works even
// when disconnected so the model can reason about degraded mode.
type MCPStatus struct {
	Connected bool   `json:"connected"`
	Transport string `json:"transport,omitempty"`
	ToolCount *int   `json:"toolCount,omitempty"`
	// URL is the sanitized endpoint this client connects to (no userinfo/query/
	// fragment — Daintree's per-session URL can carry the bearer as a query param).
	// It answers "which Daintree am I driving — the local app, or something else?",
	// which the model previously had to guess at.
	URL   string `json:"url,omitempty"`
	Error string `json:"error,omitempty"`
}

// MCPToolInfo is one discovered Daintree MCP tool (listTools entries).
type MCPToolInfo struct {
	Name        string
	Description string
}

// MCPClient is the slice of the Daintree MCP transport these tools reach. It is a
// LOCAL consumer-defined interface (NOT internal/ports.MCPClient, whose CallTool
// signature differs) so this package compiles in isolation. CallTool forwards the
// args map verbatim; Connected gates the call; Status works disconnected;
// ListTools(force) lists the catalog. The concrete mcp.Client satisfies this by
// structural match (the wiring layer adapts if signatures drift).
type MCPClient interface {
	Connected() bool
	Status() MCPStatus
	CallTool(ctx context.Context, name string, args map[string]any) (MCPCallResult, error)
	ListTools(ctx context.Context, force bool) ([]MCPToolInfo, error)
}

// MCPCallResult is the Daintree MCP call envelope (text + optional structured
// content + isError).
type MCPCallResult struct {
	Text              string `json:"text"`
	StructuredContent any    `json:"structuredContent,omitempty"`
	IsError           bool   `json:"isError"`
}

// CommandObserver receives a mark whenever a wrapper injects input into a
// terminal (terminal.sendCommand, copyTree.injectToTerminal). It feeds the
// session's cross-call settle memory (internal/tools/terminalobs): the in-turn
// waits seed their working→waiting gate from earlier working observations, and
// an input injection must INVALIDATE that evidence — the new input may start a
// new task the old observations say nothing about. Consumer-defined so this
// package compiles in isolation; nil disables recording.
type CommandObserver interface {
	MarkCommandSent(terminalID string, at int64)
}

// Deps wires the MCP family. ActiveToolNames is read off ToolContext per-turn for
// the `callable` discovery flag, so it is not duplicated here.
type Deps struct {
	MCP MCPClient
	// Observer records input injections into the shared settle memory. nil ⇒ no
	// recording (tests / stripped tool sets).
	Observer CommandObserver
}

// Tools returns the MCP family (discovery + passthrough + the wrappers in this
// slice).
func Tools(deps Deps) []tools.Tool {
	return []tools.Tool{
		newStatusTool(deps),
		newListToolsTool(deps),
		newSearchTool(deps),
		newCallTool(deps),
		newTerminalFocusTool(deps),
		newTerminalRenameTool(deps),
		newCopyTreeGenerateTool(deps),
		newTerminalSendCommandTool(deps),
		newTerminalCloseTool(deps),
		newTerminalArmTool(deps),
		newTerminalDisarmTool(deps),
		newTerminalDisarmAllTool(deps),
		newCopyTreeInjectTool(deps),
		newCopyTreeGenerateAndCopyFileTool(deps),
	}
}
