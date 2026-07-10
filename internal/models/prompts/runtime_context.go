// Package prompts holds MainPromptContext — the structured runtime/environment facts
// the CLI collects and hands to the agent session.
//
// The backend owns the system prompt, developer instructions, skill bodies, and fresh
// runtime/turn rendering. The CLI renders no system message; it projects this carrier
// into one framed stable user-role data message plus the backend's existing structured
// request.runtime block.
package prompts

import "github.com/daintreehq/daintree-assistant/internal/domain"

// MainPromptContext is the CLI-collected environment state. Stable project/agent/
// instruction fields build the request-only startup row; live tier/MCP/scheduler/worktree
// fields map to request.runtime each round. PromptContextFunc is pulled live so a
// mid-session MCP change, /permissions tier change, or scheduler start reaches the next
// request. Pinned/recalled memories and session-ended-watchers remain turn context.
type MainPromptContext struct {
	Tier        domain.Tier
	ProjectPath string
	ProjectID   string
	// Project / AgentRoster / Worktree are the richer Daintree-owned snapshot. The app
	// fetches them in parallel while the splash is playing and ships the cached immutable
	// values on every backend round. The session puts project/agents in a request-only
	// user-role message before visible history and maps worktree to the existing runtime
	// label. ProjectPath / ProjectID remain useful fallbacks when Daintree is unavailable.
	Project       *ProjectContext
	AgentRoster   *AgentRosterContext
	Worktree      *WorktreeContext
	MCPConnected  bool
	MCPStatusLine string
	// MCPTransport / MCPToolCount carry the connected MCP surface. The backend builds
	// its "connected (<transport> transport, <n> tools)" line from these when connected
	// (it only falls back to MCPStatusLine when NOT connected), so they must be populated
	// or the model is told the MCP has 0 tools.
	MCPTransport        string
	MCPToolCount        *int
	SchedulerActive     bool
	ProjectInstructions string
}

// ProjectContext is the small, broadly-useful subset of project.getCurrent. Deliberately
// exclude recency/frecency, icon/color, and arbitrary project settings: they do not help
// ordinary orchestration, change more often, and project settings may contain environment
// values that should not be attached to every model request.
type ProjectContext struct {
	ID                    string
	Name                  string
	Path                  string
	Status                string
	DaintreeConfigPresent *bool
	InRepoSettings        *bool
}

// AgentRosterContext wraps a successful discovery result so an empty catalog remains
// distinguishable from an unavailable read (nil MainPromptContext.AgentRoster).
type AgentRosterContext struct {
	Agents               []AgentContext
	Complete             bool
	AvailabilityComplete bool
	TotalCount           int
}

// AgentContext is one registered direct-agent candidate. Installed, Launchable, and
// ToolbarVisible are pointers because availability may still be hydrating and user/plugin
// agents do not have built-in toolbar entries. Pinned is Daintree's tri-state explicit
// intent; ToolbarVisible is the resolved answer to "is it in the main toolbar right now?".
type AgentContext struct {
	ID             string
	DisplayName    string
	Source         string
	Availability   string
	Installed      *bool
	Launchable     *bool
	Pinned         *bool
	ToolbarVisible *bool
}

// WorktreeContext is the complete useful worktree.getCurrent summary. Present=false means
// Daintree returned null (no current row, or its renderer store had not resolved one); a
// nil pointer means the read itself was unavailable.
type WorktreeContext struct {
	Present     bool
	ID          string
	Path        string
	Branch      string
	IsMain      bool
	IssueNumber *int
	IssueTitle  string
	PRNumber    *int
	PRTitle     string
	PRURL       string
	Status      string
	LastCommit  string
}
