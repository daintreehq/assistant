// Package backend is the native Daintree Assistant backend client — the CLI's ONLY
// model gateway. It replaces the direct provider model client for assistant turns and
// utility tasks.
//
// The CLI is a thin local runtime: it stores the visible conversation, exposes and
// executes local function tools, and ships structured startup/runtime/turn context.
// The backend owns the system prompt, developer instructions, runbook
// selection, model choice, prompt assembly, and the utility-model prompts — and it
// reaches every model THROUGH OPENROUTER, funded by an upstream credential the SERVER
// holds. The CLI ships none, for the main loop or for utility tasks. Model names that
// appear in this repo are OpenRouter route ids, never direct provider integrations. The wire contract here is Daintree-native (NOT
// OpenAI-compatible) and strict: the request schema rejects system/developer messages
// and unknown fields, so these structs deliberately emit only the fields the backend
// accepts.
//
// Reference: ../assistant-backend/docs/DAINTREE_API.md and the pydantic models in
// ../assistant-backend/src/daintree_assistant_server/contracts/.
package backend

import (
	"encoding/json"
	"math"
)

// ProtocolVersion is the Daintree wire protocol the CLI speaks. The backend
// advertises a supported range via /version and /v1/daintree/capabilities; a
// mismatch yields HTTP 426.
//
// 3 is the runbook protocol, and it is a HARD break from 2 rather than an
// addition: the response block, the selection field, the routes and the warning
// codes all say "runbook" where 2 said "skill", and nothing answers to the old
// spelling. The backend pins PROTOCOL_MIN to 3 for exactly that reason — a
// protocol-2 client is refused at the door instead of being served a body it
// would parse into an empty runbooks block and silently render as "no runbooks
// loaded". Both halves move together; there is no version in between.
const ProtocolVersion = 3

// DefaultBaseURL is the deployed backend, and the endpoint a fresh install uses.
//
// AN ANONYMOUS PRINCIPAL IS A VALID CALLER, and stays one — it is what a local backend,
// the e2e fakes, and any deployment short of `enforce` serve, and the backend funds every
// turn from its own upstream credential either way. What this comment must NOT do is say
// it is the only caller HERE: this endpoint is a secured staging deployment whose identity
// posture moves by configuration, so any sentence naming what it asks for today can be
// made wrong by a revision of it that changes no code here. A deployment that configures accounts (see internal/auth) gets the
// account's access token on protected paths; that is the deployment's answer, discovered
// per endpoint, not a property of this build. `DAINTREE_API_KEY` remains supported on top of either, and is unset on a normal
// install — but it is not a way to PAY. It wins over this client's account manager, and
// the backend either ignores it entirely (open mode) or verifies it as an ACCOUNT token;
// every model call is funded by the server's own credential regardless.
//
// (This comment once described a mandatory per-caller API key that funded the turn. That
// is gone in both halves: the CLI's own key flow was deleted with the backend migration,
// and the backend now funds every call from `serving_api_key`. Account sign-in exists
// again — see internal/auth — but it answers who is calling, not what pays. See
// docs/BACKEND.md, which is the live account of both.)
const DefaultBaseURL = "https://assistant.daintree.org"

// LocalBaseURL is the local development backend (`python -m daintree_assistant_server`
// from ../assistant-backend). It is what DAINTREE_BACKEND_URL is usually pointed at for
// the dev loop, e2e tests and benchmarks, and it is what `/backend local` selects.
//
// The env var is NOT the whole mechanism, and believing it is sends someone hunting an
// exported variable to explain an endpoint that nothing exported. Four sources resolve,
// highest first: `--backend-url`, the trusted `DAINTREE_BACKEND_URL`, the preference
// `/backend` PERSISTED on disk (internal/config/endpoint.go — a 0600 endpoint.json at
// the per-user state root holding only `{backend_url}`, a preference and never a
// credential), and DefaultBaseURL above. Env deliberately outranks the stored preference
// so a harness, an e2e run or CI is never silently redirected by a choice a human made
// months ago in an interactive session — and because that ordering otherwise reads as a
// broken `/backend`, cfg.BackendURLPinnedByEnv exists so the command can say so. Every
// one of the four goes through NormalizeBaseURL (endpoint.go), which is the single door.
const LocalBaseURL = "http://127.0.0.1:8473"

