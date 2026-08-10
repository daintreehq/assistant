// Package prompts holds MainPromptContext — the structured runtime/environment facts
// the CLI collects and hands to the agent session.
//
// The backend owns the system prompt, developer instructions, skill bodies, and fresh
// runtime/turn rendering. The CLI renders no system message; it projects this carrier
// into the backend's structured request.startup and request.runtime blocks.
package prompts

import "github.com/daintreehq/assistant/internal/domain"

// MainPromptContext is the CLI-collected environment state. Stable project/agent/
// instruction fields build request.startup; live tier/MCP/scheduler/worktree
// fields map to request.runtime each round. PromptContextFunc is pulled live so a
// mid-session MCP change, /permissions tier change, or scheduler start reaches the next
// request. Pinned/recalled memories and session-ended-watchers remain turn context.
type MainPromptContext struct {
	Tier        domain.Tier
	ProjectPath string
	ProjectID   string
	// Project / AgentRoster / Worktree are the richer Daintree-owned snapshot. The app
	// fetches them in parallel while the splash is playing and ships the cached immutable
	// values on every backend round. The session puts project/agents in request.startup
	// and maps worktree to typed request.runtime.worktree. ProjectPath / ProjectID remain
	// useful fallbacks when Daintree is unavailable.
	Project       *ProjectContext
	AgentRoster   *AgentRosterContext
	Worktree      *WorktreeContext
	MCPConnected  bool
	MCPStatusLine string
	// MCPTransport / MCPToolCount carry the connected MCP surface. The backend builds
	// its "connected (<transport> transport, <n> tools)" line from these when connected
	// (it only falls back to MCPStatusLine when NOT connected), so they must be populated
	// or the model is told the MCP has 0 tools.
	MCPTransport string
	MCPToolCount *int
	// MCPServers is the CONFIGURED integration surface: one entry per MCP server this
	// process is wired to, each naming its ENDPOINT. Without it the model cannot say
	// WHICH Daintree it is driving — in ses_8cb40b4e it was asked for the MCP URL and
	// could only guess ("typically served at something like http://localhost:PORT/mcp").
	// Deliberately identity-only (name + endpoint + role): the backend renders this as a
	// session-STABLE system block (prompt_assembler build_main_request step 2, ahead of
	// the tool schemas), so anything that fluctuates mid-session — connected, transport,
	// tool count — must stay on MCPConnected/MCPTransport/MCPToolCount, which ride the
	// volatile runtime tail instead. Endpoints are immutable for the life of a client
	// (mcp.Client resolves them once in New), so this block is constant per session.
	MCPServers          []MCPServerContext
	SchedulerActive     bool
	ProjectInstructions string
}

// MCPServerContext is one connected-to MCP server as the CLI reports it. URL is the
// sanitized endpoint (mcp.SanitizeURL — no userinfo/query/fragment, since Daintree's
// per-session URL can carry the bearer as a query param); Description is the one-line
// role the backend renders after the name.
type MCPServerContext struct {
	Name        string
	URL         string
	Description string
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
