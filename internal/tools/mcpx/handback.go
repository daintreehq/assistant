package mcpx

import (
	"context"

	"github.com/daintreehq/assistant/internal/tools"
	"github.com/daintreehq/assistant/internal/tools/handback"
)

// sendRequestsHandback decides whether ONE terminal.sendCommand should ask Daintree
// for a handback. Three gates, cheapest first, and any doubt is "no" — which leaves
// the call byte-identical to the one this wrapper has always made:
//
//  1. the feature switch is on;
//  2. the target is KNOWN to hold a live agent. A follow-up prompt to an agent is the
//     case this exists for; a shell command can hand nothing back, and Daintree
//     refuses the flag on a pane with no agent. Answered from state already in hand —
//     this sits in the send path, so it may not cost a terminal.list;
//  3. the connected host ADVERTISES the argument on terminal.sendCommand. Read per
//     call from the cache-first catalog rather than remembered here: the catalog is
//     rebuilt on reconnect, and a second copy of "supported" would outlive the host
//     it described.
//
// The id is matched EXACTLY as it will be sent — no trimming. Daintree matches ids
// exactly, so an id it would accept is the same one the roster carries; a padded or
// truncated id the host rejects anyway simply finds no agent here, and goes out as the
// legacy call.
func sendRequestsHandback(ctx context.Context, deps Deps, terminalID string) bool {
	if !deps.AgentHandback || deps.IsAgentTerminal == nil || deps.MCP == nil {
		return false
	}
	if !deps.IsAgentTerminal(terminalID) {
		return false
	}
	lctx, done := handback.LookupContext(ctx)
	infos, err := deps.MCP.ListTools(lctx, false)
	done()
	if err != nil {
		return false
	}
	for _, info := range infos {
		if info.Name == "terminal.sendCommand" {
			return handback.SchemaAccepts(info.InputSchema, info.InputSchemaProvided)
		}
	}
	return false
}

// withHandback returns a copy of args carrying the flag, leaving the original — the
// exact legacy call — intact for the refusal fallback.
func withHandback(args map[string]any) map[string]any {
	out := make(map[string]any, len(args)+1)
	for k, v := range args {
		out[k] = v
	}
	out[handback.Arg] = true
	return out
}

// handbackRefused reports whether a flagged send came back as Daintree refusing the
// FLAG: a tool-level error result (on this path only hostRefusal attaches rawText)
// that handback.IsRefusal recognises as one of the host's pre-dispatch refusals. Those
// are raised before any text is submitted, so repeating the call without the flag does
// not double-submit. A transport failure is ambiguous, has no rawText, and is never
// retried; neither is a rejection that merely mentions the word.
func handbackRefused(res tools.ToolResult) bool {
	if res.Ok || res.Error == nil {
		return false
	}
	details, ok := res.Error.Details.(map[string]any)
	if !ok {
		return false
	}
	raw, ok := details["rawText"].(string)
	return ok && handback.IsRefusal(true, raw)
}