// AllowsUnverifiedSignIn reports whether an endpoint is excused from proving it can
// spend a credential — i.e. whether a missing /v1/daintree/auth/verify route is a
// diagnostic FAILURE or a benign gap (see the doctor check in internal/cli).
//
// Only a LOOPBACK endpoint qualifies. The reasoning is about which way the test fails
// when it is wrong:
//
//   - The obvious formulation, "is this the official endpoint?", fails OPEN. Its alias
//     surface is unbounded — `:443`, an empty port, a trailing DNS root dot, an IDNA
//     spelling, userinfo — and every spelling the check does not anticipate silently
//     takes the LENIENT path and blesses a remote host that cannot answer for itself.
//   - "Is this loopback?" fails CLOSED. There is no `evil.com` spelling that parses to
//     127.0.0.1, an unparseable URL is treated as remote, and the worst outcome of a
//     miss is that a developer's own backend has to serve one more route.
//
// So every REMOTE endpoint — official, staging, or custom — is held to the full
// contract, and the lenient path exists only for the `python -m daintree_assistant_server`
// development loop, where there is no network to intercept and no third party to trust.
func AllowsUnverifiedSignIn(baseURL string) bool {
	return IsLoopbackURL(baseURL)
}

// --------------------------------------------------------------------------
// Request: POST /v1/daintree/respond
// --------------------------------------------------------------------------

// Request profiles (RespondRequest.Profile). The empty string is the wire default
// and means ProfileAssistant — the CLI omits the field on every ordinary turn so an
// orchestrator request is byte-identical to one sent before this field existed.
const (
	// ProfileAssistant is the orchestrator persona: the full base prompt, runbook
	// selection, and the whole runtime/turn context surface.
	ProfileAssistant = "assistant"
	// ProfileSubagent is the read-only worker persona: a short standalone prompt, NO
	// runbook selection, and only the startup block for context. See internal/subagent.
	ProfileSubagent = "subagent"
)

