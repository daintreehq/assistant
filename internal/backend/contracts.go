// Package backend is the native Daintree Assistant backend client. It replaces
// the direct DeepSeek/OpenAI model client for assistant turns and utility tasks.
//
// The CLI is a thin local runtime: it stores the visible conversation, exposes and
// executes local function tools, and ships structured startup/runtime/turn context.
// The backend owns the system prompt, developer instructions, skill/runbook
// selection, model choice, prompt assembly, and the utility-model prompts — and
// it talks to DeepSeek/OpenAI internally. The wire contract here is
// Daintree-native (NOT OpenAI-compatible) and strict: the request schema rejects
// system/developer messages and unknown fields, so these structs deliberately
// emit only the fields the backend accepts.
//
// Reference: ../assistant-backend/docs/DAINTREE_API.md and the pydantic models in
// ../assistant-backend/src/daintree_assistant_server/contracts/.
package backend

import "encoding/json"

// ProtocolVersion is the Daintree wire protocol the CLI speaks. The backend
// advertises a supported range via /version and /v1/daintree/capabilities; a
// mismatch yields HTTP 426.
const ProtocolVersion = 2

// DefaultBaseURL is the production backend endpoint — the default the first-run
// login flow offers. Resolution lives in config.LoadConfig: an explicit override
// or DAINTREE_BACKEND_URL (the dev/test escape hatch, e.g. the local backend on
// 127.0.0.1:8473) wins, then the persisted /login endpoint, then this constant.
// The API key travels only with the persisted login's own endpoint (see
// AppConfig.BackendAPIKey).
const DefaultBaseURL = "https://assistant.daintree.org"

// --------------------------------------------------------------------------
// Request: POST /v1/daintree/respond
// --------------------------------------------------------------------------

// RespondRequest is the single request body for the generation endpoint. The
// backend validates it with extra="forbid" at the top level, so every field here
// must be one the backend knows; optional sub-objects are pointers with omitempty so an
// absent one is never sent as null. Startup is the required value exception and therefore
// always serializes, including as {} when discovery is unavailable.
type RespondRequest struct {
	ProtocolVersion int             `json:"protocol_version"`
	Session         RespondSession  `json:"session"`
	State           *string         `json:"state,omitempty"`
	Startup         StartupContext  `json:"startup"`
	Input           RespondInput    `json:"input"`
	Runtime         *RuntimeContext `json:"runtime,omitempty"`
	Turn            *TurnContext    `json:"turn,omitempty"`
	Selection       *Selection      `json:"selection,omitempty"`
	Generation      *Generation     `json:"generation,omitempty"`
	Client          *ClientInfo     `json:"client,omitempty"`
}

// StartupContext is the stable, cache-friendly Daintree snapshot collected while the
// splash animation is visible. It is a required value on every generation request: an
// unavailable discovery serializes as {}, never as null. The backend places it before
// the visible conversation and treats every value as inert, untrusted project data.
type StartupContext struct {
	Project             *ProjectSnapshot     `json:"project,omitempty"`
	AgentRoster         *AgentRosterSnapshot `json:"agent_roster,omitempty"`
	ProjectInstructions string               `json:"project_instructions,omitempty"`
}

// ProjectSnapshot is the deliberately narrow subset of project.getCurrent that is
// useful on ordinary turns. The boolean pointers preserve unknown/false/true.
type ProjectSnapshot struct {
	ID                    string `json:"id,omitempty"`
	Name                  string `json:"name,omitempty"`
	Path                  string `json:"path,omitempty"`
	Status                string `json:"status,omitempty"`
	DaintreeConfigPresent *bool  `json:"daintree_config_present,omitempty"`
	InRepoSettings        *bool  `json:"in_repo_settings,omitempty"`
}

// AgentRosterSnapshot is one authoritative direct-agent registry read. Agents is not
// omitempty so a successful empty read remains [] rather than disappearing.
type AgentRosterSnapshot struct {
	Agents               []AgentSnapshot `json:"agents"`
	Complete             bool            `json:"complete"`
	AvailabilityComplete bool            `json:"availability_complete"`
	TotalCount           int             `json:"total_count"`
}

