// Package handback holds the one rule every call site that prompts an agent shares:
// when may `handback: true` ride an outgoing Daintree MCP call.
//
// A handback is Daintree's "the agent says it is done" signal. The caller passes the
// flag WITH a prompt; Daintree appends the instruction server-side (it carries a code
// only Daintree knows) and reports the observed reply as `lastHandback`. So this
// process sends a flag and never text — nothing here may describe the instruction, in
// a prompt or in a tool description, or the agent sees two instructions and only one
// of them carries the code.
//
// The flag is a wire argument on someone else's schema, and this CLI meets hosts that
// predate it. Several of Daintree's action schemas are strict, so an unknown argument
// can fail a launch outright. Support is therefore read from the tool's ADVERTISED
// input schema — MCP negotiates nothing at the parameter level, so that schema is the
// only honest source — and every doubt (no schema, a malformed one, a catalog read
// that failed) resolves to "leave it out", which is byte-for-byte the pre-feature call.
//
// It imports nothing from the tool families so that agenttaskx, mcpx and the app-level
// async sender can all share it without importing one another.
package handback

import "strings"

// Arg is the wire argument name on agent.launch and terminal.sendCommand.
const Arg = "handback"

// SchemaAccepts reports whether a tool's advertised input schema declares Arg as a
// TOP-LEVEL property. provided must be the transport's "the server advertised this
// schema" bit: the client substitutes an accept-anything empty object when a server
// publishes none, and reading that stand-in as support would send the argument to
// exactly the hosts that cannot be asked.
//
// A raw map lookup, deliberately — compiling the schema to ask about one key buys
// nothing, and a nested `handback` elsewhere in the document is not the argument.
func SchemaAccepts(inputSchema map[string]any, provided bool) bool {
	if !provided || inputSchema == nil {
		return false
	}
	props, ok := inputSchema["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = props[Arg]
	return ok
}

// IsRefusal reports whether a tool-level error RESULT is Daintree refusing the flag
// itself — it validates the target before dispatching anything, and rejects
// `handback: true` on a pane with no agent running. The model cannot act on that
// refusal (it has no such argument to drop), so the sender repeats the call once
// without the flag. Only ever consulted for an error RESULT, which means the server
// saw and rejected the call; a transport error is ambiguous and is never re-sent.
//
// Matching the argument's own name in the message is the narrowest signal available:
// the refusal has no stable code on the wire, and a host that rewords it while still
// naming the argument keeps working.
func IsRefusal(isError bool, text string) bool {
	return isError && strings.Contains(strings.ToLower(text), Arg)
}
