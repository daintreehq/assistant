package mcpx

import (
	"context"
	"strings"

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
// The id is matched as given. Daintree matches ids exactly, so an id it would accept
// is the same one the roster carries; a truncated id the host rejects anyway simply
// finds no agent here.
func sendRequestsHandback(ctx context.Context, deps Deps, terminalID string) bool {
	if !deps.AgentHandback || deps.IsAgentTerminal == nil || deps.MCP == nil {
		return false
	}
	if !deps.IsAgentTerminal(strings.TrimSpace(terminalID)) {
		return false
	}
	infos, err := deps.MCP.ListTools(ctx, false)
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
// FLAG: a tool-level error result (hostRefusal, which alone carries rawText) whose
// text names the argument. That is a definitive "nothing was sent", so repeating the
// call without the flag cannot double-submit. A transport failure is ambiguous, has no
// rawText, and is never retried.
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