// AgentSnapshot preserves the exact registered identifier and required provenance.
// Pointer booleans retain the registry's tri-state semantics; absent availability remains
// unknown.
type AgentSnapshot struct {
	ID             string `json:"id"`
	DisplayName    string `json:"display_name,omitempty"`
	Source         string `json:"source"`
	Availability   string `json:"availability,omitempty"`
	Installed      *bool  `json:"installed,omitempty"`
	Launchable     *bool  `json:"launchable,omitempty"`
	Pinned         *bool  `json:"pinned,omitempty"`
	ToolbarVisible *bool  `json:"toolbar_visible,omitempty"`
}

// RespondSession identifies the conversation and turn so the backend's skill
// state and selector cadence have a stable key. All four fields are accepted;
// instruction_revision/round default to 0 and are omitted when zero.
type RespondSession struct {
	ID                  string `json:"id"`
	TurnID              string `json:"turn_id"`
	InstructionRevision int    `json:"instruction_revision,omitempty"`
	Round               int    `json:"round,omitempty"`
}

// RespondInput is the visible conversation plus the client's current tool inventory.
// Stable discovery belongs in RespondRequest.Startup and must never be inserted here.
// Messages must be non-empty and carry only user/assistant/tool roles.
type RespondInput struct {
	Messages   []Message `json:"messages"`
	Tools      []Tool    `json:"tools,omitempty"`
	ToolChoice any       `json:"tool_choice,omitempty"` // "auto"|"none"|"required" | ToolChoiceNamed
}

// ToolChoiceNamed is the {"name": ...} flattened form of forcing a specific tool.
type ToolChoiceNamed struct {
	Name string `json:"name"`
}

// Generation carries only the generation params the backend supports — it
// validates with extra="forbid", so any unknown key is rejected. Stream is a
// plain bool (no omitempty) so the streaming intent is always explicit.
type Generation struct {
	Temperature    *float64 `json:"temperature,omitempty"`
	MaxTokens      *int     `json:"max_tokens,omitempty"`
	Stop           []string `json:"stop,omitempty"`
	ResponseFormat string   `json:"response_format,omitempty"` // "text" | "json_object"
	Stream         bool     `json:"stream"`
}

// Selection controls the backend's skill-selection cadence. policy
// "new_instruction" (the default) re-runs selection on a new turn / interjection /
// missing-state; "always" forces it every round.
type Selection struct {
	Policy string `json:"policy,omitempty"` // "new_instruction" | "always"
	Force  bool   `json:"force,omitempty"`
}

// ClientInfo identifies the CLI build for the backend's telemetry.
type ClientInfo struct {
	Name     string `json:"name,omitempty"`
	Version  string `json:"version,omitempty"`
	Platform string `json:"platform,omitempty"`
}

// --------------------------------------------------------------------------
// Runtime + turn context (server-rendered as inert data, NOT system prompts)
// --------------------------------------------------------------------------

// RuntimeContext is the CLI-reported environment. The backend renders it as inert
// data in the per-request (uncached) prompt tail. scheduler_active defaults to
// true on the backend, so it is sent WITHOUT omitempty — an inactive scheduler
// must be representable as an explicit false.
type RuntimeContext struct {
	PermissionTier  string                   `json:"permission_tier,omitempty"`
	MCP             *MCPInfo                 `json:"mcp,omitempty"`
	MCPServers      []MCPServer              `json:"mcp_servers,omitempty"`
	SchedulerActive bool                     `json:"scheduler_active"`
	Worktree        *CurrentWorktreeSnapshot `json:"worktree,omitempty"`
	OpenTerminals   []OpenTerminal           `json:"open_terminals,omitempty"`
}

// CurrentWorktreeSnapshot preserves the three states of the live read. A nil Runtime
// Worktree means the read was unavailable; {"current":null} is a successful, definitive
// "none selected" response; a non-nil Current carries the full useful snapshot.
type CurrentWorktreeSnapshot struct {
	Current *WorktreeSnapshot `json:"current"`
}