// RespondRequest is the single request body for the generation endpoint. The
// backend validates it with extra="forbid" at the top level, so every field here
// must be one the backend knows; optional sub-objects are pointers with omitempty so an
// absent one is never sent as null. Startup is the required value exception and therefore
// always serializes, including as {} when discovery is unavailable.
type RespondRequest struct {
	ProtocolVersion int            `json:"protocol_version"`
	Session         RespondSession `json:"session"`
	State           *string        `json:"state,omitempty"`
	// Profile names the persona that answers this request: "" (⇒ the backend's
	// "assistant" default) for the orchestrator the human talks to, ProfileSubagent
	// for a bounded read-only worker running in its own isolated conversation
	// (internal/subagent). The backend swaps the whole system prompt on it and skips
	// runbook selection entirely for a sub-agent, so it is NOT a hint — it selects
	// which of two different assembly paths runs. omitempty is load-bearing: every
	// ordinary turn must stay byte-identical on the wire, or the prompt cache the
	// stable prefix exists to protect splits in two.
	Profile    string          `json:"profile,omitempty"`
	Startup    StartupContext  `json:"startup"`
	Input      RespondInput    `json:"input"`
	Runtime    *RuntimeContext `json:"runtime,omitempty"`
	Turn       *TurnContext    `json:"turn,omitempty"`
	Selection  *Selection      `json:"selection,omitempty"`
	Generation *Generation     `json:"generation,omitempty"`
	Client     *ClientInfo     `json:"client,omitempty"`
	// Routing is the caller's endpoint-selection preference. Omitted by almost every
	// request, which is what keeps the server default in force. See routing.go.
	Routing *Routing `json:"routing,omitempty"`
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

// RespondSession identifies the conversation and turn so the backend's runbook
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

// Selection controls the backend's runbook-selection cadence. policy
// "new_instruction" (the default) re-runs selection on a new turn / interjection /
// missing-state; "always" forces it every round.
type Selection struct {
	Policy string `json:"policy,omitempty"` // "new_instruction" | "always"
	Force  bool   `json:"force,omitempty"`
	// PinnedRunbookIDs names runbooks the CALLER requires this turn, whatever the
	// backend's classifier picks. `Force` only means "re-run the selector this
	// round", so before this field there was no way to say "load THIS one" — which
	// is what makes a runbook under development testable: a failure is then the
	// runbook, not an unlucky selection.
	//
	// omitempty is LOAD-BEARING, not tidiness. Selection is validated with
	// extra="forbid", so sending the key to a deployment that predates it 422s the
	// whole turn before the model opens. Nothing may populate this without first
	// seeing Capabilities.Runbooks.PinnedRunbookIDs from the endpoint about to be
	// called (App.backendAcceptsPinnedRunbookIDs).
	//
	// Ids that are unknown, or not executable under this request's profile, are
	// DROPPED and reported in the meta event's Warnings — never a 422, because
	// whether a pin fits depends on the live catalog and the configured cap, which
	// a request-shape validator cannot see.
	PinnedRunbookIDs []string `json:"pinned_runbook_ids,omitempty"`
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
	// Display is how wide the reply will actually render. Omitted when the CLI has no
	// terminal to measure, which the backend answers with its own default width — so
	// an absent block means "unknown", never "narrow".
	Display *DisplayInfo `json:"display,omitempty"`
}

// DisplayInfo is the client's live render geometry. content_width is the load-bearing
// value — the measure the assistant's own markdown is wrapped at — and the backend
// shapes the response contract (prose length, whether a pipe table can fit) around it;
// columns is the raw terminal, sent so the model can answer questions about the window
// it is running in without inferring one number from the other. Both are cells, and
// both are optional on the backend: a surface that knows its wrap width but not its
// window (a future non-attached session publisher) sends content_width alone rather than a
// guessed pair.
//
// Bounded by displayWidthMax to mirror the backend's validation: the request is
// validated BEFORE it is used, so an absurd width from a confused terminal probe would
// 422 the whole turn rather than degrade to a default.
type DisplayInfo struct {
	Columns      int `json:"columns,omitempty"`
	ContentWidth int `json:"content_width,omitempty"`
}

// displayWidthMax mirrors the backend contract's DisplayInfo bound (extensions.py).
// Far above any real terminal, low enough that a garbage value is clamped, not sent.
const displayWidthMax = 10000

// NewDisplayInfo builds the wire block from a measured geometry, clamping both values
// into the range the backend validates. Returns nil when the content width is
// unmeasured or unusable, so an unknown surface omits the block entirely instead of
// asserting a width nobody measured.
func NewDisplayInfo(columns, contentWidth int) *DisplayInfo {
	clamp := func(v int) int {
		if v < 0 {
			return 0
		}
		if v > displayWidthMax {
			return displayWidthMax
		}
		return v
	}
	contentWidth = clamp(contentWidth)
	if contentWidth == 0 {
		return nil
	}
	return &DisplayInfo{Columns: clamp(columns), ContentWidth: contentWidth}
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
// names (runbook__find/runbook__load/daintree_internal__*).
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
// Response: non-streaming body + the first-class runbooks block
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
	// Cost is the whole request's spend. Absent ⇒ unknown, never free. See TurnCost.
	Cost *TurnCost `json:"cost"`
	// Timings is where the request's wall clock went, by phase. Absent ⇒ the backend
	// does not report timings. See TurnTimings.
	Timings         *TurnTimings  `json:"timings"`
	Runbooks        RunbooksBlock `json:"runbooks"`
	State           string        `json:"state"`
	CatalogRevision string        `json:"catalog_revision"`
	PromptVersion   string        `json:"prompt_version"`
	Warnings        []string      `json:"warnings"`
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
	// Cost is what this ONE call charged the credential the BACKEND spends, in USD —
	// the deployment's money, reported down so a session can show what it consumed. A
	// POINTER because nil means "the provider reported nothing", which is emphatically
	// not "free": coercing it to 0 would
	// quietly under-report a running total. Never compute this client-side; the router
	// knows which of ~24 endpoints served the call and what cache discount applied, and
	// anything we derived from a token price would be a guess presented as a bill.
	//
	// On a /respond body this covers the MAIN completion only — the turn total is the
	// separate `cost` block. On a /tasks result it IS that task's total (a task may
	// still make two calls when a malformed response needs a repair pass).
	Cost *float64 `json:"cost"`
}

// TurnCost is what a whole /respond request charged the backend's upstream credential,
// in USD, across every upstream call it made: the runbook selector, its repair pass, a
// losing speculative generation, the main completion, and a re-rolled round the user
// never saw.
//
// Two rules a client must IMPLEMENT rather than infer, and both exist to stop a session
// accumulator from quietly under-reporting what a session consumed:
//
//   - The whole block is ABSENT when nothing was reported. Absent means unknown, never
//     free.
//   - `complete: false` means a call that RAN did not report its cost, so Total is a
//     floor rather than a sum. (A turn that SKIPPED the selector stays complete: no call
//     happened, so nothing is missing.) One case is structurally unobservable — a
//     speculative generation cancelled mid-flight was billed, but OpenRouter reports
//     usage only in a stream's final chunk, which a cancelled stream never sends.
//
// The practical consequence is one rule: render a session total as a lower bound if ANY
// turn in it was incomplete or reported no cost at all. These figures are for proportion
// and trend; the OpenRouter dashboard of whoever runs this backend — not the user's own,
// which funds none of it — is the authority on the actual bill.
type TurnCost struct {
	Total    float64  `json:"total"`
	Main     *float64 `json:"main"`
	Selector *float64 `json:"selector"`
	// Complete is a pointer only so an older backend's omission can default to TRUE
	// (the backend's own default) instead of decoding as "incomplete" and marking every
	// turn a lower bound. Read it through IsComplete.
	Complete *bool `json:"complete"`
}

// IsComplete reports whether Total is a full sum rather than a floor. Absent ⇒ true,
// matching the backend's default for a field it only started sending recently.
func (c *TurnCost) IsComplete() bool {
	if c == nil || c.Complete == nil {
		return true
	}
	return *c.Complete
}

// TurnTimings is where a request's wall clock went, by phase, measured SERVER-side
// around real awaits (so each figure includes the queueing and network the user
// actually waited through, not the provider's own latency accounting).
//
// Every field is a POINTER, and that is the contract rather than Go pedantry. The
// backend serializes with exclude_none, so a phase that did not happen is a MISSING
// key — never 0. A selector that never ran and a selector that answered instantly are
// different facts, and decoding the first as zero merges them into a lie that reads
// like a measurement. PreparationMs and TotalMs are the two the backend promises on
// every turn; they are pointers anyway so that a backend WITHOUT this block (the
// deployed one, until this ships) cannot produce a log line claiming a 0 ms turn.
//
// The phases do NOT sum to TotalMs. They overlap deliberately: a speculative upstream
// stream opens while the selector is still running, so SelectionMs and UpstreamOpenMs
// can cover the same wall clock. Read each as "how long did this part take", never as
// a partition — and never render them as a stacked bar.
type TurnTimings struct {
	// SelectionMs is the runbook-selector call, including a parse-repair round trip when
	// one ran. Absent on a tool-continuation round, where selection is skipped by
	// design — so its absence across a turn's later rounds is the healthy shape.
	SelectionMs *int `json:"selection_ms"`
	// DocsMs is the documentation lookup, when the selector asked for one.
	DocsMs *int `json:"docs_ms"`
	// PreparationMs is request-in → upstream request built: selection, docs, state
	// verification and prompt assembly. The share of the wait the backend owns outright,
	// and the number to read against UpstreamOpenMs when asking where a slow turn went.
	PreparationMs *int `json:"preparation_ms"`
	// UpstreamOpenMs is request-in → the model's first event; mostly prefill. A SMALL
	// value next to a large SelectionMs means a winning speculation hid the open, not a
	// fast model. Absent on a non-streamed call, where one await covers opening and
	// generating and splitting it would be a guess presented as a measurement.
	UpstreamOpenMs *int `json:"upstream_open_ms"`
	// ThinkingMs is the first chain-of-thought fragment → the first visible token.
	// Absent on every normal turn: the whole interactive surface is non-thinking. A
	// value here means that posture changed.
	ThinkingMs *int `json:"thinking_ms"`
	// FirstOutputMs is request-in → the first visible token. The headline number — what
	// a user means by "it started answering".
	FirstOutputMs *int `json:"first_output_ms"`
	// GenerationMs is the first visible token → complete. Pure generation.
	GenerationMs *int `json:"generation_ms"`
	// TotalMs is the whole server-side wait for this ONE request. A retried round bills
	// and measures per attempt, so this covers the winning attempt only — which is why a
	// client-observed round duration can legitimately exceed it.
	TotalMs *int `json:"total_ms"`
}

// Any reports whether the block carries at least one measured phase. A backend that
// sends `timings` but populates nothing (or an older one that omits it entirely) must
// not produce an empty timings record in a log or a caller's accounting.
func (t *TurnTimings) Any() bool {
	if t == nil {
		return false
	}
	return t.SelectionMs != nil || t.DocsMs != nil || t.PreparationMs != nil ||
		t.UpstreamOpenMs != nil || t.ThinkingMs != nil || t.FirstOutputMs != nil ||
		t.GenerationMs != nil || t.TotalMs != nil
}

// UnmarshalJSON decodes the block and NEVER returns an error. That is not laziness
// about malformed input — it is the whole point.
//
// These numbers are telemetry. They arrive on the terminal `done` event, which the SSE
// parser decodes strictly: one `json.Unmarshal` failure anywhere in that event aborts
// the stream, and the turn — already generated, already streamed to the user, already
// BILLED upstream — fails. Letting a diagnostic field have that power is
// indefensible, and the failure is not hypothetical: every field is a `*int`, so the
// backend dropping a single `round()` and reporting `5775.3` would kill every turn,
// as would any future string-valued field. A phase we cannot parse is reported the same
// way as a phase that did not happen — absent — which is exactly the right answer.
//
// Fields are parsed INDEPENDENTLY, so one bad value costs only its own phase rather
// than the whole block.
func (t *TurnTimings) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		// Not even an object (`"timings": "soon"`, an array, a number). Nothing to
		// salvage; leave every phase absent.
		return nil
	}
	for key, target := range map[string]**int{
		"selection_ms":     &t.SelectionMs,
		"docs_ms":          &t.DocsMs,
		"preparation_ms":   &t.PreparationMs,
		"upstream_open_ms": &t.UpstreamOpenMs,
		"thinking_ms":      &t.ThinkingMs,
		"first_output_ms":  &t.FirstOutputMs,
		"generation_ms":    &t.GenerationMs,
		"total_ms":         &t.TotalMs,
	} {
		v, ok := raw[key]
		if !ok {
			continue
		}
		if ms, ok := parseMillis(v); ok {
			*target = &ms
		}
	}
	return nil
}

