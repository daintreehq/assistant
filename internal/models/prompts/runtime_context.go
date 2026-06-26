// Package prompts holds MainPromptContext — the structured runtime/environment facts
// the CLI collects and hands to the agent session.
//
// The backend OWNS all prompt text now (the system prompt, developer instructions,
// skill bodies, runtime context, and turn footer). The CLI renders NO system message,
// so the former prompt builders + prompt-text constants that lived here are gone; only
// this data carrier remains (it is mapped to the backend's structured request.runtime
// block in agent.buildRuntimeContext).
package prompts

import "github.com/daintreehq/daintree-assistant/internal/domain"

// MainPromptContext is the CLI-collected runtime/environment state. It is mapped to the
// backend's structured runtime context each round; the backend renders it as inert data.
// It is pulled live per round (PromptContextFunc) so a mid-session MCP (re)connect, a
// /permissions tier change, or a started scheduler reaches the backend on the next
// request. Volatile per-turn state (active worktree, pinned/recalled memories, the
// session-ended-watchers note) does NOT live here — it rides the structured turn context.
type MainPromptContext struct {
	Tier          domain.Tier
	ProjectPath   string
	ProjectID     string
	MCPConnected  bool
	MCPStatusLine string
	// MCPTransport / MCPToolCount carry the connected MCP surface. The backend builds
	// its "connected (<transport> transport, <n> tools)" line from these when connected
	// (it only falls back to MCPStatusLine when NOT connected), so they must be populated
	// or the model is told the MCP has 0 tools.
	MCPTransport        string
	MCPToolCount        *int
	ConfiguredAgentIDs  []string
	SchedulerActive     bool
	ProjectInstructions string
}
