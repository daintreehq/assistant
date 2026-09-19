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

import (
	"context"
	"strings"
	"time"
)

// Arg is the wire argument name on agent.launch and terminal.sendCommand.
const Arg = "handback"

// SchemaAccepts reports whether a tool's advertised input schema declares Arg as a
// TOP-LEVEL boolean property. provided must be the transport's "the server advertised
// this schema" bit: the client substitutes an accept-anything empty object when a
// server publishes none, and reading that stand-in as support would send the argument
// to exactly the hosts that cannot be asked.
//
// A raw map lookup, deliberately — compiling the schema to ask about one key buys
// nothing, and a nested `handback` elsewhere in the document is not the argument. The
// declaration itself is still read: `"handback": false` FORBIDS the key in JSON Schema,
// and a declared non-boolean type would reject the `true` this process sends, so only
// an object subschema that is untyped or typed boolean counts as support.
func SchemaAccepts(inputSchema map[string]any, provided bool) bool {
	if !provided || inputSchema == nil {
		return false
	}
	props, ok := inputSchema["properties"].(map[string]any)
	if !ok {
		return false
	}
	decl, ok := props[Arg].(map[string]any)
	if !ok {
		return false
	}
	typ, typed := decl["type"]
	return !typed || typ == "boolean"
}

// lookupBudget bounds one capability lookup. The catalog is cache-first, so this only
// bites on a COLD cache against an unresponsive host — where the alternative is an
// optional flag holding a spawn or a send hostage, since ListTools has no timeout of
// its own and a turn's context has no deadline.
const lookupBudget = 2 * time.Second

// LookupContext returns a child context for one capability lookup, bounded by a
// CANCEL (time.AfterFunc), never a deadline: mcp.Client degrades the connection on a
// DeadlineExceeded and treats Canceled as a caller abort (the mcp-bestEffort rule). On
// expiry the lookup fails, the flag is omitted, and the caller carries on with its own
// context untouched.
func LookupContext(ctx context.Context) (context.Context, func()) {
	cctx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(lookupBudget, cancel)
	return cctx, func() {
		timer.Stop()
		cancel()
	}
}

// IsRefusal reports whether a tool-level error RESULT is Daintree refusing the FLAG
// itself. Daintree validates the target before dispatching anything and rejects
// `handback: true` on a pane with no agent running; a strict schema rejects the key
// outright. Both happen before any text is submitted, which is what makes repeating
// the call without the flag safe. The model cannot act on either (it has no such
// argument to drop), so the sender repeats the call once without it.
//
// Matched on those two refusals' own wording, NOT on the bare argument name: a
// terminal called "handback-worker" that has gone missing, or an error echoing a
// command that mentions the word, is an unrelated failure that may have come after
// dispatch, and must be reported rather than re-sent. A host that rewords its refusal
// degrades to surfacing the error — never to a blind resend. Only ever consulted for an
// error RESULT; a transport error is ambiguous and is never re-sent.
func IsRefusal(isError bool, text string) bool {
	if !isError {
		return false
	}
	t := strings.ToLower(text)
	if strings.Contains(t, "handback needs an agent") {
		return true
	}
	return strings.Contains(t, "unrecognized key") && strings.Contains(t, Arg)
}