// parseMillis reads one millisecond figure, tolerating a float where an int is
// expected (the backend rounds today, but a refactor that stops rounding must cost a
// number's precision, not the user's turn). Reports false for anything that is not a
// finite number in range.
func parseMillis(raw json.RawMessage) (int, bool) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	f, err := n.Float64()
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	// A NEGATIVE duration is not a slow phase, it is a broken clock or a wrong sign, and
	// logging it would put an impossible measurement next to real ones. Converting an
	// out-of-range float to int is separately implementation-defined in Go, so bound the
	// top too rather than log whatever the hardware happens to produce.
	if f < 0 || f > math.MaxInt32 {
		return 0, false
	}
	return int(math.Round(f)), true
}

// --------------------------------------------------------------------------
// Streaming event payloads (named SSE events: meta / delta / done / error)
// --------------------------------------------------------------------------

// StreamMeta is the FIRST streamed event — always before any token. It carries
// the refreshed opaque state token (resend on the next request), the runbooks
// outcome (active set + newly-loaded titles the client surfaces), and version
// markers.
type StreamMeta struct {
	ProtocolVersion int           `json:"protocol_version"`
	RequestID       string        `json:"request_id"`
	Model           string        `json:"model"`
	Runbooks        RunbooksBlock `json:"runbooks"`
	State           string        `json:"state"`
	CatalogRevision string        `json:"catalog_revision"`
	PromptVersion   string        `json:"prompt_version"`
	Warnings        []string      `json:"warnings"`
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
	// Cost rides the TERMINAL event because that is the earliest it can be known:
	// OpenRouter reports a stream's usage only in its final chunk. Absent ⇒ unknown.
	Cost *TurnCost `json:"cost"`
	// Timings rides the terminal event for the same reason cost does: `meta` is emitted
	// BEFORE the model is opened — which is exactly what makes meta useful — so it
	// cannot know generation or total. A client logging a turn reads this, never meta.
	Timings *TurnTimings `json:"timings"`
}