// WorktreeSnapshot is the useful subset of worktree.getCurrent, normalized and bounded
// by the CLI before it reaches the backend's strict request validator.
type WorktreeSnapshot struct {
	ID          string `json:"id,omitempty"`
	Path        string `json:"path,omitempty"`
	Branch      string `json:"branch,omitempty"`
	IsMain      bool   `json:"is_main"`
	IssueNumber *int   `json:"issue_number,omitempty"`
	IssueTitle  string `json:"issue_title,omitempty"`
	PRNumber    *int   `json:"pr_number,omitempty"`
	PRTitle     string `json:"pr_title,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	Status      string `json:"status,omitempty"`
	LastCommit  string `json:"last_commit,omitempty"`
}

// OpenTerminal is one live Daintree terminal in the per-turn inventory the CLI attaches
// to the runtime block, so the model always sees the open-terminal roster as inert data
// instead of tool-calling terminal.list mid-turn to discover it. Metadata only — never
// terminal output. The list fields (id/kind/worktree/title/agent) come from a single
// terminal.list; AgentState/WaitingReason/ExitCode are refreshed from one no-output
// terminal.getStatus. ExitCode is a pointer because 0 is a meaningful clean exit that
// must be distinguishable from "no exit code".
type OpenTerminal struct {
	ID            string `json:"id"`
	Kind          string `json:"kind,omitempty"`
	WorktreeID    string `json:"worktree_id,omitempty"`
	Title         string `json:"title,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	AgentState    string `json:"agent_state,omitempty"`
	WaitingReason string `json:"waiting_reason,omitempty"`
	ExitCode      *int   `json:"exit_code,omitempty"`
}

// Per-field length limits (rune counts) for OpenTerminal. These MUST mirror the backend
// OpenTerminal pydantic max_length constraints (contracts/extensions.py). The backend
// VALIDATES the request before it sanitizes/caps for the prompt, so an over-limit field —
// e.g. a verbose, agent-authored terminal title or a long waiting reason — would 422 the
// WHOLE request and break the turn, defeating the best-effort inventory. The CLI (the only
// client) clamps to these limits before sending so that can never happen.
const (
	openTerminalIDMax            = 256
	openTerminalKindMax          = 64
	openTerminalWorktreeIDMax    = 4096
	openTerminalTitleMax         = 512
	openTerminalAgentIDMax       = 256
	openTerminalAgentStateMax    = 64
	openTerminalWaitingReasonMax = 512
)

// clampRunes truncates s to at most max runes (Unicode code points), matching how pydantic
// counts max_length, so the clamp is exact against the backend's validation.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

// Clamp returns a copy with every string field truncated to its backend max_length, so a
// long agent-controlled value can never trip the backend's pre-sanitization length
// validation and 422 the request. ids are short terminal-<uuid> values far under the limit,
// so clamping the id cannot collapse two distinct terminals in practice.
func (t OpenTerminal) Clamp() OpenTerminal {
	t.ID = clampRunes(t.ID, openTerminalIDMax)
	t.Kind = clampRunes(t.Kind, openTerminalKindMax)
	t.WorktreeID = clampRunes(t.WorktreeID, openTerminalWorktreeIDMax)
	t.Title = clampRunes(t.Title, openTerminalTitleMax)
	t.AgentID = clampRunes(t.AgentID, openTerminalAgentIDMax)
	t.AgentState = clampRunes(t.AgentState, openTerminalAgentStateMax)
	t.WaitingReason = clampRunes(t.WaitingReason, openTerminalWaitingReasonMax)
	return t
}

// MCPInfo is a coarse connectivity summary for the primary MCP surface.
type MCPInfo struct {
	Connected bool   `json:"connected"`
	Transport string `json:"transport,omitempty"`
	ToolCount *int   `json:"tool_count,omitempty"`
	Status    string `json:"status,omitempty"`
}

