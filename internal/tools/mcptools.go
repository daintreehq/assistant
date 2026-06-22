package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/daintreehq/daintree-assistant/internal/domain"
	"github.com/daintreehq/daintree-assistant/internal/safety"
)

// daintree.call-specific error codes (model-facing — §3.2, §7).
const (
	codeUseTypedWrapper = "USE_TYPED_WRAPPER"
	codeMCPUnavailable  = "MCP_UNAVAILABLE"
	codeMCPToolError    = "MCP_TOOL_ERROR"
	codeCancelled       = "CANCELLED"
)

// wrappedMCPTools is the daintree.call DENYLIST: raw MCP tool name → the typed
// wrapper the model must use instead. The escape hatch invites two failure modes
// — reaching for it when a wrapper exists, then sending arguments:{} and retrying
// the identical broken call — so we redirect to the typed, validated wrapper.
// Keep in sync with the wrappers. Verbatim 11 entries. Spec: §7.
var wrappedMCPTools = map[string]string{
	"agent.launch":                 `agentTask.spawnForEdits (mode "explore"/"edit")`,
	"terminal.getOutput":           "terminal.read / terminal.summarize / terminal.extract",
	"panel.focus":                  "terminal.focus",
	"terminal.sendCommand":         "terminal.sendCommand (typed wrapper)",
	"terminal.arm":                 "terminal.arm",
	"terminal.disarm":              "terminal.disarm",
	"terminal.disarmAll":           "terminal.disarmAll",
	"copyTree.injectToTerminal":    "copyTree.injectToTerminal",
	"copyTree.generateAndCopyFile": "copyTree.generateAndCopyFile",
	"git.snapshotRevert":           "git.snapshotRevert",
	"git.snapshotDelete":           "git.snapshotDelete",
}

// callArgs is the daintree.call argument shape: { name, arguments?, requestKey? }.
type callArgs struct {
	Name       string         `json:"name"`
	Arguments  map[string]any `json:"arguments,omitempty"`
	RequestKey string         `json:"requestKey,omitempty"`
}

// daintreeCallSchema is the raw JSON Schema (additionalProperties:false,
// required:["name"], arguments is an open object). Spec: §7.
var daintreeCallSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {
    "name": { "type": "string" },
    "arguments": { "type": "object", "additionalProperties": true },
    "requestKey": { "type": "string" }
  }
}`)

// NewDaintreeCallTool builds the raw-passthrough daintree.call tool: risk system,
// always confirmed, requires the system tier. Its handler re-applies the
// typed-wrapper denylist and the no-file-edit guard before forwarding. Spec: §7.
func NewDaintreeCallTool() *Tool {
	return &Tool{
		Name: "daintree.call",
		Description: "Raw passthrough to any Daintree MCP tool. Prefer a typed wrapper " +
			"when one exists; this is the unvalidated escape hatch.",
		Risk:        domain.RiskSystem,
		Consequence: "Invokes an arbitrary Daintree MCP tool with raw, unvalidated arguments.",
		Schema:      daintreeCallSchema,
		Decode:      StrictDecoder(func() any { return &callArgs{} }),
		Handle:      handleDaintreeCall,
	}
}

func handleDaintreeCall(ctx context.Context, args json.RawMessage, tctx *ToolContext) ToolResult {
	var a callArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return Fail(codeInvalidArgs, "Invalid arguments for daintree.call: "+err.Error())
	}

	// 1. Typed-wrapper denylist (each guard returns before the actual call).
	if wrapper, found := wrappedMCPTools[a.Name]; found {
		return Fail(codeUseTypedWrapper, fmt.Sprintf(
			"Do not call %s through daintree.call — use the typed wrapper instead: %s. "+
				"It takes named, validated parameters, so you can't drop a required argument. "+
				"Switch tools; do not retry this raw call.", a.Name, wrapper))
	}

	// 2. No-file-edit re-check on the RAW forwarded name (the registration-time
	//    guard only covers local names; this is the runtime escape-hatch re-check).
	if safety.IsForbiddenToolName(a.Name) {
		return Fail(safety.FileEditForbiddenCode, fmt.Sprintf(
			"Refusing to call %s via daintree.call — the assistant never edits files directly. "+
				"Spawn a visible agent (agentTask.spawnForEdits) to make changes.", a.Name),
			Unrecoverable())
	}

	// 3. Connectivity.
	if tctx.MCP == nil || !tctx.MCP.Connected() {
		return Fail(codeMCPUnavailable, fmt.Sprintf(
			"Daintree MCP is not connected; cannot call %s.", a.Name))
	}

	// 4. Forward. Merge arguments + an optional requestKey.
	callArgsMap := make(map[string]any, len(a.Arguments)+1)
	for k, v := range a.Arguments {
		callArgsMap[k] = v
	}
	if a.RequestKey != "" {
		callArgsMap["requestKey"] = a.RequestKey
	}

	res, err := tctx.MCP.CallTool(ctx, a.Name, callArgsMap)
	if err != nil {
		// On throw: a cancelled context maps to CANCELLED; otherwise MCP_TOOL_ERROR.
		if ctx.Err() != nil {
			return Fail(codeCancelled,
				fmt.Sprintf("Turn cancelled during %s.", a.Name), Unrecoverable())
		}
		return Fail(codeMCPToolError,
			fmt.Sprintf("Daintree MCP call %s failed: %s", a.Name, err.Error()))
	}
	if res.IsError {
		msg := res.Text
		if msg == "" {
			msg = fmt.Sprintf("Daintree MCP tool %s returned an error.", a.Name)
		}
		return Fail(codeMCPToolError, msg,
			WithDetails(map[string]any{"structuredContent": res.StructuredContent}))
	}
	return Ok(fmt.Sprintf("Called %s.", a.Name), map[string]any{
		"text":              res.Text,
		"structuredContent": res.StructuredContent,
		"isError":           res.IsError,
	})
}