// --------------------------------------------------------------------------
// Server-side context compaction (the `compaction` stream event)
// --------------------------------------------------------------------------

// ContextCompactionBlockName is the reserved `name` the compacted block wears, and
// the whole reason server-side compaction needs no server-side state: the client
// splices the block into its history and sends the name back verbatim on every later
// request, so the backend's span selector can see where frozen history ends by
// reading the array it was handed. Honoured by the backend only on a `user` message.
const ContextCompactionBlockName = "daintree_compaction"

// StreamCompactionSpan is the half-open range of `input.messages` the block stands
// in for — indices into the array the client itself sent on THIS request, so the
// splice needs no negotiation:
//
//	messages[:StartIndex] + [block] + messages[EndIndex:]
//
// EndIndex is EXCLUSIVE and always greater than StartIndex (an empty span is never
// emitted). The assistant reply currently streaming is not in that array.
// Both fields are POINTERS because both have a legitimate zero. `start_index: 0` is
// the ordinary case — a span opening at the very first message — so a plain int cannot
// tell it apart from a field the payload never sent. That distinction is load-bearing
// here in a way it usually is not: an absent start silently read as 0 would widen the
// span back to the beginning of the conversation and destroy history the server never
// asked to replace. Absent means invalid, and Bounds says so.
type StreamCompactionSpan struct {
	StartIndex *int `json:"start_index"`
	EndIndex   *int `json:"end_index"`
}

// Bounds returns the half-open span, reporting false when either edge was missing.
func (s StreamCompactionSpan) Bounds() (start, end int, ok bool) {
	if s.StartIndex == nil || s.EndIndex == nil {
		return 0, 0, false
	}
	return *s.StartIndex, *s.EndIndex, true
}