// MCPServer is one MCP server the CLI is connected to, as the CLI reports it. The
// backend renders the name/description/instructions as inert, escape-neutralized
// data so they cannot inject instructions.
type MCPServer struct {
	Name         string `json:"name"`
	Transport    string `json:"transport,omitempty"`
	Status       string `json:"status,omitempty"`
	ToolCount    *int   `json:"tool_count,omitempty"`
	Description  string `json:"description,omitempty"`
	Instructions string `json:"instructions,omitempty"`
}

// TurnContext is the per-turn context the old footer message used to carry as
// prose; the backend now takes it as structured data and renders the footer.
type TurnContext struct {
	Goal         string   `json:"goal,omitempty"`
	IsWake       bool     `json:"is_wake,omitempty"`
	WorkflowRuns []string `json:"workflow_runs,omitempty"`
	// AsyncOperations are the live runtime-owned async invocations (one
	// pre-formatted line each), re-read every round so the model always sees its
	// own in-flight async work — and never re-issues it after a compaction.
	AsyncOperations []string  `json:"async_operations,omitempty"`
	Memories        *Memories `json:"memories,omitempty"`
	// ResumedWatchers are the titles of live watchers adopted from a prior owner
	// at ownership boot (project-scoped supervision resumed automatically) —
	// surfaced once, on the first turn. Replaces the pre-supervisor
	// session_ended_watchers field; the backend contract renames in lockstep.
	ResumedWatchers []string `json:"resumed_watchers,omitempty"`
	// WorkflowState carries compact digests of the ACTIVE client-owned workflow
	// graphs (the workflow-intelligence layer), re-read every round like the
	// async ledger, so the model always sees what it already planned, did, and
	// is waiting on — and never redoes completed work after a compaction or
	// wake. Populated ONLY when workflow intelligence is enabled
	// (DAINTREE_WORKFLOW_INTELLIGENCE=1): the backend validates TurnContext with
	// extra="forbid", so a backend without the matching contract must never see
	// the field (omitempty keeps the wire byte-identical when the feature is off).
	WorkflowState []WorkflowDigest `json:"workflow_state,omitempty"`
}

// WorkflowDigest is one bounded, prompt-ready summary of a workflow graph.
// Field names/limits MUST mirror the backend's WorkflowDigest pydantic model
// (contracts/extensions.py): the backend validates BEFORE it sanitizes, so an
// over-limit field would 422 the whole turn — the CLI clamps first (Clamp).
type WorkflowDigest struct {
	ID          string   `json:"id"`
	Goal        string   `json:"goal"`
	Status      string   `json:"status"`
	Progress    string   `json:"progress,omitempty"`
	ActiveNodes []string `json:"active_nodes,omitempty"`
	Resources   []string `json:"resources,omitempty"`
	Blockers    []string `json:"blockers,omitempty"`
	NextAction  string   `json:"next_action,omitempty"`
	LastEvent   string   `json:"last_event,omitempty"`
}

// Per-field rune limits for WorkflowDigest (mirror the backend max_length
// constraints) plus the digest-list caps the rollout contract fixes.
const (
	workflowDigestIDMax   = 128
	workflowDigestGoalMax = 512
	workflowDigestStatMax = 64
	workflowDigestLineMax = 512

	// MaxWorkflowDigests caps how many workflow digests ride one turn context.
	MaxWorkflowDigests = 5
	// MaxWorkflowStateBytes caps the serialized workflow_state block; whole
	// trailing digests are dropped (never a partial cut) until it fits.
	MaxWorkflowStateBytes = 16384
)

// Clamp returns a copy with every string bounded to its backend max_length so
// a verbose graph field can never 422 the turn.
func (d WorkflowDigest) Clamp() WorkflowDigest {
	d.ID = clampRunes(d.ID, workflowDigestIDMax)
	d.Goal = clampRunes(d.Goal, workflowDigestGoalMax)
	d.Status = clampRunes(d.Status, workflowDigestStatMax)
	d.Progress = clampRunes(d.Progress, workflowDigestLineMax)
	d.NextAction = clampRunes(d.NextAction, workflowDigestLineMax)
	d.LastEvent = clampRunes(d.LastEvent, workflowDigestLineMax)
	d.ActiveNodes = clampLines(d.ActiveNodes, workflowDigestLineMax)
	d.Resources = clampLines(d.Resources, workflowDigestLineMax)
	d.Blockers = clampLines(d.Blockers, workflowDigestLineMax)
	return d
}

