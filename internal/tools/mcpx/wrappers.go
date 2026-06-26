package mcpx

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/tools"
)

// The wrappers below cover Daintree actions previously reachable only through the
// raw daintree.call escape hatch. Each carries the risk class Daintree gates the
// action at, so reads/UI-focus run without the system-tier confirmation the escape
// hatch always forces. `options` / `arguments` are opaque records forwarded
// verbatim (we deliberately do NOT model their keys).

/* ------------------------------ terminal.focus ---------------------------- */

type focusArgs struct {
	TerminalID string `json:"terminalId"`
}

var focusSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to focus in the UI." }
  },
  "required": ["terminalId"]
}`)

func newTerminalFocusTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name:        "terminal.focus",
		Description: "Focus/reveal a Daintree terminal in the UI (read-only side effect on the UI; no state mutation).",
		Risk:        domain.RiskUI,
		Schema:      focusSchema,
		Decode:      tools.StrictDecoder(func() any { return &focusArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a focusArgs
			_ = json.Unmarshal(raw, &a)
			// Daintree has no `terminal.focus` MCP tool — terminals are panels, so the
			// correct call is `panel.focus` with the terminal id as the panelId.
			return passthrough(ctx, deps.MCP, "panel.focus", map[string]any{"panelId": a.TerminalID}, "")
		},
	}
}

/* ------------------------------ terminal.rename --------------------------- */

type renameArgs struct {
	TerminalID string `json:"terminalId"`
	Name       string `json:"name"`
}

var renameSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Daintree terminal id to rename (the id field from terminal.list)." },
    "name": { "type": "string", "description": "New tab title. Required: a non-empty name renames programmatically. Keep it short and descriptive (e.g. \"merge pipeline: PRs #10779-84\")." }
  },
  "required": ["terminalId", "name"]
}`)

func newTerminalRenameTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.rename",
		Description: "Set a terminal/agent tab's title. This is how you ORGANISE the workspace: when several agents are all " +
			"named \"Claude\" (or you've lost track of a fleet), rename each to what it is actually doing so the tabs read at " +
			"a glance. Pass terminalId + a short name; rename many in ONE batched round (one call per id, all in the same turn). " +
			"UI-only, no confirmation. Typed wrapper around the Daintree terminal.rename MCP tool — don't reach for it via " +
			"tool.search or daintree.call.",
		Risk:   domain.RiskUI,
		Schema: renameSchema,
		Decode: tools.StrictDecoder(func() any { return &renameArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a renameArgs
			_ = json.Unmarshal(raw, &a)
			// Daintree's raw terminal.rename treats BOTH args as optional: an omitted
			// terminalId targets the *focused* terminal, and an omitted name opens the
			// interactive rename dialog instead of renaming
			// (terminalLifecycleActions.ts). Both are wrong for an orchestrator — it
			// must rename a SPECIFIC terminal programmatically, never pop a dialog or
			// silently retarget the focused tab — so the wrapper requires both and
			// rejects blanks with a recoverable validation error.
			if strings.TrimSpace(a.TerminalID) == "" || strings.TrimSpace(a.Name) == "" {
				return tools.Fail(domain.CodeValidation, "terminal.rename: terminalId and a non-empty name are both required.")
			}
			return passthrough(ctx, deps.MCP, "terminal.rename",
				map[string]any{"terminalId": a.TerminalID, "name": a.Name}, "")
		},
	}
}

/* ------------------------- copyTree.* shared shapes ----------------------- */

// copyTreeOptions is forwarded verbatim; the keys are Daintree's, not ours.
type copyTreeGenerateArgs struct {
	WorktreeID string         `json:"worktreeId,omitempty"`
	Options    map[string]any `json:"options,omitempty"`
}

var copyTreeGenerateSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "worktreeId": { "type": "string", "description": "Worktree to generate the copy tree for; Daintree uses the active worktree when omitted." },
    "options": { "type": "object", "additionalProperties": true, "description": "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys)." }
  },
  "required": []
}`)

// copyTreeForwardMap rebuilds the verbatim args map forwarded to Daintree: the
// whole parsed args object, so worktreeId/options travel as-is; omitted fields are
// simply absent.
func (a copyTreeGenerateArgs) forwardMap() map[string]any {
	m := map[string]any{}
	if a.WorktreeID != "" {
		m["worktreeId"] = a.WorktreeID
	}
	if a.Options != nil {
		m["options"] = a.Options
	}
	return m
}

func newCopyTreeGenerateTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "copyTree.generate",
		Description: "Generate a Daintree 'copy tree' — a concatenated digest of a worktree's files — and return it as text " +
			"(read-only). Typed wrapper around the Daintree copyTree.generate MCP tool. 'options' is forwarded verbatim; don't invent keys.",
		Risk:   domain.RiskRead,
		Schema: copyTreeGenerateSchema,
		Decode: tools.StrictDecoder(func() any { return &copyTreeGenerateArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a copyTreeGenerateArgs
			_ = json.Unmarshal(raw, &a)
			return passthrough(ctx, deps.MCP, "copyTree.generate", a.forwardMap(), "")
		},
	}
}

func newCopyTreeGenerateAndCopyFileTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "copyTree.generateAndCopyFile",
		Description: "Generate a worktree's copy tree and copy it to the OS clipboard as a file. System tier — always confirms. " +
			"Typed wrapper around the Daintree copyTree.generateAndCopyFile MCP tool. 'options' is forwarded verbatim.",
		Consequence: "Writes the generated file digest to the operating-system clipboard, replacing its current contents.",
		Risk:        domain.RiskSystem,
		Schema:      copyTreeGenerateSchema,
		Decode:      tools.StrictDecoder(func() any { return &copyTreeGenerateArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a copyTreeGenerateArgs
			_ = json.Unmarshal(raw, &a)
			return passthrough(ctx, deps.MCP, "copyTree.generateAndCopyFile", a.forwardMap(), "")
		},
	}
}

type copyTreeInjectArgs struct {
	TerminalID string         `json:"terminalId"`
	WorktreeID string         `json:"worktreeId,omitempty"`
	Options    map[string]any `json:"options,omitempty"`
}

var copyTreeInjectSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Terminal to inject the generated copy tree into." },
    "worktreeId": { "type": "string", "description": "Worktree to generate the copy tree from; Daintree uses the active worktree when omitted." },
    "options": { "type": "object", "additionalProperties": true, "description": "Opaque copyTree options forwarded to Daintree verbatim (do not invent keys)." }
  },
  "required": ["terminalId"]
}`)

func newCopyTreeInjectTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "copyTree.injectToTerminal",
		Description: "Generate a worktree's copy tree and inject it into a Daintree terminal's input. Mutating (writes into a " +
			"terminal), so it always confirms. Typed wrapper around the Daintree copyTree.injectToTerminal MCP tool. 'options' is forwarded verbatim.",
		Consequence: "Pastes a generated file digest into the named terminal's input. May be large; review the target terminal before approving.",
		Risk:        domain.RiskTerminal,
		Schema:      copyTreeInjectSchema,
		Decode:      tools.StrictDecoder(func() any { return &copyTreeInjectArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a copyTreeInjectArgs
			_ = json.Unmarshal(raw, &a)
			// Mirror terminal.sendCommand's guard: a blank terminalId would inject
			// the digest into an unnamed target, so reject it before the MCP call.
			if strings.TrimSpace(a.TerminalID) == "" {
				return tools.Fail(domain.CodeValidation, "copyTree.injectToTerminal: terminalId must be non-empty.")
			}
			m := map[string]any{"terminalId": a.TerminalID}
			if a.WorktreeID != "" {
				m["worktreeId"] = a.WorktreeID
			}
			if a.Options != nil {
				m["options"] = a.Options
			}
			return copyTreeInjectPassthrough(ctx, deps.MCP, a.TerminalID, m)
		},
	}
}

/* --------------------------- terminal.sendCommand ------------------------- */

type sendCommandArgs struct {
	TerminalID string `json:"terminalId"`
	Command    string `json:"command"`
}

var sendCommandSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Terminal to send the command to." },
    "command": { "type": "string", "description": "Shell command text to type into the terminal and run." }
  },
  "required": ["terminalId", "command"]
}`)

func newTerminalSendCommandTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.sendCommand",
		Description: "Send a command line to a Daintree terminal — types it into the terminal's input and runs it. Mutating, so it " +
			"always confirms. Typed wrapper around the Daintree terminal.sendCommand MCP tool.",
		Consequence: "Runs a shell command in the named terminal as if you typed it. Effects depend on the command and may not be reversible.",
		Risk:        domain.RiskTerminal,
		Schema:      sendCommandSchema,
		Decode:      tools.StrictDecoder(func() any { return &sendCommandArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a sendCommandArgs
			_ = json.Unmarshal(raw, &a)
			// The schema trims+requires non-empty; reject whitespace-only here so a
			// blank command never types a bare newline into the terminal.
			if strings.TrimSpace(a.TerminalID) == "" || strings.TrimSpace(a.Command) == "" {
				return tools.Fail(domain.CodeValidation, "terminal.sendCommand: terminalId and command must be non-empty.")
			}
			return terminalSendCommandPassthrough(ctx, deps.MCP, a.TerminalID, a.Command,
				map[string]any{"terminalId": a.TerminalID, "command": a.Command})
		},
	}
}

/* ------------------------------- terminal.close --------------------------- */

type terminalCloseArgs struct {
	TerminalID  string   `json:"terminalId,omitempty"`
	TerminalIDs []string `json:"terminalIds,omitempty"`
}

// ids merges the singular terminalId and the plural terminalIds into one
// whitespace-trimmed, de-duplicated, order-preserving list. Accepting BOTH shapes
// keeps the tool forgiving: the model reaches for `terminalId` by analogy with
// every other terminal.* wrapper, but `terminalIds` is the whole point of this one
// — retiring a spawned cohort in ONE confirmed call rather than N.
func (a terminalCloseArgs) ids() []string {
	seen := map[string]bool{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	add(a.TerminalID)
	for _, id := range a.TerminalIDs {
		add(id)
	}
	return out
}

// Validate runs at DECODE time (StrictDecoder honours the Validator seam) — i.e.
// BEFORE the confirmation prompt in dispatch — so an empty/blank call (`{}`,
// `{"terminalIds":[]}`, whitespace-only ids) is rejected as invalid args up front
// instead of prompting the user to confirm a close that then fails. The handler
// keeps the same guard as defense-in-depth for any path that skips Decode.
func (a terminalCloseArgs) Validate() error {
	if len(a.ids()) == 0 {
		return errors.New("provide terminalId (a single id) or terminalIds (a list); at least one non-empty terminal id is required")
	}
	return nil
}

var terminalCloseSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "A single Daintree terminal id to close." },
    "terminalIds": { "type": "array", "items": { "type": "string" }, "description": "Several terminal ids to close in ONE confirmed call (e.g. retiring a spawned cohort) — prefer this over calling terminal.close once per id." }
  },
  "required": []
}`)

func newTerminalCloseTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.close",
		Description: "Close Daintree terminal(s) — moves each to the trash and ends the agent or process running in it. ONLY call this when the USER has " +
			"explicitly asked you to close a terminal or terminals; closing is never your own decision. Do NOT call it to tidy up, to retire agents you " +
			"spawned, or to recover from a failed/ambiguous/'no terminalId' spawn — a spawn that returned no terminalId is retried or reported, never " +
			"'fixed' by closing terminals. A terminal you did not just spawn yourself may be the user's own live work or another agent mid-task " +
			"(agentState 'working'/'waiting'), so closing it ends that work irreversibly. Pass a single terminalId, or terminalIds:[...] to close a whole " +
			"cohort the user asked you to retire in ONE call. Typed wrapper around the Daintree terminal.close MCP tool. (terminal.kill, reachable via " +
			"daintree.call, deletes PERMANENTLY instead of trashing — prefer close.)",
		Consequence: "Closes the named Daintree terminal(s) — only ever at the user's explicit request — moving them to the trash and ending whatever agent or process is running in each (irreversible for in-flight work).",
		Risk:        domain.RiskTerminal,
		Schema:      terminalCloseSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalCloseArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalCloseArgs
			_ = json.Unmarshal(raw, &a)
			ids := a.ids()
			if len(ids) == 0 {
				return tools.Fail(domain.CodeValidation,
					"terminal.close: provide terminalId (a single id) or terminalIds (a list); at least one non-empty terminal id is required.")
			}
			return terminalClosePassthrough(ctx, deps.MCP, ids)
		},
	}
}

/* --------------------- terminal.arm / disarm / disarmAll ------------------ */

type terminalArmingArgs struct {
	TerminalID string `json:"terminalId"`
}

var terminalArmingSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "terminalId": { "type": "string", "description": "Terminal to add to / remove from the fleet arming set." }
  },
  "required": ["terminalId"]
}`)

func newTerminalArmTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.arm",
		Description: "Add a terminal to Daintree's fleet arming set so the human's next broadcast keystrokes are ALSO routed to it. " +
			"Mutating (reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.arm MCP tool. " +
			"Reports the resulting armed set so arming is never silent.",
		Consequence: "Arms a terminal: the human's next broadcast input is also typed into it, and keeps being routed to every armed terminal until disarmed.",
		Risk:        domain.RiskTerminal,
		Schema:      terminalArmingSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalArmingArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalArmingArgs
			_ = json.Unmarshal(raw, &a)
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.arm",
				map[string]any{"terminalId": a.TerminalID}, "Armed terminal "+a.TerminalID+".")
		},
	}
}

func newTerminalDisarmTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.disarm",
		Description: "Remove a terminal from Daintree's fleet arming set so it no longer receives the human's broadcast input. " +
			"Mutating (reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.disarm MCP tool. " +
			"Reports the resulting armed set so the change is never silent.",
		Consequence: "Disarms a terminal: it stops receiving the human's broadcast input. Changes where keystrokes are routed.",
		Risk:        domain.RiskTerminal,
		Schema:      terminalArmingSchema,
		Decode:      tools.StrictDecoder(func() any { return &terminalArmingArgs{} }),
		Handle: func(ctx context.Context, raw json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			var a terminalArmingArgs
			_ = json.Unmarshal(raw, &a)
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.disarm",
				map[string]any{"terminalId": a.TerminalID}, "Disarmed terminal "+a.TerminalID+".")
		},
	}
}

func newTerminalDisarmAllTool(deps Deps) tools.Tool {
	return tools.Tool{
		Name: "terminal.disarmAll",
		Description: "Clear Daintree's entire fleet arming set so no terminal receives the human's broadcast input. Mutating " +
			"(reroutes the human's input), so it always confirms. Typed wrapper around the Daintree terminal.disarmAll MCP tool. " +
			"Reports the resulting (empty) armed set so the change is never silent.",
		Consequence: "Disarms every terminal at once: the human's broadcast input stops being routed anywhere. Changes where keystrokes are routed.",
		Risk:        domain.RiskTerminal,
		Schema:      noArgs,
		Decode:      tools.StrictDecoder(func() any { return &struct{}{} }),
		Handle: func(ctx context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return terminalArmingPassthrough(ctx, deps.MCP, "terminal.disarmAll",
				map[string]any{}, "Cleared the fleet arming set.")
		},
	}
}

/* ------------------------------- agent.focus* ----------------------------- */

// focusTool builds a no-arg UI focus wrapper that forwards to a same-named MCP
// action. All four agent-focus tools share this exact shape.
func focusTool(deps Deps, name, description, mcpName string) tools.Tool {
	return tools.Tool{
		Name:        name,
		Description: description,
		Risk:        domain.RiskUI,
		Schema:      noArgs,
		Decode:      tools.StrictDecoder(func() any { return &struct{}{} }),
		Handle: func(ctx context.Context, _ json.RawMessage, _ *tools.ToolContext) tools.ToolResult {
			return passthrough(ctx, deps.MCP, mcpName, map[string]any{}, "")
		},
	}
}

func newFocusNextWaitingTool(deps Deps) tools.Tool {
	return focusTool(deps, "agent.focusNextWaiting",
		"Focus the next agent terminal that is waiting for input or a decision (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextWaiting MCP tool.",
		"agent.focusNextWaiting")
}

func newFocusNextWorkingTool(deps Deps) tools.Tool {
	return focusTool(deps, "agent.focusNextWorking",
		"Focus the next agent terminal that is actively working (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextWorking MCP tool.",
		"agent.focusNextWorking")
}

func newFocusNextAgentTool(deps Deps) tools.Tool {
	return focusTool(deps, "agent.focusNextAgent",
		"Focus the next agent terminal in order (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusNextAgent MCP tool.",
		"agent.focusNextAgent")
}

func newFocusPreviousAgentTool(deps Deps) tools.Tool {
	return focusTool(deps, "agent.focusPreviousAgent",
		"Focus the previous agent terminal in order (UI focus change; no state mutation). Typed wrapper around the Daintree agent.focusPreviousAgent MCP tool.",
		"agent.focusPreviousAgent")
}