// StreamCompactionBlock is the reconciled message that replaces the span. Role is
// always "user" and Name always ContextCompactionBlockName; both are checked rather
// than assumed, because a block that fails either would move the next turn's
// compaction boundary somewhere the server never intended.
type StreamCompactionBlock struct {
	Role    string `json:"role"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

// StreamPreamble is the `preamble` event: a short visible preview of the work about
// to happen, written by a fast model while the executor request is still being
// assembled. Emitted at most once per turn, after `meta` and before the executor
// produces anything.
//
// It is its OWN event rather than a `delta`, and each reason is something that
// breaks if it is folded in:
//
//   - It is PROVISIONAL. Nothing is committed to conversation history until the
//     terminal `done`; a turn that dies mid-stream must leave no trace of it.
//   - It is not the retry boundary. The client replays a failure that arrives
//     before the first executor token, and preamble text is server-generated,
//     idempotent intent — treating it as visible content would make every turn
//     that showed one un-retryable for no reason. A replayed attempt sends a
//     fresh preamble, which REPLACES the one on screen rather than appending.
//   - Bare-intent detection reads executor output. A preamble announces intended
//     work by design and would be a guaranteed false positive.
//
// The backend also appends these exact bytes to the executor's own input as its
// prior assistant turn, so the visible text and the model's view of its last turn
// are the same text. That is why the client commits ONE assistant message joining
// preamble and executor content with a blank line: two messages would say the
// conversation had a turn the executor never saw.
//
// Carries no provider or model identity: the product is Daintree.
type StreamPreamble struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	// Provisional and CommitOn are decoded AND enforced: the parser drops any event
	// that is not `true` / `"done"`. This client implements exactly one policy — hold
	// it provisional, commit it on `done` — so a backend stating different terms is
	// describing a contract we do not implement, and rendering the text anyway would
	// show a preview under rules its sender never agreed to.
	Provisional bool   `json:"provisional"`
	CommitOn    string `json:"commit_on"`
}

// StreamCompaction is the `compaction` event: reconciled state standing in for old
// history. Emitted at most once per turn, immediately BEFORE the terminal `done` and
// never after it — `done` staying terminal is deliberate, because an SSE reader
// modelled on the `data: [DONE]` convention breaks its loop on the terminal event and
// would silently drop anything following. Entirely optional: a turn that compacted
// nothing simply has no such event and the client keeps the history it already holds.
type StreamCompaction struct {
	// TurnID is the turn this block describes. A client applies the splice only when
	// it matches the turn it asked about — a block is a statement about one
	// conversation prefix at one moment, not a durable fact.
	TurnID   string                `json:"turn_id"`
	Replaces StreamCompactionSpan  `json:"replaces"`
	Block    StreamCompactionBlock `json:"block"`
}

// ContextCompactionSpanCaps is the advertised index convention for Replaces. Every
// field is checked by ReplayCompatible rather than trusted: these are the assumptions
// the splice arithmetic is built on, and a backend that changed one of them without
// the client noticing would splice the wrong messages away.
type ContextCompactionSpanCaps struct {
	Collection string `json:"collection"`
	// IndexBase is a pointer for the same reason the span's edges are: the value this
	// client requires IS zero, so a plain int would read a descriptor that never
	// mentioned index_base as though it had promised zero-based indices — opening the
	// gate on a contract nobody stated.
	IndexBase            *int `json:"index_base"`
	EndExclusive         bool `json:"end_exclusive"`
	ExcludesCurrentReply bool `json:"excludes_current_reply"`
}

// ContextCompactionCaps is the top-level `context_compaction` capability block —
// a SIBLING of `respond`, not a field inside it. A nil pointer means the deployment
// predates the feature entirely; a non-nil block with Enabled false means it
// advertises the contract but has no compactor wired (the state of every real
// deployment today, where only the null compactor is installed). The two are
// different answers and the CLI keeps them distinguishable, though it withholds
// compaction for both.
type ContextCompactionCaps struct {
	Enabled              bool                      `json:"enabled"`
	StreamEvent          string                    `json:"stream_event"`
	Delivery             string                    `json:"delivery"`
	AtMostOnce           bool                      `json:"at_most_once"`
	StreamingOnly        bool                      `json:"streaming_only"`
	BestEffort           bool                      `json:"best_effort"`
	AppendOnly           bool                      `json:"append_only"`
	BlockMessageName     string                    `json:"block_message_name"`
	Span                 ContextCompactionSpanCaps `json:"span"`
	TurnIDMatchRequired  bool                      `json:"turn_id_match_required"`
	MaxBlockContentBytes int                       `json:"max_block_content_bytes"`
}

// ReplayCompatible reports whether this deployment advertises the EXACT contract the
// local splice implements — not merely that compaction is enabled.
//
// Checking the whole block rather than Enabled alone is the point. The splice is a
// destructive rewrite of the client's own working history driven by integers the
// server chose, and every field below is an assumption that arithmetic depends on:
// which array the indices address, whether the end is exclusive, whether the reply
// being streamed is in range, what name marks the boundary for the NEXT turn. A
// backend that revises one of them ships a block this code would apply wrongly, and
// a silent wrong splice is far worse than no compaction at all — so an unrecognised
// contract reads as "not supported" and the turn runs on full history.
func (c *ContextCompactionCaps) ReplayCompatible() bool {
	if c == nil || !c.Enabled {
		return false
	}
	return c.StreamEvent == "compaction" &&
		c.Delivery == "before_done" &&
		c.AtMostOnce &&
		c.StreamingOnly &&
		c.BestEffort &&
		c.AppendOnly &&
		c.BlockMessageName == ContextCompactionBlockName &&
		c.Span.Collection == "input.messages" &&
		c.Span.IndexBase != nil && *c.Span.IndexBase == 0 &&
		c.Span.EndExclusive &&
		c.Span.ExcludesCurrentReply &&
		c.TurnIDMatchRequired &&
		c.MaxBlockContentBytes > 0
}

// --------------------------------------------------------------------------
// First-class runbooks block
// --------------------------------------------------------------------------

// RunbooksBlock is the dynamic-runbook outcome for a turn. NONE of it is folded into the
// conversation: NewlyLoaded rides the eager OnRunbookLoaded callback to the diagnostic sinks
// (run log, --json, debug trace), while Active + NewlyLoaded + Selector ride the COMMITTED
// OnMeta callback to the same sinks as one per-round runbook:decision event — the
// authoritative record of what the backend actually selected, and the only one that can
// report the active set on a round that loaded nothing or a selector that failed open.
// Prelude alone is decoded but unused. The backend no longer
// injects anything into the upstream transcript — a newly-active runbook reaches
// the model as its body in a "# Loaded runbooks" system message (plain context),
// so Prelude is now vestigial metadata the client neither replays nor renders.
type RunbooksBlock struct {
	Active      []RunbookRef `json:"active"`
	NewlyLoaded []RunbookRef `json:"newly_loaded"`
	Prelude     Prelude      `json:"prelude"`
	Selector    SelectorMeta `json:"selector"`
}

// RunbookRef identifies one active/loaded runbook. Runbooks are UNVERSIONED by design
// (change-busting rides the catalog content hash), so the backend's RunbookRef
// carries only id + title — there is no version field to decode.
type RunbookRef struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Prelude is optional runbook-load metadata the backend still emits. The client decodes
// but never replays or renders it; it is vestigial pending a coordinated server-side drop.
type Prelude struct {
	ToolExecutions []PreludeExecution `json:"tool_executions"`
}

// PreludeExecution is one runbook-load call + its result, with a display name. Part
// of the vestigial Prelude metadata — decoded but not rendered by the client.
type PreludeExecution struct {
	Call        PreludeToolCall   `json:"call"`
	Result      PreludeToolResult `json:"result"`
	DisplayName string            `json:"display_name"`
}

// PreludeToolCall mirrors a ToolCall for the synthetic runbook-load exchange.
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

// SelectorMeta is the runbook selector's telemetry for the turn.
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
// reason, and usage. The agent loop reads State/Runbooks off Meta and appends
// Message to history.
type RespondResult struct {
	Meta         StreamMeta
	Message      RespondMessage
	FinishReason string
	Usage        Usage
	// Cost is the turn's total spend, carried up from the terminal `done` event. nil
	// when the backend reported none — the caller must not read that as zero.
	Cost *TurnCost
	// Timings is the server-side phase breakdown, carried up from the terminal `done`
	// event. On a RETRIED call it describes the WINNING attempt only (each attempt is a
	// separate request with its own clock), so a client-measured round duration that
	// exceeds Timings.TotalMs is the expected shape, not a contradiction.
	Timings *TurnTimings
	// Transport is the CLIENT-side latency of the attempt that produced this result:
	// dial, TLS, upload, first byte back. It is the other half of Timings — the part
	// measured before the server's clock starts and after it stops — and the two are
	// meant to be read together. nil when the attempt never reached the wire. See
	// transport.go.
	Transport *TransportMarks
	// Preamble is the fast preview the user was shown before the executor answered,
	// empty when the turn had none. Released under the SAME commit barrier as
	// Compaction — meta seen and `done` reached — because that barrier IS the
	// contract: the event says commit_on "done", so a stream that ended in an error
	// must hand back nothing to commit.
	//
	// It is ALREADY joined onto the front of Message.Content, separated by a blank
	// line, so every caller that commits or replays the assistant turn does the right
	// thing without knowing this feature exists. The field is kept beside it so a
	// caller that needs to tell the two halves apart still can.
	Preamble string
	// Compaction is the turn's compacted context block, when the backend sent one and
	// the stream reached its terminal `done`. nil is the overwhelmingly common case —
	// no compactor, no valid span, or a deployment that predates the feature.
	//
	// Released ONLY by a committed stream, the same discipline OnMeta follows: a
	// compaction event rides just ahead of `done`, so an attempt that never reached
	// `done` (transport failure, terminal error event, EOF) cannot hand the caller a
	// block, and a retried attempt's discarded block can never be spliced. A stream
	// carrying MORE than one compaction event leaves this nil: at_most_once is part of
	// the contract, and a second block is evidence the client and server disagree about
	// what the first one replaced.
	Compaction *StreamCompaction
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
	// Routing carries the SAME endpoint preference a turn sends, and carrying it here is
	// not a nicety. A task ships the caller's content upstream exactly as a turn does —
	// terminal tails, conversation transcripts, memories — so a privacy choice honoured
	// only on /respond would be kept precisely where the user can see it and dropped
	// everywhere else. Stamped by the client for every task (see Client.RunTask), so a
	// new task call site cannot forget it.
	Routing *Routing `json:"routing,omitempty"`
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
	// Routing reports the ACTIVE endpoint-routing posture and the values a client may
	// select. The description is served rather than composed locally so a client
	// cannot invent its own privacy wording — the difference between "does not train
	// on" and "does not store" is a claim about the user's data, and only one of them
	// is true under the default mode.
	Routing  RoutingCapsBlock `json:"routing"`
	Runbooks struct {
		CatalogRevision string `json:"catalog_revision"`
		ManualResolve   bool   `json:"manual_resolve"`
		// PinnedRunbookIDs advertises that Selection.pinned_runbook_ids is accepted. It is
		// a GATE, in the same sense as Respond.DisplayContext: Selection is
		// extra="forbid" server-side, so a client that guesses wrong loses the entire
		// turn rather than one optional field.
		PinnedRunbookIDs bool `json:"pinned_runbook_ids"`
		// Catalog is every runbook the backend can load, as the minimal {id, title}
		// reference, sorted by id. It is the CANONICAL full catalog rather than a
		// profile's executable menu — capabilities carries neither a profile nor a tool
		// inventory, so it cannot honestly narrow — which is why a locally-valid id can
		// still come back `pinned_runbook_not_executable`.
		//
		// nil means the backend does not advertise a catalog at all (it predates the
		// field); a non-nil empty slice means an advertised, genuinely empty one. The
		// two are different answers and callers must not collapse them: the first
		// cannot validate an id, the second knows every id is wrong.
		//
		// Key any cache on CatalogRevision above — same snapshot. That is conservative,
		// not exact: the revision hashes each runbook's body too, so it moves on an edit
		// that leaves this list byte-identical.
		Catalog []RunbookRef `json:"catalog"`
	} `json:"runbooks"`
	// ContextCompaction is the TOP-LEVEL server-side compaction contract (a sibling of
	// `respond`, not a field within it — the backend serves it there). nil on a
	// deployment that predates the feature; see ContextCompactionCaps.ReplayCompatible
	// for why the CLI checks the whole block rather than just `enabled`.
	ContextCompaction *ContextCompactionCaps `json:"context_compaction"`
	Tasks             []string               `json:"tasks"`
	Limits            struct {
		RequestBytes int `json:"request_bytes"`
		Tools        int `json:"tools"`
	} `json:"limits"`
}

// RoutingCapsBlock is the backend's advertised routing posture.
type RoutingCapsBlock struct {
	PrivacyMode        string `json:"privacy_mode"`
	PrivacyDescription string `json:"privacy_description"`
	Sort               string `json:"sort"`
	// ClientSelectable is absent on a backend that does not accept a `routing` block,
	// which is how the CLI knows not to send one.
	ClientSelectable *RoutingSelectable `json:"client_selectable"`
}

// RoutingSelectable enumerates what a client may choose.
type RoutingSelectable struct {
	Field         string   `json:"field"`
	Privacy       []string `json:"privacy"`
	Sort          []string `json:"sort"`
	EndpointLists []string `json:"endpoint_lists"`
	MaxEndpoints  int      `json:"max_endpoints"`
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
	MaxActiveRunbooks      int      `json:"max_active_runbooks"`
	MetadataTransport      string   `json:"metadata_transport"`
	// CostReporting is present when this backend reports what each request cost its
	// upstream credential. Absent on an older deployment — which the CLI handles without needing to
	// ask, since an unreported cost is already indistinguishable from a backend that
	// reports none, and both are rendered as "unknown". Advertised here so `/doctor`
	// can name the contract rather than leave a tester guessing why /cost is empty.
	CostReporting *CostReportingCaps `json:"cost_reporting"`
	// DisplayContext reports that this backend accepts `runtime.display` — the client's
	// terminal geometry — and shapes its response contract around it. It is a GATE, not
	// a nicety: the backend validates `runtime` with extra="forbid", so sending the block
	// to a deployment that predates it 422s the WHOLE turn before the model ever runs.
	// False/absent on an older backend, which is why the CLI withholds the geometry until
	// a handshake says otherwise (App.PromptContext). Delete the gate once no such
	// deployment is reachable.
	DisplayContext bool `json:"display_context"`
}

// CostReportingCaps describes the backend's cost-reporting contract.
type CostReportingCaps struct {
	Field    string `json:"field"`
	Currency string `json:"currency"`
	// Components names the sub-fields of the cost block ("total", "main", "selector",
	// "complete").
	Components  []string `json:"components"`
	StreamEvent string   `json:"stream_event"`
	// AbsentWhenUnknown states the rule the client must implement rather than infer:
	// the block is omitted rather than zero-filled when nothing was reported.
	AbsentWhenUnknown bool `json:"absent_when_unknown"`
	// TotalMayBeIncomplete states that `complete: false` can occur, i.e. that a total
	// is sometimes a floor.
	TotalMayBeIncomplete bool `json:"total_may_be_incomplete"`
}

// Version is the GET /version body.
type Version struct {
	ServerVersion string        `json:"server_version"`
	BuildSHA      string        `json:"build_sha"`
	Protocol      ProtocolRange `json:"protocol"`
}