// clampLines rune-bounds every entry of a string list (nil-safe, in a copy).
func clampLines(in []string, max int) []string {
	if len(in) == 0 {
		return in
	}
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = clampRunes(s, max)
	}
	return out
}

// CapWorkflowDigests enforces the digest-list contract: clamp every digest,
// keep at most MaxWorkflowDigests, then drop WHOLE trailing digests until the
// serialized block fits MaxWorkflowStateBytes. Order is the caller's ranking
// (most relevant first), so the tail is always the casualty.
func CapWorkflowDigests(in []WorkflowDigest) []WorkflowDigest {
	if len(in) == 0 {
		return nil
	}
	out := make([]WorkflowDigest, 0, len(in))
	for _, d := range in {
		out = append(out, d.Clamp())
		if len(out) == MaxWorkflowDigests {
			break
		}
	}
	for len(out) > 0 {
		b, err := json.Marshal(out)
		if err == nil && len(b) <= MaxWorkflowStateBytes {
			break
		}
		out = out[:len(out)-1]
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Memories splits recalled context into pinned (durable) and relevant (per-turn
// BM25 recall) buckets.
type Memories struct {
	Pinned   []string `json:"pinned,omitempty"`
	Relevant []string `json:"relevant,omitempty"`
}

// --------------------------------------------------------------------------
// Messages, tool calls, tool defs (canonical OpenAI-ish shapes the backend reuses)
// --------------------------------------------------------------------------

// Message is one visible-conversation message. Content is raw JSON so it can be a
// string, a multimodal parts array, or an explicit null (an assistant tool-call
// turn) — exactly mirroring the local wire encoder. Roles are user/assistant/tool
// ONLY; the converter rejects system/developer before a request is built.
type Message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content,omitempty"`
	Name    string          `json:"name,omitempty"`
	// ReasoningContent is the assistant turn's chain-of-thought, replayed verbatim.
	// DeepSeek REQUIRES it on every subsequent request for any assistant turn that
	// performed a tool call (omitting it 400s the whole request); it is optional and
	// ignored for assistant turns without tool calls. omitempty so a non-thinking turn
	// (the default posture) sends nothing and the wire is byte-identical to before.
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one function call the model emitted (or one replayed in history).
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // always "function"
	Function FunctionCall `json:"function"`
}

// FunctionCall is the name + raw-JSON-string arguments of a tool call.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is a function tool definition offered to the backend. The backend bounds
// the total tool bytes, schema depth, and property count, and rejects reserved
// names (skill__find/skill__load/daintree_internal__*).
type Tool struct {
	Type     string      `json:"type"` // "function"
	Function FunctionDef `json:"function"`
}

// FunctionDef is the name/description/JSON-schema parameters of a tool.
type FunctionDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// --------------------------------------------------------------------------
// Response: non-streaming body + the first-class skills block
// --------------------------------------------------------------------------

// RespondResponse is the non-streaming response body. (The CLI streams in normal
// operation; this exists for completeness and tests.)
type RespondResponse struct {
	ProtocolVersion int            `json:"protocol_version"`
	RequestID       string         `json:"request_id"`
	Model           string         `json:"model"`
	Message         RespondMessage `json:"message"`
	FinishReason    string         `json:"finish_reason"`
	Usage           Usage          `json:"usage"`
	Skills          SkillsBlock    `json:"skills"`
	State           string         `json:"state"`
	CatalogRevision string         `json:"catalog_revision"`
	PromptVersion   string         `json:"prompt_version"`
	Warnings        []string       `json:"warnings"`
}

// RespondMessage is the assistant message in a non-streaming response.
// ReasoningContent is present only when thinking is active (exclude_none on the
// server), so a non-thinking response decodes identically to before.
type RespondMessage struct {
	Role             string     `json:"role"`
	Content          string     `json:"content"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls"`
}

// Usage is the token accounting the backend reports.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CachedTokens     int `json:"cached_tokens"`
}

// --------------------------------------------------------------------------
// Streaming event payloads (named SSE events: meta / delta / done / error)
// --------------------------------------------------------------------------

// StreamMeta is the FIRST streamed event — always before any token. It carries
// the refreshed opaque state token (resend on the next request), the skills
// outcome (active set + newly-loaded titles the client surfaces), and version
// markers.
type StreamMeta struct {
	ProtocolVersion int         `json:"protocol_version"`
	RequestID       string      `json:"request_id"`
	Model           string      `json:"model"`
	Skills          SkillsBlock `json:"skills"`
	State           string      `json:"state"`
	CatalogRevision string      `json:"catalog_revision"`
	PromptVersion   string      `json:"prompt_version"`
	Warnings        []string    `json:"warnings"`
}

// StreamDelta is one streamed chunk: visible content, chain-of-thought, and/or
// OpenAI-style tool-call delta fragments (accumulated in sse.go). ReasoningContent
// fragments stream before the first Content fragment (DeepSeek thinking mode) and
// are concatenated the same way Content is; they only appear when thinking is active.
type StreamDelta struct {
	Content          string          `json:"content,omitempty"`
	ReasoningContent string          `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCallDelta `json:"tool_calls,omitempty"`
}

// StreamStatus is the optional `status` event the backend emits once, the instant
// chain-of-thought begins (phase "thinking"). It never appears when thinking is off.
// Unknown future phase values are ignorable.
type StreamStatus struct {
	Phase string `json:"phase"`
}

// ToolCallDelta is one streamed tool-call fragment, passed through verbatim from
// the upstream model. Fragments for the same call share an index; id/name arrive
// once and argument text streams in pieces.
type ToolCallDelta struct {
	Index    *int   `json:"index"`
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	Function struct {
		Name      string `json:"name,omitempty"`
		Arguments string `json:"arguments,omitempty"`
	} `json:"function"`
}

// StreamDone terminates a successful stream. usage is always present (the backend
// dumps it without omit), with zeros when the upstream reported nothing.
type StreamDone struct {
	FinishReason string `json:"finish_reason"`
	Usage        Usage  `json:"usage"`
}

// --------------------------------------------------------------------------
// First-class skills block
// --------------------------------------------------------------------------

// SkillsBlock is the dynamic-skill outcome for a turn. The CLI surfaces ONLY the
// NewlyLoaded refs through the eager OnSkillLoaded callback; Active, Prelude, and
// Selector are decoded off the wire but not rendered. The backend no longer
// injects anything into the upstream transcript — a newly-active skill reaches
// the model as its body in a "# Loaded skills" system message (plain context),
// so Prelude is now vestigial metadata the client neither replays nor renders.
type SkillsBlock struct {
	Active      []SkillRef   `json:"active"`
	NewlyLoaded []SkillRef   `json:"newly_loaded"`
	Prelude     Prelude      `json:"prelude"`
	Selector    SelectorMeta `json:"selector"`
}

// SkillRef identifies one active/loaded skill. Skills are UNVERSIONED by design
// (change-busting rides the catalog content hash), so the backend's SkillRef
// carries only id + title — there is no version field to decode.
type SkillRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Prelude is optional skill-load metadata the backend still emits. The client
// decodes but does NOT render it (the "Skill loaded" card is built from
// NewlyLoaded titles); it is vestigial pending a coordinated server-side drop.
type Prelude struct {
	ToolExecutions []PreludeExecution `json:"tool_executions"`
}

// PreludeExecution is one skill-load call + its result, with a display name. Part
// of the vestigial Prelude metadata — decoded but not rendered by the client.
type PreludeExecution struct {
	Call        PreludeToolCall   `json:"call"`
	Result      PreludeToolResult `json:"result"`
	DisplayName string            `json:"display_name"`
}

// PreludeToolCall mirrors a ToolCall for the synthetic skill-load exchange.
type PreludeToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// PreludeToolResult mirrors a tool-result message for the synthetic exchange.
type PreludeToolResult struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	Content    string `json:"content"`
}

// SelectorMeta is the skill selector's telemetry for the turn.
type SelectorMeta struct {
	Ran        bool     `json:"ran"`
	Degraded   bool     `json:"degraded"`
	TaskType   string   `json:"task_type,omitempty"`
	Confidence *float64 `json:"confidence,omitempty"`
	Usage      *Usage   `json:"usage,omitempty"`
	Reason     string   `json:"reason,omitempty"`
}

// --------------------------------------------------------------------------
// Aggregated streaming result
// --------------------------------------------------------------------------

// FinishReasonLength is the provider finish reason for a generation cut off by
// the output-token cap. The agent loop uses it to diagnose a parse-failed final
// tool call as truncation (re-issue the amputated work) rather than a JSON
// syntax slip (re-encode the same call).
const FinishReasonLength = "length"

// RespondResult is the accumulated outcome of a streamed respond call: the meta
// event, the assembled assistant message (content + tool calls), the finish
// reason, and usage. The agent loop reads State/Skills off Meta and appends
// Message to history.
type RespondResult struct {
	Meta         StreamMeta
	Message      RespondMessage
	FinishReason string
	Usage        Usage
}

// HasToolCalls reports whether the assistant asked to run any tools.
func (r RespondResult) HasToolCalls() bool { return len(r.Message.ToolCalls) > 0 }

// --------------------------------------------------------------------------
// Tasks: POST /v1/daintree/tasks
// --------------------------------------------------------------------------

// TaskRequest is the named utility-task envelope. Clients send task DATA only —
// the backend owns the prompt, model, schema, and output mode. extra="forbid" on
// the backend rejects any attempt to smuggle messages/system/developer here.
type TaskRequest struct {
	Task         string         `json:"task"`
	RequestID    string         `json:"request_id,omitempty"`
	Input        map[string]any `json:"input,omitempty"`
	ResultSchema map[string]any `json:"result_schema,omitempty"` // only terminal_extract_json
}

// TaskResult is the typed utility-task response. Output is raw JSON decoded by the
// caller into the task-specific output struct.
type TaskResult struct {
	ID            string          `json:"id"`
	Object        string          `json:"object"`
	Task          string          `json:"task"`
	Model         string          `json:"model"`
	Output        json.RawMessage `json:"output"`
	FinishReason  string          `json:"finish_reason"`
	Usage         Usage           `json:"usage"`
	PromptVersion string          `json:"prompt_version"`
}

// --------------------------------------------------------------------------
// Capabilities / version
// --------------------------------------------------------------------------

// Capabilities is the GET /v1/daintree/capabilities body — protocol range,
// limits, stream events, and available task ids.
type Capabilities struct {
	ServerVersion string           `json:"server_version"`
	Protocol      ProtocolRange    `json:"protocol"`
	Respond       RespondCapsBlock `json:"respond"`
	Skills        struct {
		CatalogRevision string `json:"catalog_revision"`
		ManualResolve   bool   `json:"manual_resolve"`
	} `json:"skills"`
	Tasks  []string `json:"tasks"`
	Limits struct {
		RequestBytes int `json:"request_bytes"`
		Tools        int `json:"tools"`
	} `json:"limits"`
}

// ProtocolRange is the inclusive supported protocol-version range.
type ProtocolRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// RespondCapsBlock is the respond-endpoint capability summary.
type RespondCapsBlock struct {
	Endpoint               string   `json:"endpoint"`
	Model                  string   `json:"model"`
	Streaming              bool     `json:"streaming"`
	StreamEvents           []string `json:"stream_events"`
	SystemMessagesAccepted bool     `json:"system_messages_accepted"`
	MaxActiveSkills        int      `json:"max_active_skills"`
	MetadataTransport      string   `json:"metadata_transport"`
}

// Version is the GET /version body.
type Version struct {
	ServerVersion string        `json:"server_version"`
	BuildSHA      string        `json:"build_sha"`
	Protocol      ProtocolRange `json:"protocol"`
}
