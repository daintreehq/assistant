package app

import (
	"context"

	"github.com/daintreehq/assistant/internal/tools/handback"
)

// isLiveAgentTerminal answers "does this terminal hold a live agent" from the
// Session's cached roster — no MCP call, because it runs inside a send. The Session is
// read at CALL time, not captured: the tool table is built during App.Create before
// a.Session exists, and a nil Session (or a cold roster) is simply "not known", which
// leaves the flag off.
func (a *App) isLiveAgentTerminal(terminalID string) bool {
	if a == nil || a.Session == nil {
		return false
	}
	return a.Session.IsLiveAgentTerminal(terminalID)
}

// sendRequestsHandback is the three-gate handback decision for a terminal.sendCommand
// issued from THIS package (terminal.run.async's sender): switch on, target known to
// hold a live agent, and the connected host ADVERTISING the argument. It mirrors
// mcpx.sendRequestsHandback, which makes the same decision for the typed wrapper
// against its own consumer interface; the rule itself lives once, in
// internal/tools/handback. Any doubt is false — the pre-feature call.
func (a *App) sendRequestsHandback(ctx context.Context, terminalID string) bool {
	if a == nil || !a.Config.AgentHandback || a.MCP == nil {
		return false
	}
	if !a.isLiveAgentTerminal(terminalID) {
		return false
	}
	infos, err := a.MCP.ListTools(ctx, false)
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
