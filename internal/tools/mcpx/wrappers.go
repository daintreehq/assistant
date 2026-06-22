package mcpx

import (
	"context"
	"encoding/json"
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

// copyTreeForwardMap rebuilds the verbatim args map the TS passes (it forwards the
// whole parsed args object, so worktreeId/options travel as-is; omitted fields are
// simply absent).
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
			m := map[string]any{"terminalId": a.TerminalID}
			if a.WorktreeID != "" {
				m["worktreeId"] = a.WorktreeID
			}
			if a.Options != nil {
				m["options"] = a.Options
			}
			return passthrough(ctx, deps.MCP, "copyTree.injectToTerminal", m, "")
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
			return passthrough(ctx, deps.MCP, "terminal.sendCommand",
				map[string]any{"terminalId": a.TerminalID, "command": a.Command}, "")
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
